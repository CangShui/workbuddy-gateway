package main

// 成长任务的管理台入口：手动跑一轮 + 开关。
//
// ⚠️ 两个端点都挂在 /admin/api/ 下，由 adminAuthMiddleware 统一要求管理 Key，
// 写操作还过 adminSecurityMiddleware 的同源与 JSON 校验，无需在此自判鉴权。
//
// ⚠️ 手动跑一轮是**真实写操作**（会向上游领奖、改变账号当日状态），
// 因此即便开关关闭也允许手动触发 —— 开关只管「定时自动跑」。

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

// handleWebUIGrowthRun 手动触发一轮成长任务领取（POST，空 JSON 对象）。
func handleWebUIGrowthRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input map[string]json.RawMessage
	if err := decodeAdminJSON(w, r, &input); err != nil || input == nil || len(input) != 0 {
		log.Printf("[成长任务] traceId=%s 阶段=参数校验 结果=拒绝 原因=请求JSON无效 状态码=400", debugTraceID(r))
		writeJSONError(w, http.StatusBadRequest, "领取请求必须为空 JSON 对象")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	started := time.Now()
	results, err := runAllGrowthTasks(ctx)
	if err != nil {
		// 开关关闭是唯一预期的错误：明确告知怎么开，不要说成「执行失败」。
		log.Printf("[成长任务] traceId=%s 结果=拒绝 原因=%v", debugTraceID(r), err)
		writeJSONError(w, http.StatusConflict, err.Error()+"; 可先在配置里开启 growth.enabled，或直接点手动领取")
		return
	}
	claimed, credits, failed := 0, 0.0, 0
	for _, item := range results {
		claimed += item.ClaimedCount
		credits += item.ClaimedCredit
		failed += item.Failed
	}
	log.Printf("[成长任务] traceId=%s 结果=完成 账号=%d 自动领取=%d 积分=%.0f 失败=%d 耗时=%v",
		debugTraceID(r), len(results), claimed, credits, failed, time.Since(started).Round(time.Millisecond))
	writeAdminData(w, r, map[string]any{
		"accounts":      results,
		"claimedCount":  claimed,
		"claimedCredit": credits,
		"failed":        failed,
		"enabled":       growthTasksEnabled.Load(),
		"durationMs":    time.Since(started).Milliseconds(),
	})
}

// handleWebUIGrowthToggle 读写成长任务定时开关。
//
// ⚠️ 与 handleWebUILogSettings 同款形态：GET 读当前值，POST 写入并落盘。
// 写入走 persistRuntimeDocument（先校验 → 落盘 → 应用 → 失败回滚），
// 保证 config.json 的其它段原样保留。
func handleWebUIGrowthToggle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		log.Printf("[成长任务] traceId=%s 阶段=读取 开关=%t 结果=成功", debugTraceID(r), growthTasksEnabled.Load())
		writeJSON(w, http.StatusOK, map[string]any{"enabled": growthTasksEnabled.Load()})
	case http.MethodPost:
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil || input.Enabled == nil {
			log.Printf("[成长任务] traceId=%s 阶段=参数校验 结果=拒绝 原因=enabled缺失或格式无效 状态码=400", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "必须提供 enabled 布尔值")
			return
		}
		if err := saveGrowthEnabled(*input.Enabled, debugTraceID(r)); err != nil {
			writeAdminOperationError(w, r, "成长任务开关", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": *input.Enabled, "saved": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

// saveGrowthEnabled 把 growth.enabled 写入 config.json 并应用。
func saveGrowthEnabled(enabled bool, trace string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()

	doc, gateway, old, err := readGatewayDocumentLocked()
	if err != nil {
		return err
	}
	if gateway.AdminKey != cfg.AdminKey {
		log.Printf("[成长任务] traceId=%s 阶段=配置校验 结果=拒绝 原因=管理Key已变更 说明=已保留现有配置", trace)
		return errConfigChange
	}
	// 只改 growth 段，其余段（含未知段）原样保留。
	segment := map[string]any{"enabled": enabled}
	raw, err := json.Marshal(segment)
	if err != nil {
		return err
	}
	doc["growth"] = raw
	log.Printf("[成长任务] traceId=%s 阶段=配置写入 旧开关=%t 新开关=%t 说明=其他配置段原样保留",
		trace, growthTasksEnabled.Load(), enabled)
	if err := persistRuntimeDocument(doc, old, trace); err != nil {
		return err
	}
	if growthTasksEnabled.Load() != enabled {
		return errors.New("配置已写入但运行时未生效，请检查服务器日志")
	}
	return nil
}
