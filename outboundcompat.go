package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

const (
	requestProfileLegacy   = "legacy"
	requestProfileOfficial = "official"
	// 固定为 2026-10-21 国内桌面实抓版本，不声称跟随官网最新版本。
	observedDesktopUA = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
)

var officialRequests atomic.Bool

type requestProfileContextKey struct{}

func normalizeRequestProfile(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", requestProfileLegacy:
		return requestProfileLegacy, nil
	case requestProfileOfficial:
		return requestProfileOfficial, nil
	default:
		return "", fmt.Errorf("upstream.requestProfile 只支持 legacy 或 official")
	}
}

func setRequestProfile(value string) {
	profile, _ := normalizeRequestProfile(value) // 整份配置已在应用前校验。
	officialRequests.Store(profile == requestProfileOfficial)
}

// 一次逻辑请求只取一次快照，避免热加载使正文、请求头和重试使用不同配置。
func requestProfileFor(r *http.Request) string {
	if r != nil {
		if value, ok := r.Context().Value(requestProfileContextKey{}).(string); ok {
			return value
		}
	}
	profile := requestProfileLegacy
	if officialRequests.Load() {
		profile = requestProfileOfficial
	}
	if r != nil {
		*r = *r.WithContext(context.WithValue(r.Context(), requestProfileContextKey{}, profile))
	}
	return profile
}

// 三个入口在协议转译后共用此处。不要提升 user 角色，不替换客户端指令或工具。
func prepareUpstreamChatRequest(obj map[string]any, r *http.Request, requestID uint64, model string) {
	profile := requestProfileFor(r)
	trace := debugTraceID(r)
	applyThinkingRules(obj, model)
	if modified := sanitizeMessages(obj); modified > 0 {
		logSystemPrefixSanitized(r, requestID, modified)
	}
	if profile == requestProfileOfficial {
		mergeLeadingInstructionMessages(obj, r, requestID)
	}
	prepareSystemPromptForUpstream(obj, r, requestID, trace)
	ensureUpstreamMaxTokens(obj, r, requestID, trace, model)
	if profile == requestProfileOfficial {
		applyOfficialBodyDefaults(obj, r, requestID, model)
	}
	report := repairToolMessageSequence(obj)
	logToolSequenceRepair(r, trace, requestID, model, report)
	logReasoningHistoryRepair(r, requestID, model, repairReasoningHistory(obj))
}

func applyOfficialBodyDefaults(obj map[string]any, r *http.Request, id uint64, model string) {
	var added []string
	put := func(key string, value any) {
		if old, exists := obj[key]; !exists || old == nil {
			obj[key] = value
			added = append(added, key)
		}
	}
	// Chat 新旧输出预算字段等价；只有没有 max_tokens 时才转换，冲突值不擅自覆盖。
	if value := obj["max_completion_tokens"]; value != nil && obj["max_tokens"] == nil {
		obj["max_tokens"] = value
		delete(obj, "max_completion_tokens")
		added = append(added, "max_completion_tokens->max_tokens")
	}
	// 上游总是 SSE，独立的 usage 事件有助于三个下游协议得到相同计费数据。
	if value, exists := obj["stream_options"]; !exists || value == nil {
		obj["stream_options"] = map[string]any{"include_usage": true}
		added = append(added, "stream_options.include_usage")
	} else if options, ok := value.(map[string]any); ok {
		if old, exists := options["include_usage"]; !exists || old == nil {
			options["include_usage"] = true
			added = append(added, "stream_options.include_usage")
		}
	}
	// 只有该模型有完整桌面样本。其他模型不猜测其 temperature/verbosity 支持范围。
	if normalizeModelName(model) == "deepseek-v4.1-flash" {
		if !clientDeclaredOutputBudget(obj) {
			put("max_tokens", 128000) // 目录尚未就绪时，使用已验证的样本输出上限。
		}
		if obj["top_p"] == nil {
			put("temperature", 1)
		}
		put("verbosity", "high")
	}
	sort.Strings(added)
	log.Printf("[出站兼容] traceId=%s requestId=%d profile=official 阶段=正文准备 模型=%s 补入字段=%v 结果=完成 说明=保留显式档位、用户角色、原文与工具；不复制官方私有提示词",
		debugTraceID(r), id, model, added)
	debugEvent(r, "debug", "official_request_body_prepared", map[string]any{
		"profile": requestProfileOfficial, "defaulted_fields": added,
		"reasoning_effort": obj["reasoning_effort"],
		"business_impact":  "统一三个入口的已验证默认字段；不将high强制升级max，不提升user指令层级",
	})
}

// 仅合并连续前导、无附加元数据的纯文本 system。user、图片、cache_control、
// 中途插入的 system 及其他字段均不猜测或丢弃；强制提示词在合并后才追加。
func mergeLeadingInstructionMessages(obj map[string]any, r *http.Request, id uint64) {
	messages, _ := obj["messages"].([]any)
	var texts []string
	count := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || roleOfMessage(message) != "system" || len(message) != 2 {
			break
		}
		var parts []string
		switch content := message["content"].(type) {
		case string:
			parts = []string{content}
		case []any:
			for _, raw := range content {
				block, ok := raw.(map[string]any)
				if !ok || len(block) != 2 || block["type"] != "text" {
					parts = nil
					break
				}
				text, ok := block["text"].(string)
				if !ok {
					parts = nil
					break
				}
				parts = append(parts, text)
			}
		}
		if parts == nil {
			break
		}
		texts = append(texts, parts...)
		count++
	}
	if count == 0 {
		return
	}
	first := messages[0].(map[string]any)
	_, wasString := first["content"].(string)
	if count == 1 && wasString {
		return
	}
	first["content"] = strings.Join(texts, "\n\n")
	out := make([]any, 0, len(messages)-count+1)
	out = append(out, first)
	out = append(out, messages[count:]...)
	obj["messages"] = out
	log.Printf("[出站兼容] traceId=%s requestId=%d 阶段=指令结构 结果=合并连续前导纯文本system 合并消息数=%d 说明=保留文本顺序，不跨user边界，不记录正文", debugTraceID(r), id, count)
	debugEvent(r, "debug", "official_instruction_messages_merged", map[string]any{
		"messages_merged": count, "business_impact": "仅把已明确标注为system的连续纯文本合并；不提升user角色",
	})
}

func usesObservedDesktopHeaders(profile string, prof *upstreamProfile) bool {
	return profile == requestProfileOfficial && prof.Key == "cn"
}

// 只用于模型请求。登录/刷新/额度接口继续沿用原有鉴权逻辑。
// 不伪造 X-Device-Token、SDK/Node 运行时信息或其他无法取得的客户端凭证。
func applyObservedDesktopHeaders(req *http.Request, prof *upstreamProfile, profile string) {
	if !usesObservedDesktopHeaders(profile, prof) || req.Method != http.MethodPost || req.URL.Path != "/v2/chat/completions" {
		return
	}
	for _, name := range []string{
		"Origin", "Referer", "X-Client-ID", "X-Client-Version", "X-Refresh-Token",
		"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info",
	} {
		req.Header.Del(name)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", observedDesktopUA)
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Version", "5.8.3")
	req.Header.Set("X-Private-Data", "true")
	// Agent 用途随主请求/标题/子任务变化；没有可靠元数据时不把所有调用冒充 main。
	req.Header.Set("X-Codebuddy-Request", "1")
	// 保持同一逻辑请求的 message/request ID 相等；重试沿用，不改变本地审计 TraceID。
	message := strings.ReplaceAll(req.Header.Get(conversationMessageIDHeader), "-", "")
	req.Header.Set(conversationMessageIDHeader, message)
	req.Header.Set("X-Request-ID", message)
}

func encodeModelRequest(raw []byte, profile string, prof *upstreamProfile) ([]byte, error) {
	if !usesObservedDesktopHeaders(profile, prof) {
		return raw, nil
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
