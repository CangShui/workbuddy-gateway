package main

import (
	"log"
	"net/http"
	"strings"
)

// reasoningRepairReport 只统计字段形状与数量，不保存或记录思维链正文。
type reasoningRepairReport struct {
	Applied                bool
	ThinkingEnabled        bool
	HasTrace               bool
	AssistantMessages      int
	PreservedContent       int
	CopiedFromReasoning    int
	EmptyContentAdded      int
	InvalidContentReplaced int
	ReasoningMirrored      int
	ReasoningPlaceholders  int
}

// repairReasoningHistory 在发往上游的 Chat 消息形态上做一次共享修复。
// 两条入口都必须在 tool_call 序列修复后、json.Marshal 前调用：
// Chat 原生输入与 Responses 转换后的输入因此遵守同一套 DeepSeek thinking 规则。
// 已有推理字符串不覆盖；未收到推理的助手回合只补空 reasoning_content，并在上游
// 需要非空 reasoning 字段的形态下给 reasoning 一个空白占位，而不是捏造思维链。
func repairReasoningHistory(obj map[string]any) reasoningRepairReport {
	var report reasoningRepairReport
	model, _ := obj["model"].(string)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return report
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return report
	}

	report.ThinkingEnabled = thinkingExplicitlyEnabled(obj)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		if _, exists := msg["reasoning_content"]; exists {
			report.HasTrace = true
			break
		}
		if reasoning, ok := msg["reasoning"].(string); ok && reasoning != "" {
			report.HasTrace = true
			break
		}
	}
	// 显式关闭且无既有推理痕迹时不碰历史。关闭后续思考不意味着要抹掉此前
	// 已经产生的推理：有痕迹仍须回放，避免多轮上下文不一致。
	if !report.ThinkingEnabled && !report.HasTrace {
		return report
	}
	report.Applied = true
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		report.AssistantMessages++
		rc, isString := msg["reasoning_content"].(string)
		if isString {
			report.PreservedContent++
		} else {
			if _, exists := msg["reasoning_content"]; exists {
				report.InvalidContentReplaced++
			}
			if original, ok := msg["reasoning"].(string); ok {
				rc = original
				report.CopiedFromReasoning++
			} else {
				rc = ""
				report.EmptyContentAdded++
			}
			msg["reasoning_content"] = rc
		}
		if existing, ok := msg["reasoning"].(string); ok && existing != "" {
			continue
		}
		if rc != "" {
			msg["reasoning"] = rc
			report.ReasoningMirrored++
		} else {
			// 对齐上游部分租户对 len(reasoning)>0 的校验；单个空格不是推理内容。
			msg["reasoning"] = " "
			report.ReasoningPlaceholders++
		}
	}
	return report
}

// thinkingExplicitlyEnabled 仅识别客户端已明确开启的思考信号；不替用户开启思考、
// 不更改原有 effort 或 thinking.type。显式 disabled 的优先级高于其他字段。
func thinkingExplicitlyEnabled(obj map[string]any) bool {
	if thinking, ok := obj["thinking"].(map[string]any); ok {
		switch strings.ToLower(strings.TrimSpace(stringField(thinking, "type"))) {
		case "disabled":
			return false
		case "enabled":
			return true
		}
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "reasoning_summary"} {
		if enabledEffort(stringField(obj, key)) {
			return true
		}
	}
	if reasoning, ok := obj["reasoning"].(map[string]any); ok {
		return enabledEffort(stringField(reasoning, "effort"))
	}
	return false
}

func stringField(obj map[string]any, key string) string {
	value, _ := obj[key].(string)
	return value
}

func enabledEffort(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "off", "disabled":
		return false
	default:
		return true
	}
}

// logReasoningHistoryRepair 复用网关已有文件日志/TraceID；敏感正文绝不入日志。
func logReasoningHistoryRepair(r *http.Request, reqID uint64, model string, report reasoningRepairReport) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return
	}
	result := "跳过（思考未开启且无历史痕迹）"
	if report.Applied {
		result = "已核对并补全助手历史"
	}
	log.Printf("[推理历史回填] traceId=%s requestId=%d 模型=%s 结果=%s thinking开启=%t 历史有痕迹=%t 助手消息=%d 原推理保留=%d 从reasoning复制=%d 补空reasoning_content=%d 非字符串归一=%d reasoning镜像=%d reasoning空白占位=%d 业务影响=避免DeepSeek长会话11155且不记录推理正文",
		debugTraceID(r), reqID, model, result, report.ThinkingEnabled, report.HasTrace,
		report.AssistantMessages, report.PreservedContent, report.CopiedFromReasoning,
		report.EmptyContentAdded, report.InvalidContentReplaced,
		report.ReasoningMirrored, report.ReasoningPlaceholders)
	debugEvent(r, "debug", "reasoning_history_repaired", map[string]any{
		"result": result, "thinking_enabled": report.ThinkingEnabled, "has_trace": report.HasTrace,
		"assistant_messages": report.AssistantMessages, "preserved_content": report.PreservedContent,
		"copied_from_reasoning": report.CopiedFromReasoning, "empty_content_added": report.EmptyContentAdded,
		"invalid_content_replaced": report.InvalidContentReplaced,
		"reasoning_mirrored":       report.ReasoningMirrored,
		"reasoning_placeholders":   report.ReasoningPlaceholders,
		"business_impact":          "已校验推理历史字段形状；不保存提示词或推理正文",
	})
}
