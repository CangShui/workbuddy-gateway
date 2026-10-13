package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func editorRequestBody(t *testing.T, revision, content string, removal bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"revision": revision, "content": content, "acknowledged": true, "confirmAdminKeyRemoval": removal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestConfigEditorRequiresAdminAndWarningAcknowledgement(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	for _, key := range []string{"", "feature-api-key"} {
		for _, method := range []string{"GET", "POST"} {
			w := adminRequest(handler, method, "/admin/api/config-edit?acknowledged=1", key, "{}")
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "feature-admin-key") {
				t.Fatalf("配置编辑绕过管理鉴权: %s %d", method, w.Code)
			}
		}
	}
	for _, method := range []string{"GET", "POST"} {
		w := adminRequest(handler, method, "/admin/api/config-edit", "feature-admin-key", "{}")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("没有风险确认应拒绝: %s %d", method, w.Code)
		}
	}
	w := adminRequest(handler, "GET", "/admin/api/config", "feature-admin-key", "")
	if strings.Contains(w.Body.String(), "feature-admin-key") || strings.Contains(w.Body.String(), "feature-api-key") {
		t.Fatal("常规预览暴露了真实Key")
	}
	w = adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "feature-admin-key", "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(w.Body.String(), "feature-admin-key") || !strings.Contains(w.Body.String(), "visible prompt text") {
		t.Fatalf("确认后没有取得真实配置: %d", w.Code)
	}
	var view struct{ Content, Revision string }
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	onDisk, _ := os.ReadFile(runtimeConfigFile)
	if view.Content != string(onDisk) || view.Revision != configRevision(onDisk) {
		t.Fatal("编辑器必须使用原文，不能使用脱敏预览")
	}
	methodRequest := httptest.NewRequest(http.MethodPut, "/admin/api/config-edit", strings.NewReader("{}"))
	methodRequest.Header.Set("Authorization", "Bearer feature-admin-key")
	methodRequest.Header.Set("Content-Type", "application/json")
	methodRequest.Header.Set("X-Workbuddy-Admin", "1")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, methodRequest)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("配置编辑未限制方法")
	}
	crossOrigin := httptest.NewRequest(http.MethodGet, "/admin/api/config-edit?acknowledged=1", nil)
	crossOrigin.Header.Set("Authorization", "Bearer feature-admin-key")
	crossOrigin.Header.Set("Origin", "https://untrusted.invalid")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, crossOrigin)
	if w.Code != http.StatusForbidden {
		t.Fatal("明文配置编辑接口接受跨站来源")
	}
	// 锁内二次鉴权也必须拒绝旧会话（单独验证 handler，不依赖外层中间件）。
	req := httptest.NewRequest(http.MethodGet, "/admin/api/config-edit?acknowledged=1", nil)
	req.Header.Set("Authorization", "Bearer stale-key")
	w = httptest.NewRecorder()
	handleWebUIConfigEdit(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("锁内未再次校验会话")
	}
}

func TestConfigEditorSavesHotSettingsPortsAndSecretSafeAudit(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	oldWriter := log.Writer()
	var audit bytes.Buffer
	log.SetOutput(&redactingLogWriter{writer: io.MultiWriter(oldWriter, &audit)})
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	const prompt = "synthetic-private-editor-prompt"
	const newAPIKey = "synthetic-new-editor-api-key"
	content := `{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":true,"apiKey":"` + newAPIKey + `","apiPort":18317,"webPort":18316},"systemPrompt":{"force":"` + prompt + `","fallback":"fallback"},"models":{"blocklist":["off"],"accounts":{"all":{"blocklist":["paused.json"]}}},"logs":{"retentionDays":5},"upstream":{"idleTimeoutSeconds":77,"transientRetries":0},"debug":{"enabled":false}}`
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
	if w.Code != http.StatusOK {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Revision        string
		AdminKeyChanged bool
		Settings        struct{ RestartRequired bool }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(runtimeConfigFile)
	if response.Revision != configRevision(saved) || !response.Settings.RestartRequired || response.AdminKeyChanged {
		t.Fatal("保存状态、版本或待重启状态错误")
	}
	if cfg.Port != defaultAPIPort || cfg.WebPort != defaultWebPort || currentAPIKey() != newAPIKey ||
		configuredSystemPrompts().force != prompt || logRetentionDays.Load() != 5 {
		t.Fatal("热配置未生效或监听端口被错误地热切换")
	}
	if disabled, _ := modelDisabled("off"); !disabled {
		t.Fatal("模型名单未生效")
	}
	if allowed, _ := modelAccountAllowed("any", &Account{Path: "paused.json"}, nil); allowed {
		t.Fatal("全局账号名单未生效")
	}
	_, idle, retries := upstreamSettings()
	if idle != 77*time.Second || retries != 0 {
		t.Fatal("上游配置未生效")
	}
	watcher := runtimeConfigWatcher{}
	if watcher.reload() {
		t.Fatal("保存摘要未更新，导致重复热加载")
	}
	for _, secret := range []string{"feature-admin-key", newAPIKey, prompt, "fallback"} {
		if strings.Contains(audit.String(), secret) || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("编辑保存日志或响应泄漏配置正文: %q", secret)
		}
	}
	for _, marker := range []string{"[请求到达]", "[管理来源校验]", "[管理鉴权]", "[配置编辑]", "[配置热加载]", "[响应返回]"} {
		if !strings.Contains(audit.String(), marker) {
			t.Errorf("缺少完整链路审计 %s", marker)
		}
	}
}

func TestConfigEditorRejectsInvalidDocumentsWithoutMutation(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	for _, content := range []string{
		"", "{", "null", "[]", `{} {}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiPort":"8317"}}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiPort":8316,"webPort":8316}}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":true}}`,
		`{"gateway":{"adminKey":"[REDACTED]"}}`,
		`{"gateway":{"adminKey":"feature-admin-key","apiKey":"[REDACTED]"}}`,
		`{"models":{"allowlist":["one"],"blocklist":["two"]}}`,
		`{"models":{"accounts":{"all":{"blocklist":["../other.json"]}}}}`,
		`{"logs":{"retentionDays":-1}}`,
		`{"synthetic-secret-unknown-field":"secret-body"}`,
		strings.Repeat("x", configEditBudget+1),
	} {
		w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("无效配置未拒绝: 字节=%d HTTP=%d", len(content), w.Code)
		}
		after, _ := os.ReadFile(runtimeConfigFile)
		if !bytes.Equal(after, old) || currentAdminKey() != "feature-admin-key" || currentAPIKey() != "feature-api-key" {
			t.Fatal("无效配置修改了磁盘或运行时")
		}
		if strings.Contains(w.Body.String(), "synthetic-secret-unknown-field") {
			t.Fatal("校验错误泄漏用户正文")
		}
	}
	for _, body := range []string{`{`, `{"acknowledged":true,"path":"../other.json"}`, `{"acknowledged":true,"content":false}`} {
		if w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", body); w.Code != 400 {
			t.Fatal("未知表单字段或类型未拒绝")
		}
	}
}

func TestConfigEditorRevisionConflictAndExternalKeyChange(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	if err := saveAPIKeySettings(false, "", "other-page"); err != nil {
		t.Fatal(err)
	}
	changed, _ := os.ReadFile(runtimeConfigFile)
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), string(old), false))
	if w.Code != http.StatusConflict {
		t.Fatal("覆盖其他页面更新未被阻止")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(after, changed) {
		t.Fatal("版本冲突覆盖了新配置")
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"changed-before-reload"}}`)
	changed, _ = os.ReadFile(runtimeConfigFile)
	for _, method := range []string{"GET", "POST"} {
		w = adminRequest(handler, method, "/admin/api/config-edit?acknowledged=1", "feature-admin-key", editorRequestBody(t, configRevision(changed), string(old), false))
		if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "changed-before-reload") {
			t.Fatal("旧会话覆盖或读取了尚未热加载的新管理Key")
		}
	}
}

func TestConfigEditorAdminKeyChangeAndRemovalConfirmation(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	content := strings.ReplaceAll(string(old), "feature-admin-key", "synthetic-new-editor-admin-key")
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"adminKeyChanged":true`) {
		t.Fatal("管理Key变化未告知客户端")
	}
	if adminRequest(handler, "GET", "/admin/api/config", "feature-admin-key", "").Code != 401 {
		t.Fatal("旧会话仍然有效")
	}
	old, _ = os.ReadFile(runtimeConfigFile)
	w = adminRequest(handler, "POST", "/admin/api/config-edit", "synthetic-new-editor-admin-key", editorRequestBody(t, configRevision(old), `{}`, false))
	if w.Code != 400 || currentAdminKey() == "" {
		t.Fatal("未经额外确认允许清空管理Key")
	}
	w = adminRequest(handler, "POST", "/admin/api/config-edit", "synthetic-new-editor-admin-key", editorRequestBody(t, configRevision(old), `{}`, true))
	if w.Code != 200 || currentAdminKey() != "" || !strings.Contains(w.Body.String(), `"adminKeyCleared":true`) {
		t.Fatal("确认清空管理Key没有生效")
	}
	if adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "synthetic-new-editor-admin-key", "").Code != 401 {
		t.Fatal("清空管理Key意外放开管理鉴权")
	}
}

func TestConfigEditorApplyFailureRestoresDiskAndRuntime(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	old, _ := os.ReadFile(runtimeConfigFile)
	// debug 文件不可创建时，完整配置热应用必须失败并恢复旧文件。
	if err := os.Mkdir(filepath.Join(logDir, "debug-"+time.Now().Format("2006-01-02")+".jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	content := `{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":false},"debug":{"enabled":true},"models":{"blocklist":["off"]}}`
	w := adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision(old), content, false))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("应用失败未返回错误: %d", w.Code)
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(old, after) || currentAPIKey() != "feature-api-key" || debugLoggingEnabled() {
		t.Fatal("失败没有恢复原配置")
	}
	if disabled, _ := modelDisabled("off"); disabled {
		t.Fatal("失败部分应用了模型名单")
	}
}

func TestConfigEditorCanRepairInvalidCurrentFileAndRejectsOversizeRead(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	writeRuntimeFixture(t, `{`)
	w := adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "feature-admin-key", "")
	if w.Code != 200 {
		t.Fatal("管理员无法加载损坏文件进行修复")
	}
	w = adminRequest(handler, "POST", "/admin/api/config-edit", "feature-admin-key", editorRequestBody(t, configRevision([]byte("{")), `{"gateway":{"adminKey":"feature-admin-key"}}`, false))
	if w.Code != 200 {
		t.Fatalf("损坏配置修复失败: %d", w.Code)
	}
	writeRuntimeFixture(t, strings.Repeat("x", configEditBudget+1))
	if adminRequest(handler, "GET", "/admin/api/config-edit?acknowledged=1", "feature-admin-key", "").Code != 400 {
		t.Fatal("超出读取预算的配置未拒绝")
	}
}

func TestConfigEditorRejectsSymlink(t *testing.T) {
	isolateAdminFeatures(t)
	old, _ := os.ReadFile(runtimeConfigFile)
	if err := os.WriteFile("other.json", old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.json", "test-link.json"); err != nil {
		t.Skip("系统未允许创建符号链接")
	}
	if err := os.Remove(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename("test-link.json", runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	handler := newWebUIHandler()
	for _, method := range []string{"GET", "POST"} {
		w := adminRequest(handler, method, "/admin/api/config-edit?acknowledged=1", "feature-admin-key", editorRequestBody(t, configRevision(old), `{}`, true))
		if w.Code != 400 {
			t.Fatal("网页编辑错误地跟随了配置符号链接")
		}
	}
	after, _ := os.ReadFile("other.json")
	if !bytes.Equal(after, old) {
		t.Fatal("网页编辑改写了非配置目标")
	}
}
