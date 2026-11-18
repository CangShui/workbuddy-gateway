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

func TestListFilterAuditIsAuthenticatedBooleanOnlyAndDoesNotChangePolicies(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	before, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"list_filter_changed", "list_filter_restored", "list_filter_cookie_failed"} {
		for _, route := range []string{"/admin/api/status", "/admin/api/models"} {
			for _, enabled := range []bool{false, true} {
				raw, _ := json.Marshal(map[string]any{"event": event, "route": route, "status": 0, "enabled": enabled})
				w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", string(raw))
				if w.Code != http.StatusNoContent {
					t.Fatalf("valid list filter event rejected: event=%s route=%s code=%d", event, route, w.Code)
				}
			}
		}
	}
	for _, body := range []string{
		`{"event":"list_filter_changed","route":"/admin/api/status","status":0}`,
		`{"event":"list_filter_changed","route":"/admin/api/status","status":0,"enabled":"true"}`,
		`{"event":"list_filter_changed","route":"/admin/api/config-edit","status":0,"enabled":true}`,
		`{"event":"request_completed","route":"/admin/api/status","status":200,"enabled":true}`,
		`{"event":"list_filter_changed","route":"/admin/api/models","status":0,"enabled":true,"cookie":"private"}`,
	} {
		w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unsafe list filter event accepted: HTTP=%d", w.Code)
		}
	}
	body := `{"event":"list_filter_changed","route":"/admin/api/models","status":0,"enabled":true}`
	for _, key := range []string{"", "feature-api-key"} {
		if w := adminRequest(handler, "POST", "/admin/api/client-events", key, body); w.Code != http.StatusUnauthorized {
			t.Fatal("filter audit endpoint bypassed management authentication")
		}
	}
	// Cookie 不是登录凭据，甚至不能代替 Authorization 管理请求头。
	req := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	req.Header.Set("Cookie", "gateway_accounts_only_usable=1; gateway_models_only_usable=1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("display preference Cookie bypassed management authentication")
	}
	after, err := os.ReadFile(runtimeConfigFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("display preference audit changed production policies")
	}
	logData, err := os.ReadFile(filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log"))
	if err != nil || !bytes.Contains(logData, []byte("[管理前端筛选]")) ||
		!bytes.Contains(logData, []byte("只显示可用=true")) || !bytes.Contains(logData, []byte("只显示可用=false")) {
		t.Fatal("display preference changes were not logged")
	}
}

func TestStatsChartHoverAuditOnlyAcceptsFixedMetadata(t *testing.T) {
	isolateAdminFeatures(t)
	handler := newWebUIHandler()
	before, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"stats_chart_hover_started", "stats_chart_hover_ended"} {
		body := `{"event":"` + event + `","route":"/admin/api/stats","status":0}`
		w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", body)
		if w.Code != http.StatusNoContent {
			t.Fatalf("hover audit status=%d", w.Code)
		}
	}
	for _, body := range []string{
		`{"event":"stats_chart_hover_started","route":"/admin/api/config","status":0}`,
		`{"event":"stats_chart_hover_started","route":"/admin/api/stats","status":200}`,
		`{"event":"stats_chart_hover_ended","route":"/admin/api/stats","status":0,"enabled":true}`,
		`{"event":"stats_chart_hover_started","route":"/admin/api/stats","status":0,"coordinates":[1,2]}`,
	} {
		if w := adminRequest(handler, "POST", "/admin/api/client-events", "feature-admin-key", body); w.Code != http.StatusBadRequest {
			t.Fatalf("unsafe hover event accepted: %d", w.Code)
		}
	}
	if w := adminRequest(handler, "POST", "/admin/api/client-events", "", `{"event":"stats_chart_hover_started","route":"/admin/api/stats","status":0}`); w.Code != http.StatusUnauthorized {
		t.Fatal("hover audit bypassed authentication")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(before, after) {
		t.Fatal("hover audit changed configuration")
	}
	entries, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	var logs string
	for _, p := range entries {
		raw, _ := os.ReadFile(p)
		logs += string(raw)
	}
	if !strings.Contains(logs, "stats_chart_hover_started") || !strings.Contains(logs, "stats_chart_hover_ended") {
		t.Fatal("hover audit was not persisted")
	}
}

func TestWebUIBrandNeutralLabelsAndBothListFilters(t *testing.T) {
	html, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(html)), "workbuddy") {
		t.Fatal("web console static copy still contains the old brand")
	}
	for _, text := range []string{"<title>本地网关 · 网页控制台</title>", `id="onlyUsableAccounts"`, `id="onlyUsable"`,
		"只显示可用账号", "只显示可用模型", `id="addAccountCopy"`, "复制链接"} {
		if !bytes.Contains(html, []byte(text)) {
			t.Errorf("web console missing list control or neutral copy: %s", text)
		}
	}
}

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

func TestWebUIPerformanceAndSnapshotCacheConsistency(t *testing.T) {
	isolateAdminFeatures(t)
	resetStatsState(t)
	loadOptionalStats()
	catalogModels = map[string][]catalogModel{"cn": {{ID: "deepseek-v4.1-flash", HasMultiplier: true, Multiplier: 0}}}
	acc := &Account{
		Path:    "workbuddy-perf-a.json",
		Edition: "cn",
		Auth: &StoredAuth{
			Edition: "cn",
			Account: StoredAccount{UID: "u-perf", Nickname: "perf-user"},
			Auth:    StoredTokens{AccessToken: "synthetic-perf-access-token", RefreshToken: "synthetic-perf-refresh-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
		QuotaKnown:     true,
		QuotaTotal:     1000,
		QuotaRemaining: 900,
	}
	registerCredentialSecrets(acc.Auth)
	accounts = []*Account{acc}
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(statsZone)
	accountMu.Lock()
	accID, accView := statsAccountIDLocked(acc)
	accountMu.Unlock()
	requestStats.Lock()
	requestStats.document.Accounts[accID] = accView
	for i := 0; i < 500; i++ {
		at := now.Add(-time.Duration(500-i) * time.Minute).UnixMilli()
		tok := int64(100 + i)
		cr := 0.01
		ttft := 240.0
		requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
			Account: accID, Model: "deepseek-v4.1-flash", At: at, Status: 200, Complete: true,
			TotalTokens: &tok, InputTokens: &tok, Credit: &cr, LatencyMS: 800, TTFTMS: &ttft, TTFTMetric: ttftMetric,
		})
	}
	requestStats.revision = 1
	requestStats.Unlock()

	handler := newWebUIHandler()
	// 首次统计聚合建立缓存，第二次命中缓存；结果必须一致且包含 dataRevision。
	w1 := adminRequest(handler, http.MethodGet, "/admin/api/stats?period=day", "feature-admin-key", "")
	w2 := adminRequest(handler, http.MethodGet, "/admin/api/stats?period=day", "feature-admin-key", "")
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("stats status: %d / %d", w1.Code, w2.Code)
	}
	var s1, s2 map[string]any
	if err := json.Unmarshal(w1.Body.Bytes(), &s1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &s2); err != nil {
		t.Fatal(err)
	}
	if s1["dataRevision"] != float64(1) || s2["dataRevision"] != float64(1) {
		t.Fatalf("missing dataRevision: %v / %v", s1["dataRevision"], s2["dataRevision"])
	}
	if s1["summary"].(map[string]any)["requests"] != s2["summary"].(map[string]any)["requests"] {
		t.Fatalf("cached stats summary mismatch: %v vs %v", s1["summary"], s2["summary"])
	}

	// 开关切换后立即刷新状态快照与内存缓存，下一次 /admin/api/status 立刻看到停用状态。
	wt := adminRequest(handler, http.MethodPost, "/admin/api/account-toggle", "feature-admin-key", `{"target":"workbuddy-perf-a.json","enabled":false}`)
	if wt.Code != http.StatusOK {
		t.Fatalf("account toggle failed: %d %s", wt.Code, wt.Body.String())
	}
	ws := adminRequest(handler, http.MethodGet, "/admin/api/status", "feature-admin-key", "")
	if ws.Code != http.StatusOK || !strings.Contains(ws.Body.String(), `"paused":1`) {
		t.Fatalf("status snapshot was not immediately refreshed after policy toggle: %d %s", ws.Code, ws.Body.String())
	}
	if strings.Contains(ws.Body.String(), "synthetic-perf-access-token") {
		t.Fatal("admin response leaked registered secret")
	}

	// 批量前端审计接口兼容单条与批量事件，同时拒绝越界或非法字段。
	wb := adminRequest(handler, http.MethodPost, "/admin/api/client-events", "feature-admin-key",
		`{"events":[{"event":"list_filter_restored","route":"/admin/api/status","status":0,"enabled":true},{"event":"list_filter_restored","route":"/admin/api/models","status":0,"enabled":false}]}`)
	if wb.Code != http.StatusNoContent {
		t.Fatalf("batched client events rejected: %d %s", wb.Code, wb.Body.String())
	}
	wBad := adminRequest(handler, http.MethodPost, "/admin/api/client-events", "feature-admin-key", `{"events":[]}`)
	if wBad.Code != http.StatusBadRequest {
		t.Fatalf("empty batched client events should return 400, got %d", wBad.Code)
	}
}

func BenchmarkWebUIStatsCachedVsUncached(b *testing.B) {
	oldWriter := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(oldWriter)
	requestStats.Lock()
	oldDoc, oldRev, oldEnabled, oldOrder := requestStats.document, requestStats.revision, requestStats.enabled, requestStats.requestsOrder
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, statsZone)
	rows := make([]statsRequest, 50000)
	for i := range rows {
		at := now.Add(-time.Duration(len(rows)-i) * time.Minute).UnixMilli()
		tok := int64(200 + (i % 50))
		cr := 0.02
		ttft := 300.0
		rows[i] = statsRequest{
			Account: "bench-acc", Model: "deepseek-v4.1-flash", At: at, Status: 200, Complete: true,
			TotalTokens: &tok, InputTokens: &tok, Credit: &cr, LatencyMS: 950, TTFTMS: &ttft, TTFTMetric: ttftMetric,
		}
	}
	requestStats.document = statsDocument{
		Schema:    statsSchema,
		StartedAt: rows[0].At,
		Accounts:  map[string]statsAccount{"bench-acc": {File: "workbuddy-bench.json", Edition: "cn"}},
		Requests:  rows,
	}
	requestStats.revision = 1
	requestStats.enabled = true
	requestStats.requestsOrder = 0
	requestStats.Unlock()
	defer func() {
		requestStats.Lock()
		requestStats.document, requestStats.revision, requestStats.enabled, requestStats.requestsOrder = oldDoc, oldRev, oldEnabled, oldOrder
		requestStats.Unlock()
		statsQueryCache.Lock()
		statsQueryCache.basePayload, statsQueryCache.redactedBase, statsQueryCache.bucketsStart = nil, nil, nil
		statsQueryCache.Unlock()
	}()

	b.Run("window_cold_binary_search", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			statsQueryCache.Lock()
			statsQueryCache.basePayload, statsQueryCache.redactedBase = nil, nil
			statsQueryCache.Unlock()
			rec := httptest.NewRecorder()
			handleWebUIStats(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats?period=hour", nil))
			if rec.Code != http.StatusOK {
				b.Fatalf("status=%d", rec.Code)
			}
		}
	})
	b.Run("window_warm_cache", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rec := httptest.NewRecorder()
			handleWebUIStats(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats?period=hour", nil))
			if rec.Code != http.StatusOK {
				b.Fatalf("status=%d", rec.Code)
			}
		}
	})
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
