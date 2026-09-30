package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Anthropic Messages API (/v1/messages) -> 上游 Chat Completions 转译
//
// 网关只与上游 /v2/chat/completions 通信，此处负责双向协议转换：
//   - 请求：system / messages(content blocks) / tools(input_schema) / thinking
//     -> chat messages / tools(嵌套 function) / reasoning_effort
//   - 流式响应：上游 SSE 增量 -> Anthropic 语义事件（message_start、
//     content_block_start/delta/stop、message_delta、message_stop）
//   - 非流式响应：上游流式数据本地聚合为完整 Message 对象
//
// stop_reason 映射：stop->end_turn、tool_calls->tool_use、length->max_tokens。
// 上游思维链（delta.reasoning_content）映射为 thinking 内容块。
// -----------------------------------------------------------------------------

// anthropicErrFmtKey 标记本次请求的错误响应应使用 Anthropic 形状
// （{"type":"error","error":{...}}）而不是 OpenAI 形状。
type anthropicErrFmtKey struct{}

func withAnthropicErrFormat(ctx context.Context) context.Context {
	return context.WithValue(ctx, anthropicErrFmtKey{}, true)
}

// writeUpstreamError 是 upstreamChat / 鉴权中间件的统一错误出口：
// 请求来自 /v1/messages 时回 Anthropic 形状，否则回 OpenAI 形状。
func writeUpstreamError(w http.ResponseWriter, r *http.Request, statusCode int, errType, message string) {
	if r.Context().Value(anthropicErrFmtKey{}) == true {
		writeAnthropicError(w, statusCode, anthropicErrType(errType), message)
		return
	}
	writeOpenAIError(w, statusCode, errType, message)
}

func writeAnthropicError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

// anthropicErrType 把内部错误类型翻译成 Anthropic 官方错误类型枚举。
func anthropicErrType(errType string) string {
	switch errType {
	case "no_auth", "invalid_api_key":
		return "authentication_error"
	case "model_disabled", "model_account_disabled":
		return "permission_error"
	case "model_rate_limited", "all_accounts_cooldown":
		return "rate_limit_error"
	case "no_available_account", "model_requires_quota":
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// handleMessages 处理 POST /v1/messages（Anthropic Messages API）。
func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "api_error", "仅支持 POST 请求")
		return
	}
	// 标记错误出口走 Anthropic 形状（含 upstreamChat 内部的账号调度错误）。
	r = r.WithContext(withAnthropicErrFormat(r.Context()))

	reqID := requestIDFor(r)
	startTime := requestStartFor(r)

	readStarted := debugBodyReadStarted(r)
	bodyBytes, err := io.ReadAll(r.Body)
	readDuration := debugElapsedSince(readStarted)
	if err != nil {
		debugBodyReadFailed(r, bodyBytes, readStarted, err)
		writeAnthropicError(w, http.StatusBadRequest, "api_error", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	var msgReq map[string]any
	decodeStarted := time.Now()
	decodeErr := json.Unmarshal(bodyBytes, &msgReq)
	decodeDuration := time.Since(decodeStarted)
	if decodeErr != nil {
		debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, false, decodeErr)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "无效的 JSON 请求体")
		return
	}

	modelName, _ := msgReq["model"].(string)
	if modelName == "" {
		modelName = "hy4-preview"
	}
	isStream, _ := msgReq["stream"].(bool)
	debugSetModelAndStream(r, modelName, isStream)
	debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, true, nil)

	// 模型黑白名单拦截：命中即拒绝，不消耗任何上游账号额度。
	if disabled, reason := modelDisabled(modelName); disabled {
		log.Printf("[请求被拒绝] traceId=%s requestId=%d 拦截层=模型黑白名单 模型=%s 结果=拒绝 原因=%s 返回状态码=403 业务影响=请求未进入上游调用",
			w.Header().Get("X-Trace-ID"), reqID, modelName, reason)
		debugEvent(r, "warn", "model_blocked_by_config", map[string]any{
			"status_code":     http.StatusForbidden,
			"reason":          reason,
			"business_impact": "模型被网关配置禁用，请求未进入上游调用",
		})
		writeAnthropicError(w, http.StatusForbidden, "permission_error", reason)
		return
	}

	chatReq, err := anthropicToChatRequest(msgReq, modelName)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatReq["stream"] = true // 上游强制流式，非流式由网关本地聚合

	applyThinkingRules(chatReq, modelName)
	sanitizeMessages(chatReq)
	ensureLeadingSystemMessage(chatReq)
	repairReport := repairToolMessageSequence(chatReq)
	logToolSequenceRepair(r, w.Header().Get("X-Trace-ID"), reqID, modelName, repairReport)
	// 与 Chat 原生入口共用同一套 DeepSeek 多轮推理历史回填规则。
	logReasoningHistoryRepair(r, reqID, modelName, repairReasoningHistory(chatReq))

	upstreamBytes, err := json.Marshal(chatReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "序列化请求失败")
		return
	}

	log.Printf("[#%d] POST /v1/messages -> Upstream [Model: %s, Stream: %v]", reqID, modelName, isStream)

	resp, acc, prof, ok := upstreamChat(w, r, reqID, modelName, upstreamBytes, startTime)
	if !ok {
		return
	}
	if isStream {
		streamMessagesResponse(w, r, resp, modelName, reqID, acc, prof, startTime)
	} else {
		writeMessagesAggregate(w, r, resp, modelName, reqID, acc, prof, startTime)
	}
}

// -----------------------------------------------------------------------------
// 请求转换：Anthropic Messages -> Chat Completions
// -----------------------------------------------------------------------------

// anthropicToChatRequest 将 Anthropic Messages 请求体转换为上游 Chat 请求体。
func anthropicToChatRequest(body map[string]any, modelName string) (map[string]any, error) {
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if sys := extractAnthropicSystemText(body["system"]); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			messages = append(messages, convertAnthropicMessage(msg)...)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages 字段缺失或为空")
	}
	chat["messages"] = messages

	if v, ok := body["max_tokens"].(float64); ok && v > 0 {
		chat["max_tokens"] = int(v)
	}
	if v, ok := body["temperature"].(float64); ok {
		chat["temperature"] = v
	}
	if v, ok := body["top_p"].(float64); ok {
		chat["top_p"] = v
	}
	if v, ok := body["top_k"].(float64); ok {
		chat["top_k"] = v
	}
	if ss, ok := body["stop_sequences"].([]any); ok && len(ss) > 0 {
		chat["stop"] = ss
	}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if ct := convertAnthropicTools(tools); len(ct) > 0 {
			chat["tools"] = ct
		}
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		chat["tool_choice"] = convertAnthropicToolChoice(tc)
	} else if tc, ok := body["tool_choice"].(string); ok {
		chat["tool_choice"] = tc
	}

	// thinking -> reasoning_effort（Anthropic 用 budget_tokens 表达预算，上游只认
	// 扁平的 reasoning_effort；enabled 无显式档位时取 high，disabled 显式关闭——
	// applyThinkingRules 会对 "off" 做删除处理并跳过 reasoning_summary 注入）。
	if th, ok := body["thinking"].(map[string]any); ok {
		ttype, _ := th["type"].(string)
		switch strings.ToLower(strings.TrimSpace(ttype)) {
		case "disabled":
			chat["reasoning_effort"] = "off"
		case "enabled":
			effort := strings.TrimSpace(strOf(th["effort"]))
			if effort == "" {
				effort = "high"
			}
			chat["reasoning_effort"] = effort
		}
	}
	return chat, nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// extractAnthropicSystemText 提取 system 字段为纯文本。支持 string 与
// [{type:"text",text:...}] 数组（忽略 cache_control 等附加字段）。
func extractAnthropicSystemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		parts := make([]string, 0, len(s))
		for _, b := range s {
			blk, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if blk["type"] == "text" {
				if t, ok := blk["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// convertAnthropicMessage 将单条 Anthropic 消息转换为 0..n 条 chat 消息。
func convertAnthropicMessage(msg map[string]any) []any {
	role := strOf(msg["role"])
	switch content := msg["content"].(type) {
	case string:
		return []any{map[string]any{"role": role, "content": content}}
	case []any:
		return convertAnthropicBlocks(role, content)
	case nil:
		return []any{map[string]any{"role": role, "content": ""}}
	}
	return nil
}

// convertAnthropicBlocks 处理 content blocks 数组。
// user：tool_result 块 -> 独立 tool 消息（必须紧跟 assistant 的 tool_calls），
//
//	其余文本合并为一条 user 消息放在 tool 消息之后。
//
// assistant：text 块合并为 content，tool_use 块 -> tool_calls；thinking /
//
//	redacted_thinking 历史块不上传（上游自行管理思维链）。
func convertAnthropicBlocks(role string, blocks []any) []any {
	switch role {
	case "user":
		// tool_result -> 独立 tool 消息（须紧跟 assistant 的 tool_calls）；
		// text/image 合并为一条 user 消息（有图时 content 用 parts 数组）。
		var toolMsgs []any
		var parts []any
		var texts []string
		for _, bAny := range blocks {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				if t, ok := blk["text"].(string); ok && t != "" {
					texts = append(texts, t)
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "image":
				if url := anthropicImageURL(blk); url != "" {
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": url},
					})
				}
			case "tool_result":
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": strOf(blk["tool_use_id"]),
					"content":      anthropicContentText(blk["content"]),
				})
			}
		}
		var out []any
		out = append(out, toolMsgs...)
		if len(parts) > 0 {
			content := any(strings.Join(texts, ""))
			if len(parts) != len(texts) {
				content = parts // 含图片块，保留 parts 数组
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		}
		return out
	case "assistant":
		var texts []string
		var toolCalls []any
		for _, bAny := range blocks {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				if t, ok := blk["text"].(string); ok {
					texts = append(texts, t)
				}
			case "tool_use":
				args, err := json.Marshal(blk["input"])
				if err != nil || blk["input"] == nil {
					args = []byte("{}")
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   strOf(blk["id"]),
					"type": "function",
					"function": map[string]any{
						"name":      strOf(blk["name"]),
						"arguments": string(args),
					},
				})
			}
		}
		m := map[string]any{"role": "assistant", "content": strings.Join(texts, "")}
		if len(toolCalls) > 0 {
			m["tool_calls"] = toolCalls
		}
		return []any{m}
	}
	// 其他 role：文本兜底
	var texts []string
	for _, bAny := range blocks {
		if blk, ok := bAny.(map[string]any); ok && blk["type"] == "text" {
			if t, ok := blk["text"].(string); ok && t != "" {
				texts = append(texts, t)
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return []any{map[string]any{"role": role, "content": strings.Join(texts, "")}}
}

// anthropicContentText 提取 tool_result.content（string 或块数组）为纯文本。
func anthropicContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, b := range c {
			if blk, ok := b.(map[string]any); ok && blk["type"] == "text" {
				if t, ok := blk["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// anthropicImageURL 把 Anthropic image 块转为 data URL（base64 source）或原 URL。
// 上游不支持的形态返回空串，调用方静默丢弃。
func anthropicImageURL(blk map[string]any) string {
	src, _ := blk["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch strOf(src["type"]) {
	case "base64":
		media := strOf(src["media_type"])
		data := strOf(src["data"])
		if media == "" || data == "" {
			return ""
		}
		return "data:" + media + ";base64," + data
	case "url":
		return strOf(src["url"])
	}
	return ""
}

// convertAnthropicTools 将 Anthropic tools（input_schema）转为 chat 格式。
func convertAnthropicTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, tAny := range tools {
		t, ok := tAny.(map[string]any)
		if !ok {
			continue
		}
		if _, hasFn := t["function"]; hasFn {
			out = append(out, t) // 已是 chat 格式，透传
			continue
		}
		fn := map[string]any{"name": strOf(t["name"])}
		if d, ok := t["description"].(string); ok && d != "" {
			fn["description"] = d
		}
		if p, ok := t["input_schema"]; ok && p != nil {
			fn["parameters"] = p
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// convertAnthropicToolChoice 将 Anthropic tool_choice 转为 chat 格式。
func convertAnthropicToolChoice(tc map[string]any) any {
	typ := strings.ToLower(strOf(tc["type"]))
	name := strOf(tc["name"])
	switch typ {
	case "none":
		return "none"
	case "any", "required":
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		return "required"
	case "tool":
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		return "auto"
	default: // auto
		return "auto"
	}
}

// -----------------------------------------------------------------------------
// 流式回译：上游 Chat SSE -> Anthropic Messages SSE
// -----------------------------------------------------------------------------

type anthropicTextSlot struct {
	open  bool
	index int
	buf   strings.Builder
}

type anthropicToolSlot struct {
	id, name string
	args     strings.Builder
	index    int
	open     bool
}

// anthropicStreamState 把逐个到来的 chat.completion.chunk 翻译为 Anthropic
// SSE 事件字节流。同一实例也可走 aggregate() 输出非流式完整 Message。
type anthropicStreamState struct {
	msgID      string
	model      string
	started    bool
	blockIndex int
	thinking   *anthropicTextSlot
	text       *anthropicTextSlot
	tools      map[int]*anthropicToolSlot
	toolOrder  []int
	finish     string
	usage      map[string]any
}

func newAnthropicStreamState(modelName string) *anthropicStreamState {
	return &anthropicStreamState{
		msgID: "msg_" + compactUUID(),
		model: modelName,
		tools: map[int]*anthropicToolSlot{},
	}
}

// emit 格式化一个 Anthropic SSE 事件（event 行 + data 行 + 空行）。
func (s *anthropicStreamState) emit(eventType string, data map[string]any) []byte {
	data["type"] = eventType
	b, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return []byte("event: " + eventType + "\ndata: " + string(b) + "\n\n")
}

// feed 消费一个上游 chunk，返回 0..n 个 Anthropic 事件。
func (s *anthropicStreamState) feed(chunk map[string]any) []byte {
	var out []byte
	if m, ok := chunk["model"].(string); ok && m != "" {
		s.model = m
	}
	if !s.started {
		s.started = true
		out = append(out, s.emit("message_start", map[string]any{
			"message": map[string]any{
				"id":      s.msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"model":   s.model,
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})...)
	}
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		s.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, cAny := range choices {
		choice, ok := cAny.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta != nil {
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				out = append(out, s.appendThinking(rc)...)
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				out = append(out, s.appendText(ct)...)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tcAny := range tcs {
					tc, ok := tcAny.(map[string]any)
					if !ok {
						continue
					}
					out = append(out, s.appendToolCall(tc)...)
				}
			}
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			s.finish = fr
		}
	}
	return out
}

func (s *anthropicStreamState) stopTextLocked(out []byte) []byte {
	if s.text != nil && s.text.open {
		out = append(out, s.emit("content_block_stop", map[string]any{"index": s.text.index})...)
		s.text.open = false
	}
	return out
}

func (s *anthropicStreamState) stopThinkingLocked(out []byte) []byte {
	if s.thinking != nil && s.thinking.open {
		out = append(out, s.emit("content_block_stop", map[string]any{"index": s.thinking.index})...)
		s.thinking.open = false
	}
	return out
}

func (s *anthropicStreamState) appendThinking(rc string) []byte {
	var out []byte
	// 思维链只出现在正文前；若正文块开着，先收口再开新的 thinking 块。
	out = s.stopTextLocked(out)
	if s.thinking == nil || !s.thinking.open {
		s.thinking = &anthropicTextSlot{open: true, index: s.blockIndex}
		s.blockIndex++
		out = append(out, s.emit("content_block_start", map[string]any{
			"index":         s.thinking.index,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})...)
	}
	s.thinking.buf.WriteString(rc)
	out = append(out, s.emit("content_block_delta", map[string]any{
		"index": s.thinking.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
	})...)
	return out
}

func (s *anthropicStreamState) appendText(ct string) []byte {
	var out []byte
	// 正文开始：先收口 thinking 块，保证块顺序 thinking -> text。
	out = s.stopThinkingLocked(out)
	if s.text == nil || !s.text.open {
		s.text = &anthropicTextSlot{open: true, index: s.blockIndex}
		s.blockIndex++
		out = append(out, s.emit("content_block_start", map[string]any{
			"index":         s.text.index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})...)
	}
	s.text.buf.WriteString(ct)
	out = append(out, s.emit("content_block_delta", map[string]any{
		"index": s.text.index,
		"delta": map[string]any{"type": "text_delta", "text": ct},
	})...)
	return out
}

func (s *anthropicStreamState) appendToolCall(tc map[string]any) []byte {
	idx := 0
	if v, ok := tc["index"].(float64); ok {
		idx = int(v)
	}
	slot, exists := s.tools[idx]
	if !exists {
		slot = &anthropicToolSlot{index: s.blockIndex}
		s.blockIndex++
		s.tools[idx] = slot
		s.toolOrder = append(s.toolOrder, idx)
	}
	var out []byte
	// 工具块开始：先收口 thinking / text，保证块顺序 thinking -> text -> tool_use。
	out = s.stopThinkingLocked(out)
	out = s.stopTextLocked(out)
	if id, ok := tc["id"].(string); ok && id != "" {
		slot.id = id
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && n != "" {
			slot.name = n
		}
	}
	if !slot.open {
		slot.open = true
		out = append(out, s.emit("content_block_start", map[string]any{
			"index": slot.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": slot.id, "name": slot.name, "input": map[string]any{},
			},
		})...)
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if a, ok := fn["arguments"].(string); ok && a != "" {
			slot.args.WriteString(a)
			out = append(out, s.emit("content_block_delta", map[string]any{
				"index": slot.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": a},
			})...)
		}
	}
	return out
}

// mapStopReason 将 chat finish_reason 映射为 Anthropic stop_reason。
func mapStopReason(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func numToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func (s *anthropicStreamState) anthropicUsage() map[string]any {
	in, out := 0, 0
	if s.usage != nil {
		in = numToInt(s.usage["prompt_tokens"])
		out = numToInt(s.usage["completion_tokens"])
		if in == 0 && out == 0 {
			in = numToInt(s.usage["input_tokens"])
			out = numToInt(s.usage["output_tokens"])
		}
	}
	return map[string]any{"input_tokens": in, "output_tokens": out}
}

// finishEvents 关闭所有未收口块并输出 message_delta + message_stop。
func (s *anthropicStreamState) finishEvents() []byte {
	var out []byte
	out = s.stopThinkingLocked(out)
	out = s.stopTextLocked(out)
	for _, idx := range s.toolOrder {
		slot := s.tools[idx]
		if slot.open {
			out = append(out, s.emit("content_block_stop", map[string]any{"index": slot.index})...)
			slot.open = false
		}
	}
	out = append(out, s.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": mapStopReason(s.finish), "stop_sequence": nil},
		"usage": s.anthropicUsage(),
	})...)
	out = append(out, s.emit("message_stop", map[string]any{})...)
	return out
}

// aggregate 输出完整 Anthropic Message 对象（非流式响应）。
func (s *anthropicStreamState) aggregate() map[string]any {
	content := []any{}
	if s.thinking != nil && s.thinking.buf.Len() > 0 {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": s.thinking.buf.String(), "signature": "",
		})
	}
	if s.text != nil && s.text.buf.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": s.text.buf.String()})
	}
	for _, idx := range s.toolOrder {
		slot := s.tools[idx]
		var input any
		args := slot.args.String()
		if args == "" {
			input = map[string]any{}
		} else if err := json.Unmarshal([]byte(args), &input); err != nil {
			input = args // 参数不是合法 JSON 时原样返回，不让单次调用失败
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": slot.id, "name": slot.name, "input": input,
		})
	}
	return map[string]any{
		"id":            s.msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         s.model,
		"stop_reason":   mapStopReason(s.finish),
		"stop_sequence": nil,
		"usage":         s.anthropicUsage(),
	}
}

// -----------------------------------------------------------------------------
// 响应出口：流式 / 聚合 / count_tokens
// -----------------------------------------------------------------------------

// scanUpstreamSSE 逐行扫描上游 SSE 并把每个 chunk 喂给状态机。
// 返回 scanner 错误（nil 表示干净 EOF）。
func scanUpstreamSSE(body io.Reader, state *anthropicStreamState, sink func([]byte)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if sink != nil {
			if out := state.feed(chunk); len(out) > 0 {
				sink(out)
			}
		} else {
			state.feed(chunk)
		}
	}
	return scanner.Err()
}

// streamMessagesResponse 将上游流式响应实时回译为 Anthropic SSE。
func streamMessagesResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "服务器不支持流式响应 Flush")
		return
	}
	writeEvent := func(b []byte) {
		if len(b) == 0 {
			return
		}
		_, _ = w.Write(b)
		flusher.Flush()
	}

	state := newAnthropicStreamState(modelName)
	body := newTTFTReader(resp.Body, startTime)
	scanErr := scanUpstreamSSE(body, state, writeEvent)
	if scanErr != nil {
		debugEvent(r, "error", "stream_response_failed", map[string]any{
			"error_type": debugErrorType(scanErr),
			"error":      safeDebugError(scanErr),
		})
		// 上游流中断时不能伪造 message_stop：那会让客户端把残缺输出当成完整
		// 结果。改为下发 error 事件并直接返回，明确告知本次响应不完整。
		reason := "上游流式响应中断，本次回复不完整"
		if errors.Is(scanErr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("上游超过 %v 无数据，判定连接卡死并中断，本次回复不完整", upstreamIdleTimeout)
		}
		writeEvent(state.emit("error", map[string]any{
			"error": map[string]any{"type": "api_error", "message": reason},
		}))
		log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游流式读取 账号=%s 异常=%v 业务影响=本次响应不完整，已下发 error 事件而非伪造完成", debugTraceID(r), reqID, acc.Path, scanErr)
		recordModelTTFT(modelName, body.duration())
		recordModelLatency(modelName, time.Since(startTime))
		return
	}
	if state.finish == "" {
		// 干净 EOF 但没有 finish_reason：拒绝伪造 message_stop，下发 error 事件。
		debugEvent(r, "error", "stream_closed_without_finish", map[string]any{
			"error_type":      "stream_closed_without_finish",
			"finish_reason":   "",
			"business_impact": "上游干净结束但没有 finish_reason，已拒绝 message_stop，改为 error 事件",
		})
		writeEvent(state.emit("error", map[string]any{
			"error": map[string]any{"type": "api_error", "message": "上游流结束但没有 finish_reason，本次回复不完整"},
		}))
		log.Printf("[异常] traceId=%s requestId=%d 发生阶段=Messages收尾 账号=%s 结果=拒绝当成成功 原因=上游干净结束但没有 finish_reason", debugTraceID(r), reqID, acc.Path)
		recordModelTTFT(modelName, body.duration())
		recordModelLatency(modelName, time.Since(startTime))
		return
	}

	writeEvent(state.finishEvents())
	observeModelCredit(acc, modelName, state.usage, reqID)
	recordModelTokens(modelName, state.usage, reqID)
	recordModelTTFT(modelName, body.duration())
	recordModelLatency(modelName, time.Since(startTime))
	debugEvent(r, "info", "stream_response_completed", map[string]any{"status_code": http.StatusOK})
	log.Printf("[#%d] Messages 流式输出完成 (账号 %s [%s], 耗时 %v, 首字 %v)", reqID, acc.Path, prof.Label, time.Since(startTime), body.duration())
}

// writeMessagesAggregate 本地聚合上游流式数据，输出完整 Anthropic Message。
func writeMessagesAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	state := newAnthropicStreamState(modelName)
	body := newTTFTReader(resp.Body, startTime)
	if err := scanUpstreamSSE(body, state, nil); err != nil {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游聚合读取 账号=%s 异常=%v", debugTraceID(r), reqID, acc.Path, err)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	if state.finish == "" {
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type":      "stream_closed_without_finish",
			"business_impact": "上游干净结束但没有 finish_reason，拒绝聚合为完整 Message",
		})
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "上游流结束但没有 finish_reason，本次回复不完整")
		return
	}
	msg := state.aggregate()
	observeModelCredit(acc, modelName, state.usage, reqID)
	recordModelTokens(modelName, state.usage, reqID)
	recordModelTTFT(modelName, body.duration())
	recordModelLatency(modelName, time.Since(startTime))
	out, err := json.Marshal(msg)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "序列化响应失败")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(out),
	})
	log.Printf("[#%d] Messages 非流式响应完成 (账号 %s [%s], 耗时 %v)", reqID, acc.Path, prof.Label, time.Since(startTime))
}

// handleCountTokens 处理 POST /v1/messages/count_tokens。
// V1 为本地估算（不打上游）：CJK 字符约 1 token/字，ASCII 约 4 字符/token，
// tools schema 按序列化字节估算。Claude Code 对该端点缺失时会本地兜底，
// 精确计数可后续用 max_tokens=1 真实流式调用实现。
func handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "api_error", "仅支持 POST 请求")
		return
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "api_error", "读取请求体失败")
		return
	}
	defer r.Body.Close()
	var msgReq map[string]any
	if err := json.Unmarshal(bodyBytes, &msgReq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "无效的 JSON 请求体")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"input_tokens": estimateAnthropicInputTokens(msgReq),
	})
}

func estimateAnthropicInputTokens(body map[string]any) int {
	var sb strings.Builder
	if sys := extractAnthropicSystemText(body["system"]); sys != "" {
		sb.WriteString(sys)
		sb.WriteByte('\n')
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			appendAnthropicContentText(&sb, msg["content"])
			sb.WriteByte('\n')
		}
	}
	n := approxTokens(sb.String())
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			n += len(b) / 4
		}
	}
	return n
}

func appendAnthropicContentText(sb *strings.Builder, content any) {
	switch c := content.(type) {
	case string:
		sb.WriteString(c)
	case []any:
		for _, bAny := range c {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				sb.WriteString(strOf(blk["text"]))
			case "tool_result":
				sb.WriteString(anthropicContentText(blk["content"]))
			case "tool_use":
				sb.WriteString(strOf(blk["name"]))
				if b, err := json.Marshal(blk["input"]); err == nil {
					sb.Write(b)
				}
			}
		}
	}
}

func approxTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}
