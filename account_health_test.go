package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountHealthPercentage(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name    string
		rows    []accountSnapshot
		stale   bool
		state   string
		percent *float64
	}{
		{"empty", nil, false, "empty", nil},
		{"all_ok", []accountSnapshot{{State: "active"}, {State: "active"}}, false, "healthy", float64p(100)},
		{"paused_in_denominator", []accountSnapshot{{State: "active"}, {State: "active", SchedulingEnabled: &off}}, false, "degraded", float64p(50)},
		{"no_available", []accountSnapshot{{State: "expired"}, {State: "disabled"}}, false, "unavailable", float64p(0)},
		{"unknown_not_healthy", []accountSnapshot{{State: "unrecognized"}, {State: "active"}}, false, "degraded", float64p(50)},
		{"stale_not_100", []accountSnapshot{{State: "active"}}, true, "unknown", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeAccountHealth(tc.rows, tc.stale)
			if got.State != tc.state || (got.Percent == nil) != (tc.percent == nil) || (got.Percent != nil && *got.Percent != *tc.percent) {
				t.Fatalf("got=%+v want=%s/%v", got, tc.state, tc.percent)
			}
		})
	}
	rows := make([]accountSnapshot, 1000)
	for i := range rows {
		rows[i].State = "active"
	}
	rows[999].State = "cooldown"
	if got := summarizeAccountHealth(rows, false); got.Percent == nil || *got.Percent != 99.9 {
		t.Fatal("non-fully-healthy pool must not round up to 100")
	}
}

func TestAccountAlertParsesKnownLogsOnly(t *testing.T) {
	prefix := "2026/10/14 22:59:30 "
	tests := []struct{ body, state, account string }{
		{`[账号状态] {"at":1791989970,"account":"workbuddy a.json","state":"expired"}`, "expired", "workbuddy a.json"},
		{"[Cooldown] 账号 a.json 触发频率限制，自动屏蔽至 2099-01-01 (提示: x)", "cooldown", "a.json"},
		{"[Quota] traceId=x 账号 a.json 额度查询成功，总额度=0；账号已冻结调度，等待额度恢复", "paid_exhausted", "a.json"},
		{"[Quota] traceId=x 账号 a.json 企业额度计算=limitNum 100 - credit 100；分配额度已用完，收费模型暂停调度", "paid_exhausted", "a.json"},
		{"[Auth] 账号 a.json 判定登录态失效，已停止调度并写入失效标记", "disabled", "a.json"},
		{`[管理开关] traceId=x 结果=成功 类型=account 目标="a b.json" 启用=false 说明=配置已保存`, "paused", "a b.json"},
		{"[Quota] traceId=x 账号 a.json 查询额度失败，保留上一次数据", "", ""},
		{"[Auth] 账号 a.json Token 需要续期，开始调用上游刷新接口", "", ""},
		{"[Auth] 账号 a.json 续期被上游判定登录态失效（连续1/3次）；暂时保留凭据", "", ""},
		{"[ModelCooldown] 账号 a.json 模型 m 触发模型级频率限制", "", ""},
		{`[管理开关] traceId=x 结果=成功 类型=model 目标="a.json" 启用=false 说明=模型停用`, "", ""},
		{`[账号状态] {"at":1,"account":"a","state":"active"}`, "", ""},
		{`[账号状态] {"at":0,"account":"a","state":"expired"}`, "", ""},
		{"[外部接口] 服务返回文字 账号失效", "", ""},
	}
	for _, tc := range tests {
		event, ok := parseAccountLogEvent(prefix+tc.body, "gateway-2026-10-14.log")
		if ok != (tc.state != "") || (ok && (event.State != tc.state || event.Account != tc.account)) {
			t.Fatalf("body=%s got=%+v/%v", tc.body, event, ok)
		}
	}
}

func TestAccountStateLogsTransitionsWithoutFlooding(t *testing.T) {
	accountStateLog.Lock()
	old := accountStateLog.last
	accountStateLog.last = map[string]string{}
	accountStateLog.Unlock()
	t.Cleanup(func() { accountStateLog.Lock(); accountStateLog.last = old; accountStateLog.Unlock() })
	var audit bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&audit)
	defer log.SetOutput(writer)
	off := false
	rows := []accountSnapshot{{Path: "a.json", State: "expired", SchedulingEnabled: &off, DisabledReason: "Bearer never-emit-raw"}}
	logAccountStateChanges(rows, time.Now().Unix())
	logAccountStateChanges(rows, time.Now().Add(time.Second).Unix())
	if strings.Count(audit.String(), "[账号状态]") != 2 {
		t.Fatal("identical snapshot must not flood logs")
	}
	if !strings.Contains(audit.String(), `"state":"expired"`) || !strings.Contains(audit.String(), `"state":"paused"`) || strings.Contains(audit.String(), "never-emit-raw") {
		t.Fatal("missing kinds or leaked raw reason")
	}
	rows[0].State, rows[0].SchedulingEnabled = "active", nil
	logAccountStateChanges(rows, time.Now().Add(2*time.Second).Unix())
	if !strings.Contains(audit.String(), `"state":"active"`) {
		t.Fatal("recovery transition not logged")
	}
}

func TestAccountAlertReadDeduplicateAndResolve(t *testing.T) {
	chdirTemp(t)
	os.Mkdir(logDir, 0700)
	day := "gateway-2026-10-14.log"
	lines := "2026/10/14 01:00:00 [账号状态] {\"at\":100,\"account\":\"a.json\",\"state\":\"disabled\"}\n" +
		"2026/10/14 02:00:00 [账号状态] {\"at\":200,\"account\":\"a.json\",\"state\":\"disabled\"}\n" +
		"2026/10/14 03:00:00 [账号状态] {\"at\":300,\"account\":\"b.json\",\"state\":\"expired\"}\n" +
		"2026/10/14 04:00:00 [账号状态] {\"at\":400,\"account\":\"c.json\",\"state\":\"paused\"}\n" +
		"2026/10/14 04:00:00 [账号状态] {\"at\":999,\"account\":\"c.json\",\"state\":\"paused\"}" // incomplete trailing line
	os.WriteFile(filepath.Join(logDir, day), []byte(lines), 0600)
	os.WriteFile(filepath.Join(logDir, "debug-2026-10-14.jsonl"), []byte(lines), 0600)
	scan, err := readAccountAlertLogs()
	if err != nil || len(scan.Events) != 3 || scan.Events["a.json\x00disabled"].At != 200 || scan.Events["c.json\x00paused"].At != 400 {
		t.Fatalf("%+v %v", scan, err)
	}
	off := false
	rows := []accountSnapshot{{Path: "a.json", State: "active"}, {Path: "b.json", State: "expired"},
		{Path: "c.json", State: "active", SchedulingEnabled: &off}, {Path: "d.json", State: "cooldown"}}
	alerts, missing := currentAccountAlerts(rows, scan)
	if len(alerts) != 2 || alerts[0].State != "expired" || alerts[1].State != "paused" || missing != 1 {
		t.Fatalf("recovery/dedup/priority: %+v missing=%d", alerts, missing)
	}
	if alerts[0].LogFile != day {
		t.Fatal("source must reference real log")
	}
}

func TestAccountAlertEndpointAuthenticatedReadOnlyAndHealth(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-health-admin"}); err != nil {
		t.Fatal(err)
	}
	accountAlertCache.Lock()
	accountAlertCache.at = time.Time{}
	accountAlertCache.Unlock()
	t.Cleanup(func() { accountAlertCache.Lock(); accountAlertCache.at = time.Time{}; accountAlertCache.Unlock() })
	doc := statusSnapshot{UpdatedAt: time.Now().Unix(), Accounts: []accountSnapshot{{Path: "a.json", State: "disabled"}}}
	raw, _ := json.Marshal(doc)
	os.WriteFile(statusSnapshotFile, raw, 0600)
	event, _ := json.Marshal(accountLogEvent{At: time.Now().Unix(), Account: "a.json", State: "disabled"})
	log.Printf("[账号状态] %s", event)
	handler := newWebUIHandler()
	for _, path := range []string{"/admin/api/account-alerts", "/admin/api/status"} {
		if got := adminRequest(handler, "GET", path, "", ""); got.Code != 401 {
			t.Fatalf("no auth status=%d", got.Code)
		}
	}
	got := adminRequest(handler, "GET", "/admin/api/account-alerts", "synthetic-health-admin", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"severity":"urgent"`) {
		t.Fatalf("%d %s", got.Code, got.Body.String())
	}
	got = adminRequest(handler, "POST", "/admin/api/account-alerts", "synthetic-health-admin", "{}")
	if got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("write accepted %d", got.Code)
	}
	got = adminRequest(handler, "GET", "/admin/api/status", "synthetic-health-admin", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"state":"unavailable"`) {
		t.Fatalf("%d %s", got.Code, got.Body.String())
	}
}

func TestAccountAlertMissingAndBoundedLogs(t *testing.T) {
	chdirTemp(t)
	scan, err := readAccountAlertLogs()
	if err != nil || len(scan.Events) != 0 {
		t.Fatal("missing logs must be empty, not a fabricated alert")
	}
	os.Mkdir(logDir, 0700)
	path := filepath.Join(logDir, "gateway-2026-10-14.log")
	content := strings.Repeat("x", int(accountAlertReadBudget)+100) + "\n2026/10/14 01:00:00 [账号状态] {\"at\":123,\"account\":\"a.json\",\"state\":\"expired\"}\n"
	os.WriteFile(path, []byte(content), 0600)
	scan, err = readAccountAlertLogs()
	if err != nil || !scan.Truncated || scan.Scanned != accountAlertReadBudget || len(scan.Events) != 1 {
		t.Fatalf("%+v %v", scan, err)
	}
	// 直接处理器对写操作也拒绝，不能以无鉴权执行的测试误判为只读。
	rec := httptest.NewRecorder()
	handleWebUIAccountAlerts(rec, httptest.NewRequest("DELETE", "/admin/api/account-alerts", nil))
	if rec.Code != 405 {
		t.Fatal("DELETE must be rejected")
	}
}

func TestAccountAlertLogReadFailureDoesNotBreakHealth(t *testing.T) {
	chdirTemp(t)
	accountAlertCache.Lock()
	accountAlertCache.at = time.Time{}
	accountAlertCache.Unlock()
	t.Cleanup(func() { accountAlertCache.Lock(); accountAlertCache.at = time.Time{}; accountAlertCache.Unlock() })
	if err := os.WriteFile(logDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(statusSnapshot{UpdatedAt: time.Now().Unix(), Accounts: []accountSnapshot{{Path: "a.json", State: "active"}}})
	if err := os.WriteFile(statusSnapshotFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handleWebUIAccountAlerts(rec, httptest.NewRequest(http.MethodGet, "/admin/api/account-alerts", nil))
	if rec.Code != 503 {
		t.Fatalf("unreadable logs must not masquerade as an empty list: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleWebUIStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("logs failure broke health endpoint: %d", rec.Code)
	}
}
