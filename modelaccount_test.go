package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestModelAccountFilterPerModelAndPriority(t *testing.T) {
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	err := setModelAccountFilter(map[string]modelAccountFileConfig{
		" Model-A ": {Allowlist: []string{"a.json", "b.json"}, Blocklist: []string{"b.json"}},
		"model-b":   {Blocklist: []string{"a.json"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := &Account{Path: "auth/a.json"}, &Account{Path: "b.json"}, &Account{Path: "c.json"}
	pool := []*Account{a, b, c}
	for _, tc := range []struct {
		model string
		acc   *Account
		want  bool
	}{
		{"model-a", a, true}, {"model-a", b, false}, {"model-a", c, false},
		{"MODEL-B", a, false}, {"model-b", b, true}, {"model-b", c, true},
		{"unconfigured", a, true}, {"unconfigured", b, true}, {"unconfigured", c, true},
	} {
		got, _ := modelAccountAllowed(tc.model, tc.acc, pool)
		if got != tc.want {
			t.Errorf("model=%s account=%s allowed=%t want=%t", tc.model, tc.acc.Path, got, tc.want)
		}
	}
	if !modelAccountRuleConfigured("model-a") || modelAccountRuleConfigured("unconfigured") {
		t.Fatal("rule configured detection failed")
	}
}

func TestModelAccountFilterRejectsPathsAndDuplicateNames(t *testing.T) {
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	for _, bad := range []string{"../a.json", "auth/a.json", `auth\a.json`, "", "a.txt", "a*b.json", "a?.json", "[a].json", "/a.json"} {
		if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Allowlist: []string{bad}}}); err == nil {
			t.Errorf("accepted invalid file name %q", bad)
		}
	}
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Allowlist: []string{"a.json"}}}); err != nil {
		t.Fatal(err)
	}
	a, b := &Account{Path: "dir1/a.json"}, &Account{Path: "dir2/a.json"}
	for _, acc := range []*Account{a, b} {
		if ok, _ := modelAccountAllowed("m", acc, []*Account{a, b}); ok {
			t.Fatal("ambiguous basename must not be allowed")
		}
	}
}

func TestModelAccountFilterConfigAndSnapshot(t *testing.T) {
	chdirTemp(t)
	resetModelStats()
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	config := `{"models":{"accounts":{"model-a":{"allowlist":["a.json"],"blocklist":["b.json"]}}}}`
	if err := os.WriteFile(runtimeConfigFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	a, b := &Account{Path: "a.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaRemaining: 10},
		&Account{Path: "b.json", Auth: &StoredAuth{}, QuotaKnown: true, QuotaRemaining: 10}
	modelStatsMu.Lock()
	modelStats["model-a"] = &modelStat{}
	modelStatsMu.Unlock()
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{a, b}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	writeStatusSnapshot()
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	var row *modelStatSnapshot
	for i := range snap.Models {
		if snap.Models[i].ID == "model-a" {
			row = &snap.Models[i]
		}
	}
	if row == nil || row.AvailableAccounts != 1 {
		t.Fatalf("snapshot availability should exclude blocked account: %+v", row)
	}
	table := renderModelTable([]modelStatSnapshot{*row})
	if strings.Contains(table, "账号规则") {
		t.Fatalf("monitor table must not show removed account rule column: %s", table)
	}
	if _, err := os.Stat(filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log")); err == nil {
		t.Fatal("test should not create gateway logs by itself")
	}
}

func TestMissingConfigClearsAccountPolicy(t *testing.T) {
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Blocklist: []string{"a.json"}}}); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatal(err)
	}
	allowed, _ := modelAccountAllowed("m", &Account{Path: "a.json"}, nil)
	if !allowed || modelAccountRuleCount() != 0 {
		t.Fatal("missing config must leave model accounts unrestricted")
	}
}

func TestConfiguredAccountRuleAppearsWithoutCatalogOrTraffic(t *testing.T) {
	resetModelStats()
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{
		"custom-model": {Allowlist: []string{"a.json"}},
		"empty-rule":   {},
	}); err != nil {
		t.Fatal(err)
	}
	if got := modelAccountRuleCount(); got != 1 {
		t.Fatalf("only active rules count, got %d", got)
	}
	rows := buildModelStatSnapshots(time.Now(), []*Account{{Path: "a.json", Auth: &StoredAuth{}}})
	var found bool
	for _, row := range rows {
		if row.ID == "empty-rule" {
			t.Fatal("empty rule must not create a monitor row")
		}
		if row.ID == "custom-model" {
			found = true
			if row.AvailableAccounts != 1 {
				t.Fatalf("custom monitor row: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("configured model must appear in monitor even without catalog entry or traffic")
	}
}

func TestModelAccountPolicySelectionAndNoFallback(t *testing.T) {
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{
		"m": {Allowlist: []string{"a.json"}},
	}); err != nil {
		t.Fatal(err)
	}
	a, b := &Account{Path: "a.json", Auth: &StoredAuth{}}, &Account{Path: "b.json", Auth: &StoredAuth{}}
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = []*Account{a, b}, 0
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	selected, _, err := nextAccountForModel("m", nil)
	if err != nil || selected != a {
		t.Fatalf("selection=%v err=%v", selected, err)
	}
	if selected, _, err := nextAccountForModel("m", map[*Account]bool{a: true}); selected != nil || err == nil {
		t.Fatalf("fallback selected blocked account: %v, %v", selected, err)
	}
	selected, _, err = nextAccountForModel("other", nil)
	if err != nil || selected != b {
		t.Fatalf("policy leaked to other model: %v, %v", selected, err)
	}
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Allowlist: []string{"absent.json"}}}); err != nil {
		t.Fatal(err)
	}
	_, _, err = nextAccountForModel("m", nil)
	if !errors.Is(err, errModelAccountPolicy) {
		t.Fatalf("expected policy-specific rejection, got %v", err)
	}
}

func TestModelAccountPolicyHitLogsSingleShortMarker(t *testing.T) {
	resetModelFilter()
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Blocklist: []string{"b.json"}}}); err != nil {
		t.Fatal(err)
	}
	a, b := &Account{Path: "a.json", Auth: &StoredAuth{}}, &Account{Path: "b.json", Auth: &StoredAuth{}}
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = []*Account{a, b}, 0
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	oldBase, oldClient := profileCN.Base, cfg.HttpClient
	profileCN.Base = upstream.URL
	cfg.HttpClient = upstream.Client()
	t.Cleanup(func() { profileCN.Base, cfg.HttpClient = oldBase, oldClient })

	var audit bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&audit)
	t.Cleanup(func() { log.SetOutput(oldLog) })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)
	logs := audit.String()
	if !strings.Contains(logs, "账号名单命中") || !strings.Contains(logs, "a.json") {
		t.Fatalf("hit marker missing: %s", logs)
	}
	if strings.Contains(logs, "b.json") || strings.Contains(logs, "候选账号=") {
		t.Fatalf("excluded account must not be listed per-account: %s", logs)
	}
}

func TestModelAccountPolicyRejectsBeforeUpstream(t *testing.T) {
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Allowlist: []string{"absent.json"}}}); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: "a.json", Auth: &StoredAuth{}}}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	var audit, debug bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&audit)
	t.Cleanup(func() { log.SetOutput(oldLog) })
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = &debugJSONSink{writer: &debug}
	debugSinkMu.Unlock()
	t.Cleanup(func() {
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "账号黑白名单禁用") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(audit.String(), "账号黑白名单没有匹配的凭据") ||
		!strings.Contains(audit.String(), "未进入上游") {
		t.Fatalf("audit missing policy rejection: %s", audit.String())
	}
	if !strings.Contains(debug.String(), `"event":"model_account_policy_rejected"`) {
		t.Fatalf("debug missing policy rejection event: %s", debug.String())
	}
	if strings.Contains(audit.String(), "候选账号=") {
		t.Fatalf("per-account policy log must be removed: %s", audit.String())
	}
}

func TestAdminProbeSkipsAccountDisallowedForModel(t *testing.T) {
	chdirTemp(t)
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Blocklist: []string{"blocked.json"}}}); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: "blocked.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/probe", strings.NewReader(`{"models":["m"]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleAdminProbe)).ServeHTTP(rec, req)
	var out probeResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Results) != 1 ||
		out.Results[0].Status != "skipped" || !strings.Contains(out.Results[0].Detail, "账号名单排除") {
		t.Fatalf("probe must skip forbidden account without upstream traffic: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestModelAccountPolicyPriceProbeSelection(t *testing.T) {
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Blocklist: []string{"a.json"}}}); err != nil {
		t.Fatal(err)
	}
	a := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "a"}}, QuotaKnown: true, QuotaRemaining: 10}
	b := &Account{Path: "b.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "b"}}, QuotaKnown: true, QuotaRemaining: 10}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{a, b}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	if got := pickProbeAccountForModel("cn", "m"); got != b {
		t.Fatalf("price probe selected blocked account: %v", got)
	}
}

// 名单外账号即使在轮询顺序里靠前，也绝不能被选中（端到端验证）。
func TestModelAccountBlockedAccountNeverSelected(t *testing.T) {
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{"m": {Allowlist: []string{"c.json"}}}); err != nil {
		t.Fatal(err)
	}
	var called []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.Header.Get("X-User-Id"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	oldBase, oldClient := profileCN.Base, cfg.HttpClient
	profileCN.Base = upstream.URL
	cfg.HttpClient = upstream.Client()
	t.Cleanup(func() { profileCN.Base, cfg.HttpClient = oldBase, oldClient })

	accountMu.Lock()
	oldAcc, oldRR := accounts, rrIndex
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "ta", ExpiresAt: time.Now().Add(time.Hour).Unix()}, Account: StoredAccount{UID: "UA"}}},
		{Path: "b.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "tb", ExpiresAt: time.Now().Add(time.Hour).Unix()}, Account: StoredAccount{UID: "UB"}}},
		{Path: "c.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "tc", ExpiresAt: time.Now().Add(time.Hour).Unix()}, Account: StoredAccount{UID: "UC"}}},
	}
	rrIndex = 0
	accountMu.Unlock()
	t.Cleanup(func() { accountMu.Lock(); accounts, rrIndex = oldAcc, oldRR; accountMu.Unlock() })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(called) != 1 || called[0] != "UC" {
		t.Fatalf("名单外账号被调用: %v (期望仅 UC)", called)
	}
}
