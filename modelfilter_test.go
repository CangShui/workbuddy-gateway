package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func resetModelFilter() {
	setModelFilter(nil, nil)
}

// 黑名单：命中即禁用；未命中放行；大小写与空白不敏感。
func TestModelBlocklist(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter([]string{"  DeepSeek-V4.1-Flash ", "gpt-5.6-sol"}, nil)

	for _, model := range []string{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", "gpt-5.6-sol"} {
		disabled, reason := modelDisabled(model)
		if !disabled {
			t.Fatalf("模型 %s 应被黑名单禁用", model)
		}
		if !strings.Contains(reason, "已被网关禁用") {
			t.Fatalf("禁用原因应为中文提示, got %q", reason)
		}
	}
	if disabled, _ := modelDisabled("hy3"); disabled {
		t.Fatal("未命中黑名单的模型不应被禁用")
	}
}

// 白名单：非空时只放行列表内模型。
func TestModelAllowlist(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter(nil, []string{"deepseek-v4.1-flash"})

	if disabled, _ := modelDisabled("deepseek-v4.1-flash"); disabled {
		t.Fatal("白名单内模型应放行")
	}
	disabled, reason := modelDisabled("gpt-5.6-sol")
	if !disabled || !strings.Contains(reason, "不在白名单内") {
		t.Fatalf("白名单外模型应被禁用并给出中文原因, disabled=%v reason=%q", disabled, reason)
	}
}

// 黑白名单都未配置时不做任何限制。
func TestModelFilterDisabledByDefault(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	if modelFilterConfigured() {
		t.Fatal("默认不应启用任何名单")
	}
	if disabled, _ := modelDisabled("any-model"); disabled {
		t.Fatal("未配置名单时不应禁用任何模型")
	}
}

// config.json 的 models 段应能加载黑名单与白名单。
func TestRuntimeConfigLoadsModelFilter(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()

	dir := t.TempDir()
	path := dir + "/config.json"
	content := `{"models":{"blocklist":["blocked-model"],"allowlist":["allowed-model"]}}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if !modelFilterConfigured() {
		t.Fatal("配置应启用模型名单")
	}
	if disabled, _ := modelDisabled("blocked-model"); !disabled {
		t.Fatal("黑名单模型应被禁用")
	}
	if disabled, _ := modelDisabled("other-model"); !disabled {
		t.Fatal("白名单非空时，名单外模型应被禁用")
	}
	if disabled, _ := modelDisabled("allowed-model"); disabled {
		t.Fatal("白名单内模型应放行")
	}
}

// 被禁用模型必须从 /v1/models 列表中隐藏。
func TestMergedModelIDsHidesBlockedModels(t *testing.T) {
	resetModelsStateT(t)
	resetModelFilter()
	defer resetModelFilter()
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {
			{ID: "keep-me", FromLive: true},
			{ID: "hide-me", FromLive: true},
		},
	}
	modelsMu.Unlock()
	defer resetModelsStateT(t)

	setModelFilter([]string{"hide-me"}, nil)
	ids, _ := mergedModelIDs()
	for _, id := range ids {
		if id == "hide-me" {
			t.Fatalf("被禁用模型不应出现在模型列表中: %v", ids)
		}
	}
	found := false
	for _, id := range ids {
		if id == "keep-me" {
			found = true
		}
	}
	if !found {
		t.Fatalf("正常模型应保留: %v", ids)
	}
}

// 被禁用模型必须从 monitor 统计附表中隐藏，即使历史上被请求过。
func TestModelStatSnapshotsHideBlockedModels(t *testing.T) {
	resetModelStats()
	resetModelFilter()
	defer resetModelFilter()
	recordModelRequest("hidden-by-config")
	recordModelSuccess("hidden-by-config")

	setModelFilter([]string{"hidden-by-config"}, nil)
	rows := buildModelStatSnapshots(time.Now(), nil)
	for _, row := range rows {
		if row.ID == "hidden-by-config" {
			t.Fatalf("被禁用模型不应出现在统计附表: %+v", row)
		}
	}
}

// 端到端：请求被禁用模型时返回 403 + 中文提示，且不调用上游。
func TestBlockedModelRequestReturnsChineseError(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter([]string{"banned-model"}, nil)

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	oldBase := profileCN.Base
	oldClient := cfg.HttpClient
	profileCN.Base = upstream.URL
	cfg.HttpClient = upstream.Client()
	defer func() {
		profileCN.Base = oldBase
		cfg.HttpClient = oldClient
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "a.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
	}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"banned-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("被禁用模型应返回 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Fatal("被禁用模型不应调用上游")
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	errObj, _ := resp["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "已被网关禁用") {
		t.Fatalf("错误信息应为中文提示, got %q", msg)
	}
	if errObj["type"] != "model_disabled" {
		t.Fatalf("错误类型应为 model_disabled, got %v", errObj["type"])
	}
}

// 端到端：/v1/responses 入口同样拦截。
func TestBlockedModelResponsesEndpoint(t *testing.T) {
	resetModelFilter()
	defer resetModelFilter()
	setModelFilter(nil, []string{"only-this-model"})

	body := `{"model":"not-in-allowlist","stream":true,"input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleResponses)).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("白名单外模型应返回 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "已被网关禁用") {
		t.Fatalf("应返回中文禁用提示: %s", rec.Body.String())
	}
}
