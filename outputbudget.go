package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

const outputBudgetErrorCode = "output_budget_exhausted"
const outputBudgetErrorMessage = "上游已耗尽本次输出预算，但未生成正文或工具调用。请调整思考档位或输出预算后再试；网关不会因此自动重试，也不会更改模型或思考设置。"

// completionOutput 只观察上游原始增量，不缓存正文；身份还原的暂存尾部不影响判断。
type completionOutput struct {
	content bool
	tool    bool
}

func (s *completionOutput) observe(chunk map[string]any) {
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta, _ = choice["message"].(map[string]any)
		}
		for _, key := range []string{"content", "refusal"} {
			if text, ok := delta[key].(string); ok && strings.TrimSpace(text) != "" {
				s.content = true
			}
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			s.observeFunction(fn)
		}
		fn, _ := delta["function_call"].(map[string]any)
		s.observeFunction(fn)
	}
}

func (s *completionOutput) observeFunction(fn map[string]any) {
	if strings.TrimSpace(strOf(fn["name"])) != "" || strings.TrimSpace(strOf(fn["arguments"])) != "" {
		s.tool = true
	}
}

func (s completionOutput) emptyLimit(finish string) bool {
	return finish == "length" && !s.content && !s.tool
}

func completionOutputFromJSON(raw []byte) (completionOutput, string) {
	var chunk map[string]any
	_ = json.Unmarshal(raw, &chunk) // 调用方传入 aggregateCompletion 已序列化的响应。
	var output completionOutput
	output.observe(chunk)
	return output, noteFinishReason("", chunk)
}

func outputBudgetErrorPayload(usage map[string]any, anthropic bool) map[string]any {
	out := map[string]any{
		"error": map[string]any{
			"type": "invalid_request_error", "code": outputBudgetErrorCode, "message": outputBudgetErrorMessage,
		},
	}
	if anthropic {
		out["type"] = "error"
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

// 聚合请求使用 400，明确这是预算不足，不把已计费的生成变成可自动重试的 5xx。
func writeOutputBudgetError(w http.ResponseWriter, usage map[string]any, anthropic bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(outputBudgetErrorPayload(usage, anthropic))
}

func logOutputBudget(r *http.Request, reqID uint64, model, finish string, output completionOutput, usage map[string]any) {
	if finish != "length" {
		return
	}
	completion, _ := usageNumber(usage, "completion_tokens")
	reasoning, ok := usageNumber(usage, "completion_thinking_tokens")
	if !ok {
		details, _ := usage["completion_tokens_details"].(map[string]any)
		reasoning, _ = usageNumber(details, "reasoning_tokens")
	}
	result := "保留已有输出并标记预算截断"
	if output.emptyLimit(finish) {
		result = "预算耗尽但无正文或工具调用，拒绝记为成功"
	}
	log.Printf("[输出预算耗尽] traceId=%s requestId=%d 模型=%s 阶段=上游响应收尾 结束原因=%s 有正文=%t 有工具=%t 输出Token=%d 推理Token=%d 结果=%s 业务影响=保留实测费用，不自动重试、不换号、不改变思考档位；不记录正文",
		debugTraceID(r), reqID, model, finish, output.content, output.tool, completion, reasoning, result)
	debugEvent(r, "warn", "output_budget_exhausted", map[string]any{
		"finish_reason": finish, "has_content": output.content, "has_tool": output.tool,
		"completion_tokens": completion, "reasoning_tokens": reasoning,
		"empty_output":    output.emptyLimit(finish),
		"business_impact": "输出预算已耗尽，响应不记为完整成功；实测用量仍结算，不自动重试或降档",
	})
}
