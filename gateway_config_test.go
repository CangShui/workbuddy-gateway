package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func isolatedGatewayConfig(t *testing.T) {
	t.Helper()
	chdirTemp(t)
	oldCfg := cfg
	gatewayConfigMu.Lock()
	oldSettings := gatewaySettings
	gatewayConfigMu.Unlock()
	oldPrompts := configuredSystemPrompts()
	oldWriter := log.Writer()
	cfg = Config{Addr: "127.0.0.1", WebUI: true, AuthFile: "workbuddy.json"}
	if err := applyGatewayConfig(gatewayFileConfig{}); err != nil {
		t.Fatal(err)
	}
	closeLog := initFileLogging("gateway-config-test")
	t.Cleanup(func() {
		closeLog()
		log.SetOutput(oldWriter)
		gatewayConfigMu.Lock()
		cfg, gatewaySettings = oldCfg, oldSettings
		gatewayConfigMu.Unlock()
		setSystemPromptConfig(oldPrompts.fallback, oldPrompts.force)
	})
}

func adminRequest(handler http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Trace-ID", "gateway-config-test")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Workbuddy-Admin", "1")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func initTestAdmin(t *testing.T, handler http.Handler) string {
	t.Helper()
	w := adminRequest(handler, http.MethodPost, "/admin/api/setup", "", "{}")
	if w.Code != http.StatusCreated {
		t.Fatalf("setup status=%d", w.Code)
	}
	var body struct {
		Key string `json:"adminKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(body.Key) {
		t.Fatal("generated admin key must have exactly 32 random hex characters")
	}
	return body.Key
}

func TestGatewayConfigPortsAndValidation(t *testing.T) {
	isolatedGatewayConfig(t)
	if cfg.Port != 8317 || cfg.WebPort != 8316 || currentAPIKey() != "" || currentAdminKey() != "" {
		t.Fatal("default ports/auth do not match the requested defaults")
	}
	for _, g := range []gatewayFileConfig{
		{APIPort: -1}, {WebPort: 65536}, {APIPort: 9200, WebPort: 9200},
		{APIKeyEnabled: true}, {AdminKey: " has space"}, {APIKey: "newline\nkey"},
	} {
		if _, err := normalizeGatewayConfig(g); err == nil {
			t.Fatal("invalid gateway config accepted")
		}
	}
	content := `{"gateway":{"apiPort":9317,"webPort":9316,"adminKey":"manual-admin","apiKeyEnabled":true,"apiKey":"manual-api"}}`
	if err := os.WriteFile(runtimeConfigFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9317 || cfg.WebPort != 9316 || currentAPIKey() != "manual-api" || currentAdminKey() != "manual-admin" {
		t.Fatal("gateway config was not loaded")
	}
	cfg.PortExplicit, cfg.Port = true, 9417
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil || cfg.Port != 9417 || cfg.WebPort != 9316 {
		t.Fatal("explicit legacy -port override must not override webPort")
	}
}

func TestGatewayAdminSetupOnceAndPreservesConfig(t *testing.T) {
	isolatedGatewayConfig(t)
	const original = `{"systemPrompt":{"force":"synthetic retained prompt"},"models":{"accounts":{"m":{"allowlist":["workbuddy-a.json"]}}},"gateway":{"apiPort":9317,"webPort":9316}}`
	if err := os.WriteFile(runtimeConfigFile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	handler := newWebUIHandler()
	key := initTestAdmin(t, handler)
	onDisk, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var parsed runtimeFileConfig
	if err := json.Unmarshal(onDisk, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Gateway.AdminKey != key || currentAdminKey() != key {
		t.Fatal("successful setup must persist the same plaintext key before updating memory")
	}
	if parsed.SystemPrompt.Force != "synthetic retained prompt" ||
		len(parsed.Models.Accounts["m"].Allowlist) != 1 ||
		parsed.Gateway.APIPort != 9317 || parsed.Gateway.WebPort != 9316 {
		t.Fatal("setup changed unrelated config")
	}
	w := adminRequest(handler, http.MethodPost, "/admin/api/setup", "", "{}")
	if w.Code != 409 || strings.Contains(w.Body.String(), key) {
		t.Fatal("repeat setup must neither rotate nor reveal the existing key")
	}
	for _, route := range []string{"/admin/api/config", "/admin/api/settings", "/admin/api/setup"} {
		w := adminRequest(handler, http.MethodGet, route, key, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), key) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("key disclosure or cache issue at %s", route)
		}
	}
	logData, err := os.ReadFile(filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(logData, []byte(key)) {
		t.Fatal("admin key leaked into audit logs")
	}
}

func TestGatewaySetupRejectsRemoteOriginAndMalformedInput(t *testing.T) {
	for _, kind := range []string{"remote", "rebind-host", "foreign-origin", "proxy", "no-marker", "form", "null", "extra-field"} {
		t.Run(kind, func(t *testing.T) {
			isolatedGatewayConfig(t)
			body := "{}"
			if kind == "null" {
				body = "null"
			}
			if kind == "extra-field" {
				body = `{"adminKey":"attacker-value"}`
			}
			r := httptest.NewRequest(http.MethodPost, "http://localhost/admin/api/setup", strings.NewReader(body))
			r.RemoteAddr = "127.0.0.1:12345"
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Workbuddy-Admin", "1")
			switch kind {
			case "remote":
				r.RemoteAddr = "192.0.2.10:1000"
			case "rebind-host":
				r.Host = "attacker.example"
			case "foreign-origin":
				r.Header.Set("Origin", "https://attacker.example")
			case "proxy":
				r.Header.Set("X-Forwarded-For", "192.0.2.10")
			case "no-marker":
				r.Header.Del("X-Workbuddy-Admin")
			case "form":
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			w := httptest.NewRecorder()
			newWebUIHandler().ServeHTTP(w, r)
			if w.Code != 400 && w.Code != 403 {
				t.Fatalf("unsafe bootstrap request accepted: status=%d", w.Code)
			}
			// 只有「首次生成管理 Key」这条拒绝提示需要给出全部可操作路径；
			// 同源/Content-Type 等中间件拒绝各有自己的原因，不在此断言范围内。
			respBody := w.Body.String()
			if strings.Contains(respBody, "首次自动生成管理 Key") {
				for _, hint := range []string{"SSH", "config.json", "gateway.adminKey"} {
					if !strings.Contains(respBody, hint) {
						t.Fatalf("拒绝提示缺少可操作路径 %q: %s", hint, respBody)
					}
				}
			}
			if currentAdminKey() != "" {
				t.Fatal("rejected request changed admin key")
			}
			if _, err := os.Stat(runtimeConfigFile); !os.IsNotExist(err) {
				t.Fatal("rejected request wrote config")
			}
		})
	}
}

func TestGatewayConcurrentSetupHasOneWinner(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			w := adminRequest(handler, http.MethodPost, "/admin/api/setup", "", "{}")
			switch w.Code {
			case 201:
				wins.Add(1)
			case 409:
			default:
				t.Errorf("unexpected concurrent setup status=%d", w.Code)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("setup winners=%d", wins.Load())
	}
}

func TestGatewayBootstrapSaveFailureKeepsMemoryEmpty(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := os.Mkdir(runtimeConfigFile, 0700); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(newWebUIHandler(), http.MethodPost, "/admin/api/setup", "", "{}")
	if w.Code != 500 || currentAdminKey() != "" {
		t.Fatalf("failed setup status=%d switched_memory=%v", w.Code, currentAdminKey() != "")
	}
}

func TestGatewayAPIKeyToggleAndRoleSeparation(t *testing.T) {
	isolatedGatewayConfig(t)
	web := newWebUIHandler()
	admin := initTestAdmin(t, web)
	api := newAPIHandler()
	apiStatus := func(key string) int {
		return adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", key, `{"messages":[]}`).Code
	}
	if apiStatus("") != 200 || apiStatus("arbitrary-client-key") != 200 {
		t.Fatal("disabled API authentication must accept no key or any key")
	}
	for _, bad := range []string{"", "synthetic-api-key"} {
		if adminRequest(web, http.MethodGet, "/admin/api/settings", bad, "").Code != 401 {
			t.Fatal("API auth disabled must not disable admin auth")
		}
	}
	if adminRequest(web, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true}`).Code != 400 {
		t.Fatal("enabling API authentication without a key must fail")
	}
	w := adminRequest(web, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true,"apiKey":"synthetic-api-key"}`)
	if w.Code != 200 || currentAPIKey() != "synthetic-api-key" {
		t.Fatal("API key change was not applied")
	}
	if apiStatus("") != 401 || apiStatus(admin) != 401 || apiStatus("synthetic-api-key") != 200 {
		t.Fatal("enabled API authentication or role separation failed")
	}
	if adminRequest(web, http.MethodGet, "/admin/api/settings", "synthetic-api-key", "").Code != 401 {
		t.Fatal("API key granted admin access")
	}
	if w := adminRequest(web, http.MethodGet, "/admin/api/config", admin, ""); strings.Contains(w.Body.String(), "synthetic-api-key") || strings.Contains(w.Body.String(), admin) {
		t.Fatal("config response leaked either key")
	}
	if adminRequest(web, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":false}`).Code != 200 {
		t.Fatal("disabling API auth failed")
	}
	if apiStatus("") != 200 || apiStatus("another-random-key") != 200 || currentAdminKey() != admin {
		t.Fatal("disabling API auth must leave independent admin auth intact")
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil || currentAPIKey() != "" || currentAdminKey() != admin {
		t.Fatal("persisted auth state was not restored correctly")
	}
	if adminRequest(web, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true}`).Code != 200 || apiStatus("synthetic-api-key") != 200 {
		t.Fatal("stored API key should survive the disabled state")
	}
	if adminRequest(api, http.MethodGet, "/admin/api/settings", "synthetic-api-key", "").Code != 404 ||
		adminRequest(api, http.MethodGet, "/ui/", "synthetic-api-key", "").Code != 404 ||
		adminRequest(web, http.MethodPost, "/v1/messages/count_tokens", admin, `{"messages":[]}`).Code != 404 {
		t.Fatal("API and management routes were not isolated")
	}
}

func TestGatewaySettingsFailurePreservesActiveKey(t *testing.T) {
	isolatedGatewayConfig(t)
	web := newWebUIHandler()
	admin := initTestAdmin(t, web)
	if err := saveAPIKeySettings(true, "old-synthetic-key", "test"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["gateway"].(map[string]any)["adminKey"] = "externally-replaced-admin"
	changed, _ := json.Marshal(doc)
	if err := os.WriteFile(runtimeConfigFile, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if adminRequest(web, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":false}`).Code != 409 {
		t.Fatal("external admin-key edits must not be overwritten")
	}
	if currentAPIKey() != "old-synthetic-key" || currentAdminKey() != admin {
		t.Fatal("failed save altered active keys")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(after, changed) {
		t.Fatal("failed update modified externally edited config")
	}
}

func TestGatewaySplitListenersAndCollisionCleanup(t *testing.T) {
	isolatedGatewayConfig(t)
	cfg.Port, cfg.WebPort = 0, 0
	cfg.WebUI = false
	servers, listeners, err := prepareGatewayServers()
	if err != nil || len(servers) != 1 || len(listeners) != 1 {
		t.Fatal("without -webui only API listener should be created")
	}
	apiAddress := listeners[0].Addr().String()
	listeners[0].Close()
	apiListener, err := net.Listen("tcp", apiAddress)
	if err != nil {
		t.Fatal(err)
	}
	_, apiPort, _ := net.SplitHostPort(apiListener.Addr().String())
	apiListener.Close()
	webListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer webListener.Close()
	cfg.Port = portNumber(t, apiPort)
	cfg.WebPort = webListener.Addr().(*net.TCPAddr).Port
	cfg.WebUI = true
	if _, _, err := prepareGatewayServers(); err == nil {
		t.Fatal("occupied web port must fail startup")
	}
	rebound, err := net.Listen("tcp", apiAddress)
	if err != nil {
		t.Fatal("failed web binding left API port occupied")
	}
	rebound.Close()
}

func portNumber(t *testing.T, text string) int {
	t.Helper()
	var port int
	if err := json.Unmarshal([]byte(text), &port); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestWebUIFixedLogRedactionBudgetAndSnapshotFailure(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)
	file := filepath.Join(logDir, "gateway-2020-01-01.log")
	const secret = "SYNTHETIC_OLD_ACCESS_TOKEN"
	if err := os.WriteFile(file, []byte("code=12153 reason=denied traceId=old access_token="+secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(handler, http.MethodGet, "/admin/api/logs?file=gateway-2020-01-01.log", admin, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), "12153") {
		t.Fatal("legacy logs were not selectively redacted")
	}
	original, _ := os.ReadFile(file)
	if !bytes.Contains(original, []byte(secret)) {
		t.Fatal("display redaction modified the original log file")
	}
	long := strings.Repeat("A", int(webUILogReadBudget)+100) + "\nend\n"
	if err := os.WriteFile(file, []byte(long), 0600); err != nil {
		t.Fatal(err)
	}
	w = adminRequest(handler, http.MethodGet, "/admin/api/logs?file=gateway-2020-01-01.log", admin, "")
	var body struct {
		Truncated bool     `json:"truncated"`
		Lines     []string `json:"lines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Truncated || len(body.Lines) != 1 || body.Lines[0] != "end" {
		t.Fatal("over-budget partial line must be omitted, keeping only complete lines")
	}
	if err := os.WriteFile(statusSnapshotFile, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/api/status", "/admin/api/models"} {
		if w := adminRequest(handler, http.MethodGet, path, admin, ""); w.Code != 503 {
			t.Fatalf("snapshot parse failure at %s returned %d", path, w.Code)
		}
	}
}

func TestWebUIConfigAndStatusSecretCopies(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)
	snap := statusSnapshot{UpdatedAt: time.Now().Add(-time.Minute).Unix(), Accounts: []accountSnapshot{
		{Path: "synthetic.json", State: "cooldown", CooldownMsg: "code=401 Authorization: Bearer legacy-synthetic-secret"},
	}}
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(statusSnapshotFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(handler, http.MethodGet, "/admin/api/status", admin, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "legacy-synthetic-secret") || !strings.Contains(w.Body.String(), `"stale":true`) {
		t.Fatal("status copy redaction or stale flag failed")
	}
	unchanged, _ := os.ReadFile(statusSnapshotFile)
	if !bytes.Equal(data, unchanged) {
		t.Fatal("status display changed business snapshot")
	}
}

func BenchmarkTailLongLineLinear(b *testing.B) {
	path := filepath.Join(b.TempDir(), "line.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("A", 32<<10)+"\n"), 0600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lines, err := tailLines(path, 200)
		if err != nil || len(lines) != 1 || len(lines[0]) != 32<<10 {
			b.Fatal("tail result mismatch")
		}
	}
}

// 旧启动参数必须被接受（不报错），但必须完全不产生作用。
func TestGatewayLegacyAPIKeyFlagIsAcceptedButInert(t *testing.T) {
	isolatedGatewayConfig(t)
	// 场景一：config.json 未配置 Key，旧参数不得开启鉴权、不得写盘。
	ignoreLegacyAPIKeyFlag("synthetic-legacy-key")
	if currentAPIKey() != "" {
		t.Fatal("legacy -api-key must not enable API authentication")
	}
	if _, err := os.Stat(runtimeConfigFile); !os.IsNotExist(err) {
		t.Fatal("legacy flag must not write config.json")
	}
	if code := adminRequest(newAPIHandler(), http.MethodPost, "/v1/messages/count_tokens", "", `{"messages":[]}`).Code; code != 200 {
		t.Fatalf("legacy flag changed model API behaviour, status=%d", code)
	}
	if code := adminRequest(newAPIHandler(), http.MethodPost, "/v1/messages/count_tokens", "any-client-key", `{"messages":[]}`).Code; code != 200 {
		t.Fatalf("arbitrary key should still work while API auth is disabled, status=%d", code)
	}
	// 场景二：config.json 已配置 Key，旧参数不得覆盖配置。
	content := `{"gateway":{"apiPort":8317,"webPort":8316,"adminKey":"manual-admin","apiKeyEnabled":true,"apiKey":"synthetic-config-key"}}`
	if err := os.WriteFile(runtimeConfigFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	ignoreLegacyAPIKeyFlag("synthetic-other-key")
	if currentAPIKey() != "synthetic-config-key" {
		t.Fatal("legacy flag must not override config.json")
	}
	if code := adminRequest(newAPIHandler(), http.MethodPost, "/v1/messages/count_tokens", "synthetic-other-key", `{"messages":[]}`).Code; code != 401 {
		t.Fatalf("legacy flag value must not authenticate, status=%d", code)
	}
}

// 显式启用的本地浏览器夹具。只启动两个测试 handler，不启动上游、续期或生产服务。
func TestWebUIBrowserHarness(t *testing.T) {
	dir := os.Getenv("WB_UI_HARNESS_DIR")
	if dir == "" {
		t.Skip("browser harness not requested")
	}
	isolatedGatewayConfig(t)
	api := httptest.NewServer(newAPIHandler())
	defer api.Close()
	web := httptest.NewServer(newWebUIHandler())
	defer web.Close()
	_, apiPort, _ := net.SplitHostPort(strings.TrimPrefix(api.URL, "http://"))
	_, webPort, _ := net.SplitHostPort(strings.TrimPrefix(web.URL, "http://"))
	cfg.Port, cfg.WebPort = portNumber(t, apiPort), portNumber(t, webPort)
	g := gatewayFileConfig{APIPort: cfg.Port, WebPort: cfg.WebPort}
	data, _ := json.Marshal(map[string]any{"gateway": g, "systemPrompt": map[string]string{"force": "synthetic fixture prompt"}})
	if err := os.WriteFile(runtimeConfigFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	snap := statusSnapshot{UpdatedAt: time.Now().Unix(), Accounts: []accountSnapshot{
		{Path: "fixture.json", Edition: "cn", State: "active", QuotaKnown: true},
	}, Models: []modelStatSnapshot{{ID: "fixture-model", CNMultiplier: "-", IntlMultiplier: "-", AvailableAccounts: 1}}}
	data, _ = json.Marshal(snap)
	if err := os.WriteFile(statusSnapshotFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]string{"apiURL": api.URL, "webURL": web.URL, "configDir": mustWorkingDir(t)})
	if err := os.WriteFile(filepath.Join(dir, "ready.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("synthetic browser harness ready; no production/upstream requests")
	deadline := time.After(10 * time.Minute)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				return
			}
		}
	}
}

func mustWorkingDir(t *testing.T) string {
	t.Helper()
	path, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// 编译期确认管理 HTTP 请求仍走标准响应接口，不引入特殊流式写入器要求。
var _ io.Writer = (*httptest.ResponseRecorder)(nil)
