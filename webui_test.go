package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setWebUI 临时启用/关闭管理台并在用例结束后还原，避免污染其他测试。
func setWebUI(t *testing.T, enabled bool, apiKey string) {
	t.Helper()
	oldCfg := cfg
	cfg.WebUI = enabled
	cfg.AdminKey = apiKey
	cfg.Port, cfg.WebPort = defaultAPIPort, defaultWebPort
	t.Cleanup(func() {
		cfg = oldCfg
	})
}

// 使用真实的独立管理端口中间件链，包括鉴权、来源校验及审计。
func newWebUIMux() http.Handler {
	return newWebUIHandler()
}

func TestWebUIPreflightError(t *testing.T) {
	cases := []struct {
		webui  bool
		apiKey string
		want   bool
	}{
		{false, "", false},
		{false, "k", false},
		{true, "", false}, // 没有管理 Key 时允许启动本机首次设置页面。
		{true, "k", false},
	}
	for _, c := range cases {
		setWebUI(t, c.webui, c.apiKey)
		err := webUIPreflightError()
		if (err != nil) != c.want {
			t.Fatalf("webui=%v apiKey=%q err=%v want=%v", c.webui, c.apiKey, err, c.want)
		}
	}
}

func TestWebUIDisabledRoutesAbsent(t *testing.T) {
	setWebUI(t, false, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	for _, p := range []string{"/ui/", "/ui/app.js", "/admin/api/status"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("未启用管理台时 %s 状态码=%d，期望 404", p, rec.Code)
		}
	}
}

func TestWebUIEnabledAuthAndStaticShell(t *testing.T) {
	chdirTemp(t)
	setWebUI(t, true, "webui-key")
	raw, _ := json.Marshal(statusSnapshot{UpdatedAt: time.Now().Unix()})
	if err := os.WriteFile(statusSnapshotFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	handler := newWebUIMux()

	// 静态外壳免鉴权，浏览器才能加载页面。
	for _, p := range []string{"/ui/", "/ui/app.js", "/ui/style.css"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("静态资源 %s 状态码=%d，期望 200", p, rec.Code)
		}
	}

	// 数据接口必须鉴权。
	for _, p := range []string{"/admin/api/status", "/admin/api/models", "/admin/api/logs", "/admin/api/config", "/admin/api/credentials"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("无密钥访问 %s 状态码=%d，期望 401", p, rec.Code)
		}
	}

	seen := func(p string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer webui-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("带密钥访问 %s 状态码=%d，期望 200", p, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("访问 %s 返回非 JSON: %v", p, err)
		}
		return body
	}

	status := seen("/admin/api/status")
	if _, ok := status["gateway"]; !ok {
		t.Fatal("/admin/api/status 缺少 gateway 字段")
	}
	if _, ok := status["summary"]; !ok {
		t.Fatal("/admin/api/status 缺少 summary 字段")
	}
	if _, ok := status["siteSummary"]; !ok {
		t.Fatal("/admin/api/status 缺少 siteSummary 字段")
	}
	models := seen("/admin/api/models")
	if _, ok := models["models"]; !ok {
		t.Fatal("/admin/api/models 缺少 models 字段")
	}
	seen("/admin/api/logs")
	seen("/admin/api/config")
	seen("/admin/api/credentials")
}

func TestSummarizeAccountsBySite(t *testing.T) {
	accs := []accountSnapshot{
		{Edition: "cn", State: "active"},
		{Edition: "cn", State: "cooldown"},
		{Edition: "intl", State: "disabled"},
		{Edition: "", State: "active"}, // 站点缺省按国内站处理
	}
	got := summarizeAccountsBySite(accs)
	if got["cn"]["total"] != 3 || got["cn"]["active"] != 2 || got["cn"]["cooldown"] != 1 {
		t.Fatalf("国内站汇总错误: %+v", got["cn"])
	}
	if got["intl"]["total"] != 1 || got["intl"]["disabled"] != 1 || got["intl"]["active"] != 0 {
		t.Fatalf("国际站汇总错误: %+v", got["intl"])
	}
	empty := summarizeAccountsBySite(nil)
	if empty["cn"]["total"] != 0 || empty["intl"]["total"] != 0 {
		t.Fatalf("空账号池应返回全 0: %+v", empty)
	}
}

func TestMaskToken(t *testing.T) {
	if got := maskToken(""); got != "" {
		t.Fatalf("空令牌应保持为空，得到 %q", got)
	}
	if got := maskToken("abc"); got != "****" {
		t.Fatalf("过短令牌应整体掩码，得到 %q", got)
	}
	if got := maskToken("ACCESS_TOKEN_SECRET_123456"); got != "ACCESS****" {
		t.Fatalf("掩码结果应只保留前 6 位，得到 %q", got)
	}
}

func TestWebUICredentialsMaskedAndNoPlaintext(t *testing.T) {
	chdirTemp(t)
	oldExplicit, oldDir, oldFile := cfg.AuthExplicit, cfg.AuthDir, cfg.AuthFile
	cfg.AuthExplicit = false
	cfg.AuthDir = ""
	t.Cleanup(func() {
		cfg.AuthExplicit, cfg.AuthDir, cfg.AuthFile = oldExplicit, oldDir, oldFile
	})

	const accessToken = "ACCESS_TOKEN_SECRET_123456"
	const refreshToken = "REFRESH_TOKEN_SECRET_abcdef"
	payload := `{"auth":{"accessToken":"` + accessToken + `","refreshToken":"` + refreshToken +
		`","expiresAt":1893456000,"domain":"www.codebuddy.cn"},` +
		`"account":{"uid":"uid-1","nickname":"user-a"},"edition":"cn"}`
	if err := os.WriteFile("workbuddy-a.json", []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handleWebUICredentials(rec, httptest.NewRequest(http.MethodGet, "/admin/api/credentials", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, accessToken) || strings.Contains(body, refreshToken) {
		t.Fatal("凭据响应泄露了明文令牌")
	}
	if !strings.Contains(body, "ACCESS****") || !strings.Contains(body, "REFRES****") {
		t.Fatalf("凭据响应缺少脱敏令牌前缀: %s", body)
	}
	if !strings.Contains(body, "user-a") {
		t.Fatal("凭据响应应保留非敏感字段（昵称）")
	}
}

func TestWebUILogsRejectsIllegalName(t *testing.T) {
	for _, bad := range []string{"evil.txt", "../etc/passwd", "../gateway-2026-10-06.log", "workbuddy.json"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin/api/logs?file="+bad, nil)
		handleWebUILogs(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("非法日志名 %q 状态码=%d，期望 400", bad, rec.Code)
		}
	}
}

func TestWebUILogsListsLogDir(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}
	name := "gateway-2026-10-06.log"
	if err := os.WriteFile(filepath.Join(logDir, name), []byte("line1\nline2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handleWebUILogs(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs?lines=50", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d，期望 200", rec.Code)
	}
	var resp struct {
		File  string   `json:"file"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.File != name {
		t.Fatalf("默认选中文件=%q，期望 %q", resp.File, name)
	}
	if len(resp.Lines) != 2 {
		t.Fatalf("日志行数=%d，期望 2", len(resp.Lines))
	}
}
