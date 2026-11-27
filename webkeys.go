package main

import (
	"errors"
	"log"
	"net/http"
	"strings"
)

// 模型 API Key 的多条管理（新增 / 删除 / 显式查看）。
//
// 三条纪律：
//  1. 只有「新增」和「显式查看」两个接口会返回 Key 原文，其余接口一律只返回掩码；
//  2. 原文不进入日志、审计事件、浏览器持久存储（前端只放在内存与输入框里）；
//  3. 查看与删除都按 id 定位，不做「按行号/按下标」的操作，避免并发改动后错删。

type apiKeyCreateInput struct {
	Label  string `json:"label"`
	APIKey string `json:"apiKey"`
}

type apiKeyIDInput struct {
	ID string `json:"id"`
}

// gatewayAPIKeyIndexOfID 返回指定 id 在数组中的下标，未找到返回 -1。
func gatewayAPIKeyIndexOfID(keys []gatewayAPIKey, id string) int {
	for i := range keys {
		if keys[i].ID == id {
			return i
		}
	}
	return -1
}

func writeGatewayKeyError(w http.ResponseWriter, trace, action string, err error) {
	status := http.StatusInternalServerError
	var validation gatewayValidationError
	if errors.As(err, &validation) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, errConfigChange) || errors.Is(err, errEnabledWithoutAPIKey) {
		status = http.StatusConflict
	}
	log.Printf("[模型Key管理] traceId=%s 阶段=%s 结果=失败 状态码=%d 原因=%v 业务影响=Key列表与鉴权状态未变化", trace, action, status, err)
	writeJSONError(w, status, safeDebugError(err))
}

func handleWebUIAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	trace := debugTraceID(r)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input apiKeyCreateInput
	if err := decodeAdminJSON(w, r, &input); err != nil {
		log.Printf("[模型Key管理] traceId=%s 阶段=新增 结果=拒绝 状态码=400 原因=请求JSON无效或含未定义字段", trace)
		writeJSONError(w, http.StatusBadRequest, "新增 Key 请求必须是合法 JSON，且只接受 label 与 apiKey 两个字段")
		return
	}
	label := strings.TrimSpace(input.Label)
	key := strings.TrimSpace(input.APIKey)
	generated := false
	if key == "" {
		secret, err := randomGatewaySecret()
		if err != nil {
			log.Printf("[模型Key管理] traceId=%s 阶段=新增 结果=失败 原因=随机生成Key失败", trace)
			writeJSONError(w, http.StatusInternalServerError, "随机生成 Key 失败，请重试")
			return
		}
		key, generated = secret, true
	}
	if err := validateGatewaySecret("apiKeys[].apiKey", key); err != nil {
		log.Printf("[模型Key管理] traceId=%s 阶段=新增 结果=拒绝 状态码=400 原因=Key格式不合法", trace)
		writeJSONError(w, http.StatusBadRequest, safeDebugError(err))
		return
	}
	if err := validateGatewayAPIKeyLabel(label); err != nil {
		log.Printf("[模型Key管理] traceId=%s 阶段=新增 结果=拒绝 状态码=400 原因=标签不合法", trace)
		writeJSONError(w, http.StatusBadRequest, safeDebugError(err))
		return
	}
	created := newGatewayAPIKey(key, label)
	total := 0
	err := mutateGatewayAPIKeys(trace, func(g *gatewayFileConfig) error {
		g.APIKeys = append(g.APIKeys, created)
		g.APIKey = ""
		total = len(g.APIKeys)
		return nil
	})
	if err != nil {
		writeGatewayKeyError(w, trace, "新增", err)
		return
	}
	log.Printf("[模型Key管理] traceId=%s 阶段=新增 结果=成功 Key条数=%d 自动生成=%t 自定义标签=%t 说明=Key原文仅在本次响应返回，未写入日志", trace, total, generated, label != "")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           created.ID,
		"label":        created.Label,
		"apiKey":       created.APIKey,
		"apiKeyMasked": maskGatewayAPIKey(created.APIKey),
		"createdAt":    created.CreatedAt,
		"generated":    generated,
		"settings":     gatewaySettingsView(),
	})
}

func handleWebUIAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	trace := debugTraceID(r)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input apiKeyIDInput
	if err := decodeAdminJSON(w, r, &input); err != nil || strings.TrimSpace(input.ID) == "" {
		log.Printf("[模型Key管理] traceId=%s 阶段=删除 结果=拒绝 状态码=400 原因=缺少有效的Key id", trace)
		writeJSONError(w, http.StatusBadRequest, "删除 Key 请求必须提供 id")
		return
	}
	id := strings.TrimSpace(input.ID)
	remaining := 0
	enabledNow := false
	err := mutateGatewayAPIKeys(trace, func(g *gatewayFileConfig) error {
		idx := gatewayAPIKeyIndexOfID(g.APIKeys, id)
		if idx < 0 {
			return gatewayValidationError("未找到该 Key，可能已被删除或配置已重新加载")
		}
		g.APIKeys = append(g.APIKeys[:idx], g.APIKeys[idx+1:]...)
		g.APIKey = ""
		remaining = len(g.APIKeys)
		// 删除不改变鉴权开关；公共配置校验在同一把写锁内拒绝启用鉴权的空列表。
		enabledNow = g.APIKeyEnabled
		return nil
	})
	if err != nil {
		writeGatewayKeyError(w, trace, "删除", err)
		return
	}
	log.Printf("[模型Key管理] traceId=%s 阶段=删除 结果=成功 剩余条数=%d 鉴权启用=%t 说明=已移除指定Key，鉴权开关未改变", trace, remaining, enabledNow)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":       true,
		"remaining":     remaining,
		"apiKeyEnabled": enabledNow,
		"settings":      gatewaySettingsView(),
	})
}

func handleWebUIAPIKeyReveal(w http.ResponseWriter, r *http.Request) {
	trace := debugTraceID(r)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input apiKeyIDInput
	if err := decodeAdminJSON(w, r, &input); err != nil || strings.TrimSpace(input.ID) == "" {
		log.Printf("[模型Key管理] traceId=%s 阶段=查看明文 结果=拒绝 状态码=400 原因=缺少有效的Key id", trace)
		writeJSONError(w, http.StatusBadRequest, "查看 Key 请求必须提供 id")
		return
	}
	id := strings.TrimSpace(input.ID)
	var found gatewayAPIKey
	gatewayConfigMu.RLock()
	if idx := gatewayAPIKeyIndexOfID(gatewaySettings.APIKeys, id); idx >= 0 {
		found = gatewaySettings.APIKeys[idx]
	}
	gatewayConfigMu.RUnlock()
	if found.ID == "" {
		log.Printf("[模型Key管理] traceId=%s 阶段=查看明文 结果=拒绝 状态码=404 原因=未找到指定Key", trace)
		writeJSONError(w, http.StatusNotFound, "未找到该 Key，可能已被删除或配置已重新加载")
		return
	}
	// 明文只用于这一次显式查看，禁止任何中间层缓存；响应体不进入日志、不进入持久存储。
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	log.Printf("[模型Key管理] traceId=%s 阶段=查看明文 结果=成功 标签已设置=%t 说明=仅显式查看返回原文，原文未写入日志与持久存储", trace, found.Label != "")
	// 必须用 writeJSON 而不是 writeAdminData：后者会按字段名 apiKey 把原文脱敏掉。
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        found.ID,
		"label":     found.Label,
		"apiKey":    found.APIKey,
		"createdAt": found.CreatedAt,
	})
}
