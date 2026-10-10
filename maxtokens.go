package main

import (
	"log"
	"net/http"
)

// 上游按「输入 token + 输出预算」判断是否超出模型窗口：请求不带 max_tokens 时，
// 上游会为输出默认预留 384000，标称 1048576 窗口的 deepseek-v4.1-flash 实际只剩
// 约 664576 可用输入。长会话会在远未到窗口上限时被 400 code=11133
// （model_param_invalid，报错文案不指明具体参数）拒绝，表现为「聊着聊着就 400」。
//
// 实测（2026-10-10，deepseek-v4.1-flash，窗口 1048576）：
//
//	678912 输入 + 未声明输出预算       -> 400 code=11133
//	678912 输入 + max_tokens=128000    -> 200
//	678912 输入 + max_tokens=400000    -> 400 code=11133
//
// 因此仅在客户端未声明输出预算时，补上模型目录声明的输出上限，把被默认预留的
// 额度收回来。客户端显式传入的值一律不覆盖；目录未收录的模型保持原样，不猜测。
func ensureUpstreamMaxTokens(obj map[string]any, r *http.Request, requestID uint64, traceID, modelName string) {
	if clientDeclaredOutputBudget(obj) {
		return
	}
	limit := modelMaxOutputTokens(modelName)
	if limit <= 0 {
		return
	}
	obj["max_tokens"] = limit
	if traceID == "" {
		traceID = debugTraceID(r)
	}
	if traceID == "" {
		traceID = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[输出预算] traceId=%s requestId=%d 模型=%s 阶段=上游请求序列化前 客户端已声明输出预算=false 结果=已补max_tokens=%d 业务影响=收回上游默认预留的输出额度，避免长会话被11133拒绝",
		traceID, requestID, modelName, limit)
	debugEvent(r, "debug", "max_tokens_budget_injected", map[string]any{
		"model": modelName, "injected_max_tokens": limit,
		"business_impact": "客户端未声明输出预算，网关按模型目录补入输出上限以收回上游默认预留额度；不覆盖客户端显式值",
	})
}

// clientDeclaredOutputBudget 判断客户端是否已声明输出预算。
// max_tokens 为 null 视为未声明；max_completion_tokens 是等价的新字段，
// 只要其中任意一个带有效值就保持请求原样。
func clientDeclaredOutputBudget(obj map[string]any) bool {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if value, ok := obj[key]; ok && value != nil {
			return true
		}
	}
	return false
}

// modelMaxOutputTokens 返回模型在站点目录中声明的最大输出 token 数。
// 目录未收录该模型时返回 0，调用方应保持请求原样。
func modelMaxOutputTokens(modelID string) int {
	modelID = normalizeModelName(modelID)
	if modelID == "" {
		return 0
	}
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	best := 0
	for _, site := range catalogSites {
		for _, m := range catalogModels[site] {
			if m.ID == modelID && m.MaxOutputTokens > best {
				best = m.MaxOutputTokens
			}
		}
	}
	return best
}
