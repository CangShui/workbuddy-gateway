package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolateAdminFeatures(t *testing.T) {
	t.Helper()
	isolatePolicies(t)
	oldRetention := logRetentionDays.Swap(0)
	cleanupScheduleMu.Lock()
	oldSchedule := lastLogCleanup
	lastLogCleanup.time, lastLogCleanup.days = time.Time{}, 0
	cleanupScheduleMu.Unlock()
	t.Cleanup(func() {
		logRetentionDays.Store(oldRetention)
		cleanupScheduleMu.Lock()
		lastLogCleanup = oldSchedule
		cleanupScheduleMu.Unlock()
	})
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"feature-admin-key","apiKeyEnabled":true,"apiKey":"feature-api-key"},"systemPrompt":{"force":"visible prompt text"},"models":{"accounts":{"all":{"blocklist":[]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
}

func TestEditableCredentialsScopeConflictAndSecretAudit(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthExplicit, cfg.AuthFile = true, "workbuddy-edit.json"
	const oldToken = "synthetic-editor-access-token-private"
	const refresh = "synthetic-editor-refresh-token-private"
	content := `{"auth":{"accessToken":"` + oldToken + `","refreshToken":"` + refresh + `","expiresAt":1900000000},"account":{"uid":"u1","nickname":"before"},"edition":"cn","extra":{"keep":true}}`
	if err := os.WriteFile(cfg.AuthFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadAccounts(); err != nil {
		t.Fatal(err)
	}
	accounts[0].QuotaKnown, accounts[0].QuotaRemaining = true, 123
	handler := newWebUIHandler()
	if w := adminRequest(handler, "GET", "/admin/api/credential-edit?file=workbuddy-edit.json", "feature-api-key", ""); w.Code != 401 {
		t.Fatal("模型Key不应取得明文凭据")
	}
	for _, target := range []string{"config.json", "../config.json", "/etc/passwd"} {
		w := adminRequest(handler, "GET", "/admin/api/credential-edit?file="+target, "feature-admin-key", "")
		if w.Code != 400 {
			t.Fatalf("任意路径未被拒绝: %s %d", target, w.Code)
		}
	}
	w := adminRequest(handler, "GET", "/admin/api/credential-edit?file=workbuddy-edit.json", "feature-admin-key", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), oldToken) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("受鉴权保护的明文编辑器未加载")
	}
	var view struct{ Content, Revision string }
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	edited := strings.ReplaceAll(strings.ReplaceAll(view.Content, oldToken, "synthetic-edited-new-access-token"), "before", "after")
	body, _ := json.Marshal(map[string]string{"path": cfg.AuthFile, "revision": view.Revision, "content": edited})
	w = adminRequest(handler, "POST", "/admin/api/credential-edit", "feature-admin-key", string(body))
	if w.Code != 200 {
		t.Fatalf("编辑保存失败: %d %s", w.Code, w.Body.String())
	}
	raw, _ := os.ReadFile(cfg.AuthFile)
	if string(raw) != edited || accounts[0].Auth.Account.Nickname != "after" || accounts[0].QuotaRemaining != 123 {
		t.Fatal("文件或运行时凭据没有更新，或丢失了额度账本")
	}
	w = adminRequest(handler, "POST", "/admin/api/credential-edit", "feature-admin-key", string(body))
	if w.Code != 409 {
		t.Fatal("旧编辑版本未被拒绝，可能覆盖后台续期")
	}
	w = adminRequest(handler, "GET", "/admin/api/credentials", "feature-admin-key", "")
	if strings.Contains(w.Body.String(), "synthetic-edited-new-access-token") || strings.Contains(w.Body.String(), refresh) {
		t.Fatal("凭据列表泄露原文")
	}
	for _, file := range listLogFiles() {
		logData, _ := os.ReadFile(filepath.Join(logDir, file.Name))
		for _, secret := range []string{oldToken, refresh, "synthetic-edited-new-access-token"} {
			if strings.Contains(string(logData), secret) {
				t.Fatal("明文凭据被记录进日志")
			}
		}
	}
	for _, bad := range []string{`null`, `{`, `{"auth":{"accessToken":""}}`, `{"auth":{"accessToken":"ACCESS****"}}`, `{"auth":{"accessToken":"x"},"edition":"bad"}`} {
		body, _ = json.Marshal(map[string]string{"path": cfg.AuthFile, "revision": credentialRevision(raw), "content": bad})
		if w := adminRequest(handler, "POST", "/admin/api/credential-edit", "feature-admin-key", string(body)); w.Code != 400 {
			t.Fatalf("接受了非法凭据: %s", bad)
		}
		after, _ := os.ReadFile(cfg.AuthFile)
		if string(after) != string(raw) {
			t.Fatal("非法编辑改变了文件")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := saveEditedCredential(ctx, cfg.AuthFile, credentialRevision(raw), edited, "cancel"); err == nil {
		t.Fatal("取消的请求不应保存凭据")
	}
}

func TestCredentialEditRejectsSymlink(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthExplicit, cfg.AuthFile = true, "workbuddy-link.json"
	if err := os.WriteFile("source.json", []byte(`{"auth":{"accessToken":"synthetic"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source.json", cfg.AuthFile); err != nil {
		t.Skip("本机无法创建符号链接")
	}
	w := adminRequest(newWebUIHandler(), "GET", "/admin/api/credential-edit?file="+cfg.AuthFile, "feature-admin-key", "")
	if w.Code != 400 {
		t.Fatal("符号链接凭据不能通过网页编辑")
	}
}

func TestCredentialEditorRestoresInvalidFileAndResetsNewIdentity(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthExplicit, cfg.AuthFile, cfg.ReloadInterval = true, "workbuddy-fixed.json", 0
	accounts = nil
	if err := os.WriteFile(cfg.AuthFile, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	content := `{"auth":{"accessToken":"synthetic-restored-token"},"account":{"uid":"one"},"edition":"cn"}`
	if err := saveEditedCredential(context.Background(), cfg.AuthFile, credentialRevision([]byte(`{broken`)), content, "restore"); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Auth.Account.UID != "one" {
		t.Fatal("修复凭据时未立即加入运行账号池")
	}
	accounts[0].QuotaKnown, accounts[0].QuotaRemaining = true, 1000
	accounts[0].ModelStates = map[string]*modelRuntimeState{"m": {CostClass: modelCostFree}}
	changed := strings.ReplaceAll(content, `"one"`, `"two"`)
	if err := saveEditedCredential(context.Background(), cfg.AuthFile, credentialRevision([]byte(content)), changed, "identity"); err != nil {
		t.Fatal(err)
	}
	if accounts[0].QuotaKnown || accounts[0].QuotaRemaining != 0 || len(accounts[0].ModelStates) != 0 {
		t.Fatal("新账号身份错误地沿用了旧账号余额/免费账本")
	}
}

func TestPortSettingsSaveAndPendingRestart(t *testing.T) {
	isolateAdminFeatures(t)
	actualAPI, actualWeb := cfg.Port, cfg.WebPort
	handler := newWebUIHandler()
	w := adminRequest(handler, "POST", "/admin/api/settings", "feature-admin-key", `{"apiPort":9317,"webPort":9316}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if cfg.Port != actualAPI || cfg.WebPort != actualWeb || view["configuredApiPort"] != float64(9317) || view["restartRequired"] != true {
		t.Fatal("保存端口改变了实际监听或未标明需重启")
	}
	raw, _ := os.ReadFile(runtimeConfigFile)
	var c runtimeFileConfig
	_ = json.Unmarshal(raw, &c)
	if c.Gateway.APIPort != 9317 || c.Gateway.WebPort != 9316 || c.SystemPrompt.Force != "visible prompt text" || currentAPIKey() != "feature-api-key" {
		t.Fatal("端口未保存或改变了其他配置")
	}
	for _, body := range []string{`{"apiPort":70000}`, `{"apiPort":9316}`, `{"webPort":-1}`, `{"webPort":1.5}`} {
		if w := adminRequest(handler, "POST", "/admin/api/settings", "feature-admin-key", body); w.Code != 400 {
			t.Fatalf("非法端口被接受: %s %d", body, w.Code)
		}
	}
	cfg.PortExplicit = true
	if w := adminRequest(handler, "POST", "/admin/api/settings", "feature-admin-key", `{"webPort":8317}`); w.Code != 400 {
		t.Fatal("-port 覆盖导致的端口冲突未被拒绝")
	}
	w = adminRequest(handler, "GET", "/admin/api/config", "feature-admin-key", "")
	if !strings.Contains(w.Body.String(), "visible prompt text") || strings.Contains(w.Body.String(), "feature-api-key") {
		t.Fatal("提示词应可见，Key仍应隐藏")
	}
	html, _ := webFS.ReadFile("web/index.html")
	js, _ := webFS.ReadFile("web/app.js")
	if strings.Contains(string(html), "maskHints") || strings.Contains(string(js), "maskSystemPrompt") {
		t.Fatal("隐藏systemPrompt功能未移除")
	}
}

func TestLogRetentionBoundaryAndSafeFiles(t *testing.T) {
	isolateAdminFeatures(t)
	now := time.Now()
	old := now.AddDate(0, 0, -7).Format("2006-01-02")
	boundary := now.AddDate(0, 0, -6).Format("2006-01-02")
	deleteNames := []string{"gateway-" + old + ".log", "debug-" + old + ".jsonl", "deployment-" + old + ".jsonl"}
	keepNames := []string{"gateway-" + boundary + ".log", "gateway-" + now.Format("2006-01-02") + ".log", "gateway-2099-01-01.log", "gateway-2026-02-30.log", "workbuddy-a.json", "report.md", "random-2000-01-01.log"}
	for _, name := range append(deleteNames, keepNames...) {
		if err := os.WriteFile(filepath.Join(logDir, name), []byte("fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	directory := "gateway-2000-01-01.log"
	if err := os.Mkdir(filepath.Join(logDir, directory), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := cleanupLogs(now, 0, "off")
	if err != nil || result.Deleted != 0 {
		t.Fatal("0 应保留全部")
	}
	result, err = cleanupLogs(now, 7, "boundary")
	if err != nil || result.Deleted != 3 {
		t.Fatalf("清理数量错误: %+v %v", result, err)
	}
	for _, name := range deleteNames {
		if _, err := os.Stat(filepath.Join(logDir, name)); !os.IsNotExist(err) {
			t.Fatalf("过期日志未删除: %s", name)
		}
	}
	for _, name := range append(keepNames, directory) {
		if _, err := os.Stat(filepath.Join(logDir, name)); err != nil {
			t.Fatalf("清理误删其他文件: %s", name)
		}
	}
}

func TestDailyLogRotationAndActiveProtection(t *testing.T) {
	isolateAdminFeatures(t)
	writer, err := newDailyLogWriter("debug", ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	yesterday := time.Now().AddDate(0, 0, -1)
	writer.mu.Lock()
	err = writer.rotate(yesterday)
	oldPath := writer.path
	writer.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	result, err := cleanupLogs(time.Now(), 1, "active")
	if err != nil || result.SkippedActive == 0 {
		t.Fatal("不应删除正在写入的旧日志")
	}
	if _, err := writer.Write([]byte("new day event\n")); err != nil {
		t.Fatal(err)
	}
	if writer.path == oldPath {
		t.Fatal("跨午夜写入未切换日期")
	}
	if result, err := cleanupLogs(time.Now(), 1, "rotated"); err != nil || result.Deleted != 1 {
		t.Fatalf("轮转后旧日志未回收: %+v %v", result, err)
	}
}

func TestLogSettingsPreserveConfigAndHotReload(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	for _, body := range []string{`{}`, `{"retentionDays":-1}`, `{"retentionDays":1.5}`, `{"retentionDays":36501}`} {
		if w := adminRequest(handler, "POST", "/admin/api/log-settings", "feature-admin-key", body); w.Code != 400 {
			t.Fatalf("非法保留天数被接受: %s", body)
		}
	}
	w := adminRequest(handler, "POST", "/admin/api/log-settings", "feature-admin-key", `{"retentionDays":7}`)
	if w.Code != 200 || logRetentionDays.Load() != 7 {
		t.Fatalf("保留设置未生效: %d %s", w.Code, w.Body.String())
	}
	raw, _ := os.ReadFile(runtimeConfigFile)
	var c runtimeFileConfig
	_ = json.Unmarshal(raw, &c)
	if c.Logs.RetentionDays != 7 || c.SystemPrompt.Force != "visible prompt text" || currentAPIKey() != "feature-api-key" {
		t.Fatal("保留设置覆盖了其他配置")
	}
	oldName := "gateway-" + time.Now().AddDate(0, 0, -8).Format("2006-01-02") + ".log"
	_ = os.WriteFile(filepath.Join(logDir, oldName), []byte("expired"), 0600)
	w = adminRequest(handler, "POST", "/admin/api/log-cleanup", "feature-admin-key", `{}`)
	if w.Code != 200 {
		t.Fatalf("清理失败: %s", w.Body.String())
	}
	if w := adminRequest(handler, "POST", "/admin/api/log-cleanup", "feature-admin-key", `null`); w.Code != 400 {
		t.Fatal("清理请求必须显式提交空对象，不能为 null")
	}
	if _, err := os.Stat(filepath.Join(logDir, oldName)); !os.IsNotExist(err) {
		t.Fatal("手动清理没有删除过期日志")
	}
	if w := adminRequest(handler, "POST", "/admin/api/log-cleanup", "feature-api-key", `{}`); w.Code != 401 {
		t.Fatal("模型Key不应能清理日志")
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"feature-admin-key"},"logs":{"retentionDays":3}}`)
	if !(new(runtimeConfigWatcher)).reload() || logRetentionDays.Load() != 3 {
		t.Fatal("文件编辑的日志保留配置未热加载")
	}
	_ = os.WriteFile(filepath.Join(logDir, oldName), []byte("expired"), 0600)
	scheduledLogCleanup(time.Now())
	if _, err := os.Stat(filepath.Join(logDir, oldName)); !os.IsNotExist(err) {
		t.Fatal("自动清理没有应用新规则")
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"feature-admin-key"}}`)
	if !(new(runtimeConfigWatcher)).reload() || logRetentionDays.Load() != 0 {
		t.Fatal("旧配置缺省字段应回到不自动清理")
	}
	r := httptest.NewRequest(http.MethodPost, "http://localhost/admin/api/log-cleanup", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer feature-admin-key")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Workbuddy-Admin", "1")
	r.Header.Set("Origin", "https://foreign.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatal("跨站日志清理未被拦截")
	}
}
