package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------------
// OpenAI Responses API (/v1/responses) -> 上游 Chat Completions 转译
//
// 网关只与上游 /v2/chat/completions 通信，此处负责双向协议转换：
//   - 请求：input / instructions / tools(扁平) / tool_choice / max_output_tokens /
//     reasoning.effort  ->  chat messages / tools(嵌套 function) / max_tokens / reasoning_effort
//   - 非流式响应：chat.completion -> response{object:"response", output:[...]}
//   - 流式响应：上游 SSE 增量 -> Responses 语义事件（response.output_text.delta、
//     response.function_call_arguments.delta、response.completed ...）
// -----------------------------------------------------------------------------

// handleResponses 处理 POST /v1/responses。
func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}

	reqID := atomic.AddUint64(&reqCounter, 1)
	startTime := time.Now()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	var respReq map[string]any
	if err := json.Unmarshal(bodyBytes, &respReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}

	modelName, _ := respReq["model"].(string)
	if modelName == "" {
		modelName = "hy4-preview"
	}
	isStream, _ := respReq["stream"].(bool)

	chatReq, err := responsesToChatRequest(respReq, modelName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatReq["stream"] = true // 上游强制流式，非流式由网关本地聚合

	applyThinkingRules(chatReq, modelName)
	sanitizeMessages(chatReq)
	ensureLeadingSystemMessage(chatReq)

	upstreamBytes, err := json.Marshal(chatReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "序列化请求失败")
		return
	}

	log.Printf("[#%d] POST /v1/responses -> Upstream [Model: %s, Stream: %v]", reqID, modelName, isStream)

	resp, acc, prof, ok := upstreamChat(w, r, reqID, modelName, upstreamBytes, startTime)
	if !ok {
		return
	}
	if isStream {
		streamResponsesResponse(w, resp, modelName, reqID, acc, prof, startTime)
	} else {
		writeResponsesAggregate(w, resp, modelName, reqID, acc, prof, startTime)
	}
}

// responsesToChatRequest 将 Responses 请求体转换为上游 Chat Completions 请求体。
func responsesToChatRequest(respReq map[string]any, modelName string) (map[string]any, error) {
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if instructions, ok := respReq["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := respReq["input"].(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, itemAny := range input {
			switch item := itemAny.(type) {
			case string:
				messages = append(messages, map[string]any{"role": "user", "content": item})
			case map[string]any:
				messages = append(messages, convertResponsesInputItem(item)...)
			}
		}
	}

	if len(messages) == 0 {
		return nil, fmt.Errorf("input 为空：Responses 请求必须提供 input 或 instructions")
	}
	chat["messages"] = messages

	if v, ok := respReq["temperature"]; ok && v != nil {
		chat["temperature"] = v
	}
	if v, ok := respReq["top_p"]; ok && v != nil {
		chat["top_p"] = v
	}
	if v, ok := respReq["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if tools := convertResponsesTools(respReq["tools"]); len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc := convertResponsesToolChoice(respReq["tool_choice"]); tc != nil {
		chat["tool_choice"] = tc
	}
	if reasoning, ok := respReq["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" && effort != "none" {
			chat["reasoning_effort"] = effort
		}
	}
	return chat, nil
}

// convertResponsesInputItem 将单个 Responses input item 转换为 0..1 条 chat 消息。
func convertResponsesInputItem(item map[string]any) []any {
	switch typ, _ := item["type"].(string); typ {
	case "function_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       ifEmpty(callID, "call_"+compactUUID()),
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}
	case "function_call_output":
		callID, _ := item["call_id"].(string)
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      stringifyToolOutput(item["output"]),
		}}
	case "reasoning":
		// 上游无法接收 reasoning item，忽略（历史上下文不影响后续对话）
		return nil
	}

	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	return []any{map[string]any{"role": role, "content": convertResponsesContent(item["content"])}}
}

// convertResponsesContent 将 Responses content（string 或 parts 数组）转换为 chat content。
func convertResponsesContent(content any) any {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]any, 0, len(c))
		for _, pAny := range c {
			p, ok := pAny.(map[string]any)
			if !ok {
				if s, ok := pAny.(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
				continue
			}
			switch typ, _ := p["type"].(string); typ {
			case "input_text", "output_text", "text":
				if t, ok := p["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t, ok := p["refusal"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image":
				url, _ := p["image_url"].(string)
				if url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return fmt.Sprintf("%v", c)
	}
}

// stringifyToolOutput 将 function_call_output 的 output 统一转为字符串。
func stringifyToolOutput(output any) string {
	switch o := output.(type) {
	case nil:
		return ""
	case string:
		return o
	default:
		if b, err := json.Marshal(o); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", o)
	}
}

// convertResponsesTools 将 Responses 扁平 function 工具转换为 chat 嵌套 function 工具。
func convertResponsesTools(toolsAny any) []any {
	arr, ok := toolsAny.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, tAny := range arr {
		t, ok := tAny.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := t["type"].(string)
		if typ != "" && typ != "function" {
			continue // 仅支持 function 工具
		}
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		params := t["parameters"]
		if name == "" {
			// 兼容旧式嵌套 {type:"function", function:{...}}
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
				desc, _ = fn["description"].(string)
				params = fn["parameters"]
			}
		}
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if desc != "" {
			fn["description"] = desc
		}
		if params != nil {
			fn["parameters"] = params
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// convertResponsesToolChoice 将 Responses tool_choice 转换为 chat tool_choice。
func convertResponsesToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		if v == "auto" || v == "none" || v == "required" {
			return v
		}
	case map[string]any:
		if typ, _ := v["type"].(string); typ == "function" {
			name, _ := v["name"].(string)
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
	return nil
}

// writeResponsesAggregate 聚合上游 SSE 后转换为 Responses 非流式响应。
func writeResponsesAggregate(w http.ResponseWriter, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		log.Printf("[#%d] 聚合响应失败: %v", reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	observeModelCredit(acc, modelName, usageFromCompletion(completionJSON), reqID)
	recordModelTTFT(modelName, body.duration())
	recordModelLatency(modelName, time.Since(startTime))
	out, err := chatCompletionToResponses(completionJSON, modelName)
	if err != nil {
		log.Printf("[#%d] Responses 转换失败: %v", reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "translate_error", "转换 Responses 响应失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	log.Printf("[#%d] Responses 非流式响应完成 (账号 %s [%s], 耗时 %v)", reqID, acc.Path, prof.Label, time.Since(startTime))
}

// chatCompletionToResponses 将 chat.completion JSON 转换为 Responses 响应对象。
func chatCompletionToResponses(chatJSON []byte, modelName string) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(chatJSON, &chat); err != nil {
		return nil, err
	}

	var content, reasoning string
	var toolCalls []any
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				content, _ = msg["content"].(string)
				reasoning, _ = msg["reasoning_content"].(string)
				toolCalls, _ = msg["tool_calls"].([]any)
			}
		}
	}

	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":      "rs_" + compactUUID(),
			"type":    "reasoning",
			"status":  "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if content != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + compactUUID(),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": content, "annotations": []any{}},
			},
		})
	}
	for _, tcAny := range toolCalls {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		callID, _ := tc["id"].(string)
		name, args := "", ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		output = append(output, map[string]any{
			"id":        "fc_" + compactUUID(),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   ifEmpty(callID, "call_"+compactUUID()),
			"name":      name,
			"arguments": args,
		})
	}

	now := time.Now().Unix()
	created := now
	if v, ok := chat["created"].(float64); ok && v > 0 && int64(v) <= now {
		created = int64(v)
	}
	respID, _ := chat["id"].(string)
	if respID == "" {
		respID = "resp_" + compactUUID()
	} else {
		respID = "resp_" + strings.TrimPrefix(respID, "chatcmpl-")
	}

	result := buildResponsesEnvelope(respID, modelName, created)
	result["status"] = "completed"
	result["output"] = output
	result["completed_at"] = now
	if usage, ok := chat["usage"].(map[string]any); ok {
		result["usage"] = toResponsesUsage(usage)
	}
	return json.Marshal(result)
}

// buildResponsesEnvelope 构造 Responses 响应骨架（含规范要求的常见字段）。
func buildResponsesEnvelope(id, model string, createdAt int64) map[string]any {
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           createdAt,
		"completed_at":         nil,
		"status":               "in_progress",
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                true,
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"usage":                nil,
		"metadata":             map[string]any{},
	}
}

// toResponsesUsage 将 chat usage 转换为 Responses usage 结构。
func toResponsesUsage(u map[string]any) map[string]any {
	in := numOr0(u["prompt_tokens"])
	out := numOr0(u["completion_tokens"])
	total := numOr0(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	cached, reasoningTokens := 0.0, 0.0
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = numOr0(d["cached_tokens"])
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoningTokens = numOr0(d["reasoning_tokens"])
	}
	return map[string]any{
		"input_tokens":          int64(in),
		"input_tokens_details":  map[string]any{"cached_tokens": int64(cached)},
		"output_tokens":         int64(out),
		"output_tokens_details": map[string]any{"reasoning_tokens": int64(reasoningTokens)},
		"total_tokens":          int64(total),
	}
}

func numOr0(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// streamResponsesResponse 将上游 Chat Completions SSE 实时转译为 Responses 语义事件流。
func streamResponsesResponse(w http.ResponseWriter, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}

	seq := 0
	emit := func(eventType string, data map[string]any) {
		data["type"] = eventType
		data["sequence_number"] = seq
		seq++
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
		flusher.Flush()
	}

	created := time.Now().Unix()
	respID := "resp_" + compactUUID()
	envelope := buildResponsesEnvelope(respID, modelName, created)
	emit("response.created", map[string]any{"response": envelope})
	emit("response.in_progress", map[string]any{"response": envelope})

	nextIndex := 0
	idxToItem := map[int]map[string]any{}

	// reasoning item 状态
	reasoningOpen := false
	reasoningIndex := -1
	reasoningItemID := ""
	var reasoningSB strings.Builder

	// message item 状态
	msgOpen := false
	msgIndex := -1
	msgItemID := ""
	var msgSB strings.Builder

	// function_call item 状态
	toolCalls := map[int]*mergedToolCall{}
	var toolOrder []int
	tcItemID := map[int]string{}
	tcIndex := map[int]int{}

	openReasoning := func() {
		if reasoningOpen {
			return
		}
		reasoningOpen = true
		reasoningIndex = nextIndex
		nextIndex++
		reasoningItemID = "rs_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": reasoningIndex,
			"item":         map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		})
		emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}

	openMessage := func() {
		if msgOpen {
			return
		}
		msgOpen = true
		msgIndex = nextIndex
		nextIndex++
		msgItemID = "msg_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": msgIndex,
			"item":         map[string]any{"id": msgItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		})
		emit("response.content_part.added", map[string]any{
			"item_id": msgItemID, "output_index": msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "annotations": []any{}, "text": ""},
		})
	}

	var usage map[string]any

	body := newTTFTReader(resp.Body, startTime)
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
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				openReasoning()
				reasoningSB.WriteString(rc)
				emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "delta": rc,
				})
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				openMessage()
				msgSB.WriteString(ct)
				emit("response.output_text.delta", map[string]any{
					"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "delta": ct,
				})
			}
			if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
				// 先提取本次参数增量，供合并后补发 delta 事件
				type argDelta struct {
					idx int
					arg string
				}
				var argDeltas []argDelta
				for _, tcAny := range tcs {
					tc, ok := tcAny.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := tc["index"].(float64); ok {
						idx = int(v)
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if a, ok := fn["arguments"].(string); ok && a != "" {
							argDeltas = append(argDeltas, argDelta{idx: idx, arg: a})
						}
					}
				}
				before := len(toolOrder)
				applyToolCallDelta(toolCalls, &toolOrder, tcs)
				for i := before; i < len(toolOrder); i++ {
					idx := toolOrder[i]
					st := toolCalls[idx]
					if st.ID == "" {
						st.ID = "call_" + compactUUID()
					}
					tcItemID[idx] = "fc_" + compactUUID()
					tcIndex[idx] = nextIndex
					nextIndex++
					emit("response.output_item.added", map[string]any{
						"output_index": tcIndex[idx],
						"item": map[string]any{
							"id": tcItemID[idx], "type": "function_call", "status": "in_progress",
							"call_id": st.ID, "name": st.Name, "arguments": "",
						},
					})
				}
				for _, ad := range argDeltas {
					emit("response.function_call_arguments.delta", map[string]any{
						"item_id": tcItemID[ad.idx], "output_index": tcIndex[ad.idx], "delta": ad.arg,
					})
				}
			}
		}
	}

	// 若无任何输出，补一个空 message，保证 output 非空且事件序列完整
	if !reasoningOpen && !msgOpen && len(toolOrder) == 0 {
		openMessage()
	}

	// 收尾：reasoning
	if reasoningOpen {
		text := reasoningSB.String()
		emit("response.reasoning_summary_text.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "text": text})
		emit("response.reasoning_summary_part.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}})
		item := map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
		emit("response.output_item.done", map[string]any{"output_index": reasoningIndex, "item": item})
		idxToItem[reasoningIndex] = item
	}

	// 收尾：message
	if msgOpen {
		text := msgSB.String()
		emit("response.output_text.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "text": text})
		emit("response.content_part.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "annotations": []any{}, "text": text}})
		item := map[string]any{"id": msgItemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "text": text}}}
		emit("response.output_item.done", map[string]any{"output_index": msgIndex, "item": item})
		idxToItem[msgIndex] = item
	}

	// 收尾：function calls
	for _, idx := range toolOrder {
		st := toolCalls[idx]
		args := st.Args.String()
		emit("response.function_call_arguments.done", map[string]any{"item_id": tcItemID[idx], "output_index": tcIndex[idx], "name": st.Name, "arguments": args})
		item := map[string]any{"id": tcItemID[idx], "type": "function_call", "status": "completed", "call_id": st.ID, "name": st.Name, "arguments": args}
		emit("response.output_item.done", map[string]any{"output_index": tcIndex[idx], "item": item})
		idxToItem[tcIndex[idx]] = item
	}

	output := make([]any, 0, len(idxToItem))
	for i := 0; i < nextIndex; i++ {
		if item, ok := idxToItem[i]; ok {
			output = append(output, item)
		}
	}

	final := buildResponsesEnvelope(respID, modelName, created)
	final["status"] = "completed"
	final["output"] = output
	final["completed_at"] = time.Now().Unix()
	if usage != nil {
		final["usage"] = toResponsesUsage(usage)
	}
	observeModelCredit(acc, modelName, usage, reqID)
	emit("response.completed", map[string]any{"response": final})

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	recordModelTTFT(modelName, body.duration())
	recordModelLatency(modelName, time.Since(startTime))
	log.Printf("[#%d] Responses 流式输出完成 (账号 %s [%s], 耗时 %v, 首字 %v)", reqID, acc.Path, prof.Label, time.Since(startTime), body.duration())
}

// compactUUID 返回去掉连字符的随机 UUID。
func compactUUID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}
