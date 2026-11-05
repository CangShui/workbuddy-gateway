package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// 管理台顶栏「脱敏」菜单：读写 config.json 的 redact 段，并提供不落任何原值的预览。

func redactSettingsView() map[string]any {
	o := currentRedactOptions()
	// 按配置原顺序返回词条，便于网页原样编辑。
	return map[string]any{
		"secrets":  o.secrets,
		"personal": o.personal,
		"words":    append([]string{}, o.listed...),
		"active":   o.active(),
		"stats":    redactBriefSummary(),
	}
}

func saveRedactSettings(update func(*redactFileConfig), trace string) (redactFileConfig, error) {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	doc, gateway, old, err := readGatewayDocumentLocked()
	if err == nil && (cfg.AdminKey == "" || gateway.AdminKey != cfg.AdminKey) {
		err = errConfigChange
	}
	gatewayConfigMu.Unlock()
	if err != nil {
		return redactFileConfig{}, err
	}
	var current redactFileConfig
	for key, raw := range doc {
		if strings.EqualFold(key, "redact") {
			_ = json.Unmarshal(raw, &current)
			delete(doc, key) // 大小写变体只保留一份
		}
	}
	update(&current)
	current, err = normalizeRedactConfig(current)
	if err != nil {
		return current, err
	}
	doc["redact"], _ = json.Marshal(current)
	log.Printf("[请求脱敏设置] traceId=%s 阶段=配置写入 密钥与凭据=%t 个人信息=%t 自定义词数=%d 说明=其他配置段原样保留；不记录词条正文",
		trace, current.Secrets, current.Personal, len(current.Words))
	return current, persistRuntimeDocument(doc, old, trace)
}

func handleWebUIRedact(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, redactSettingsView())
	case http.MethodPost:
		var input struct {
			Secrets  *bool     `json:"secrets"`
			Personal *bool     `json:"personal"`
			Words    *[]string `json:"words"`
		}
		if err := decodeAdminJSONLimit(w, r, &input, 128<<10); err != nil || input.Secrets == nil && input.Personal == nil && input.Words == nil {
			log.Printf("[请求脱敏设置] traceId=%s 阶段=参数校验 结果=拒绝 状态码=400", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "至少提供 secrets、personal 或 words 之一，字段类型必须正确")
			return
		}
		_, err := saveRedactSettings(func(c *redactFileConfig) {
			if input.Secrets != nil {
				c.Secrets = *input.Secrets
			}
			if input.Personal != nil {
				c.Personal = *input.Personal
			}
			if input.Words != nil {
				c.Words = *input.Words
			}
		}, debugTraceID(r))
		if err != nil {
			writeAdminOperationError(w, r, "请求脱敏设置", err)
			return
		}
		writeJSON(w, http.StatusOK, redactSettingsView())
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

func handleWebUIRedactPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input struct {
		Text *string `json:"text"`
	}
	if err := decodeAdminJSONLimit(w, r, &input, 80<<10); err != nil || input.Text == nil {
		writeJSONError(w, http.StatusBadRequest, "必须提供 text 字符串，最大约 64KB")
		return
	}
	masked, hits, off := redactPreview(*input.Text)
	if hits == nil {
		hits = []redactHit{}
	}
	// 预览不登记占位符、不写日志正文；只返回脱敏后的文本与命中类型。
	log.Printf("[请求脱敏预览] traceId=%s 字符数=%d 命中=%d 设置全关时按默认预览=%t 说明=不记录正文，预览占位符不可还原",
		debugTraceID(r), len([]rune(*input.Text)), len(hits), off)
	writeJSON(w, http.StatusOK, map[string]any{"masked": masked, "hits": hits, "off": off})
}
