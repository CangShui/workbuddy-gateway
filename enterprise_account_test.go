package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnterpriseUsageSchemaValidation(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       float64
		unlimited  bool
		bad        bool
	}{
		{"camel_precise", `{"limitNum":2000,"credit":812.45}`, 1187.55, false, false},
		{"snake_strings", `{"limit_num":"5","used_num":"1.25"}`, 3.75, false, false},
		{"usedNum", `{"limitNum":5,"usedNum":1}`, 4, false, false},
		{"unlimited", `{"limitNum":-1,"credit":12.5}`, -1, true, false},
		{"allocated_zero", `{"limitNum":0,"credit":0}`, 0, false, false},
		{"overdrawn", `{"limitNum":3,"credit":4}`, 0, false, false},
		{"same_alias_values", `{"limitNum":5,"limit_num":"5","credit":1,"used_num":"1"}`, 4, false, false},
		{"missing_used", `{"limitNum":2000}`, 0, false, true},
		{"null_used", `{"limitNum":2000,"credit":null}`, 0, false, true},
		{"missing_limit", `{"credit":0}`, 0, false, true},
		{"negative_used", `{"limitNum":2000,"credit":-1}`, 0, false, true},
		{"invalid_negative_limit", `{"limitNum":-2,"credit":0}`, 0, false, true},
		{"nan", `{"limitNum":"NaN","credit":0}`, 0, false, true},
		{"infinity", `{"limitNum":5,"credit":"+Inf"}`, 0, false, true},
		{"overflow", `{"limitNum":"1e9999","credit":0}`, 0, false, true},
		{"alias_limit_conflict", `{"limitNum":0,"limit_num":2000,"credit":0}`, 0, false, true},
		{"alias_used_conflict", `{"limitNum":2000,"credit":0,"used_num":100}`, 0, false, true},
		{"empty", `{}`, 0, false, true},
		{"null", `null`, 0, false, true},
		{"array", `[]`, 0, false, true},
		{"bool_limit", `{"limitNum":true,"credit":0}`, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, remaining, unlimited, err := parseEnterpriseUsage([]byte(tc.data))
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v wantBad=%t", err, tc.bad)
			}
			if !tc.bad && (math.Abs(remaining-tc.want) > 0.000001 || unlimited != tc.unlimited) {
				t.Fatalf("remaining=%v unlimited=%v", remaining, unlimited)
			}
		})
	}
}

// 全链路本地集成：真实 HTTP 客户端→真实鉴权/三个 API 入口→选号→模拟上游 SSE。
// 不只测「函数可选号」；验证修复后真正有正文返回，以及确实耗尽/14018仍被阻断。
func TestEnterpriseQuotaFullHTTPFlow(t *testing.T) {
	isolatedGatewayConfig(t)
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })
	oldCN, oldIntl := profileCN, profileINTL
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accountMu.Unlock()
	t.Cleanup(func() {
		profileCN, profileINTL = oldCN, oldIntl
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "synthetic-api-key"}); err != nil {
		t.Fatal(err)
	}
	const model = "glm-5.3-flash"
	var chatCalls atomic.Int32
	var mode atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/get-enterprise-user-usage":
			for key, value := range map[string]string{
				"X-Enterprise-Id": "ent-1", "X-Tenant-Id": "ent-1", "X-User-Id": "user-1",
				"Authorization": "Bearer synthetic-access", "X-Domain": "synthetic.example",
			} {
				if r.Header.Get(key) != value {
					t.Errorf("%s=%q want %q", key, r.Header.Get(key), value)
				}
			}
			if r.Header.Get("X-Trace-ID") == "" || r.Method != http.MethodPost {
				t.Error("missing trace or wrong method")
			}
			switch mode.Load() {
			case 0:
				io.WriteString(w, `{"code":0,"data":{"limitNum":2000,"credit":812.45}}`)
			case 1:
				io.WriteString(w, `{"code":0,"data":{"limitNum":-1,"credit":12.5}}`)
			case 2:
				io.WriteString(w, `{"code":0,"data":{"limitNum":2000,"credit":2000}}`)
			case 3:
				w.WriteHeader(503)
				io.WriteString(w, `{"msg":"synthetic failure"}`)
			case 4:
				io.WriteString(w, `{"code":0,"data":{"limitNum":2000}}`)
			case 5:
				io.WriteString(w, `{"code":0,"data":{"limitNum":-1,"credit":12.5}}`)
			}
		case "/v2/chat/completions":
			chatCalls.Add(1)
			if r.Header.Get("X-Enterprise-Id") != "ent-1" || r.Header.Get("X-No-Enterprise-Id") != "" {
				t.Error("chat lost enterprise identity")
			}
			if mode.Load() == 5 {
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, `{"code":14018,"msg":"credits exhausted"}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"id\":\"synthetic-completion\",\"model\":%q,\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"enterprise OK\"}}]}\n\n", model)
			io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"credit\":0.25,\"total_tokens\":150,\"prompt_tokens\":100,\"completion_tokens\":50}}\n\ndata: [DONE]\n\n")
		default:
			t.Errorf("enterprise flow called forbidden personal or other endpoint: %s", r.URL.Path)
			http.Error(w, "forbidden test endpoint", 400)
		}
	}))
	defer up.Close()
	profileCN.Base, profileCN.Origin = up.URL, up.URL
	cfg.HttpClient = up.Client()
	acc := &Account{Path: "enterprise.json", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "user-1", EnterpriseID: "ent-1"},
		Auth: StoredTokens{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix(), Domain: "synthetic.example"},
	}, QuotaKnown: true, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{model: {CostClass: modelCostPaid, QuotaBlocked: true}}}
	accountMu.Lock()
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	api := httptest.NewServer(newAPIHandler())
	defer api.Close()
	request := func(route, body, key string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, api.URL+route, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Trace-ID", "enterprise-http-test")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(raw)
	}
	chatBody := `{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hello"}]}`
	if status, _ := request("/v1/chat/completions", chatBody, ""); status != 401 || chatCalls.Load() != 0 {
		t.Fatal("unauthenticated request must not reach upstream")
	}
	if err := refreshAccountQuota(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		body := chatBody
		if route == "/v1/responses" {
			body = `{"model":"` + model + `","stream":false,"input":"hello"}`
		}
		if route == "/v1/messages" {
			body = `{"model":"` + model + `","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hello"}]}`
		}
		if status, raw := request(route, body, "synthetic-api-key"); status != 200 || !strings.Contains(raw, "enterprise OK") {
			t.Fatalf("%s status=%d response=%s", route, status, raw)
		}
	}
	if chatCalls.Load() != 3 {
		t.Fatalf("chat calls=%d want 3", chatCalls.Load())
	}
	mode.Store(1)
	if err := refreshAccountQuota(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	if pickProbeAccountForModel("cn", model) != acc {
		t.Fatal("unlimited enterprise account incorrectly excluded from explicit price-probe selection")
	}
	if status, raw := request("/v1/chat/completions", chatBody, "synthetic-api-key"); status != 200 || !strings.Contains(raw, "enterprise OK") {
		t.Fatalf("unlimited flow status=%d response=%s", status, raw)
	}
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(statusSnapshotFile)
	var snapshot statusSnapshot
	if err != nil || json.Unmarshal(saved, &snapshot) != nil || len(snapshot.Accounts) != 1 || !snapshot.Accounts[0].QuotaUnlimited {
		t.Fatal("unlimited flag missing from persistent snapshot")
	}
	accountMu.Lock()
	acc.QuotaUnlimited = false
	restoreAccountRuntimeStateLocked()
	accountMu.Unlock()
	if !acc.QuotaUnlimited || acc.QuotaExhausted {
		t.Fatal("unlimited state not restored")
	}
	for _, broken := range []int32{3, 4} {
		mode.Store(broken)
		if err := refreshAccountQuota(context.Background(), acc); err == nil {
			t.Fatal("broken query was accepted")
		}
		if !acc.QuotaUnlimited || acc.QuotaExhausted || !acc.PlanStale {
			t.Fatal("query failure overwrote last verified quota")
		}
	}
	mode.Store(2)
	if err := refreshAccountQuota(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	before := chatCalls.Load()
	if status, raw := request("/v1/chat/completions", chatBody, "synthetic-api-key"); status != 503 || !strings.Contains(raw, "额度阻断=1") || chatCalls.Load() != before {
		t.Fatalf("exhausted flow status=%d response=%s calls=%d", status, raw, chatCalls.Load())
	}
	mode.Store(5)
	if err := refreshAccountQuota(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	request("/v1/chat/completions", chatBody, "synthetic-api-key")
	before = chatCalls.Load()
	if status, _ := request("/v1/chat/completions", chatBody, "synthetic-api-key"); status != 503 || chatCalls.Load() != before {
		t.Fatal("unlimited display must not bypass a real upstream 14018 block")
	}
	// 中间件、选号拒绝、外部调用和返回全链路普通日志实际落盘。
	logs, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	var audit strings.Builder
	for _, p := range logs {
		raw, _ := os.ReadFile(p)
		audit.Write(raw)
	}
	for _, marker := range []string{"请求到达", "中间件-鉴权", "请求被拦截", "业务入口", "企业分配额度", "外部接口", "响应返回", "enterprise-http-test"} {
		if !strings.Contains(audit.String(), marker) {
			t.Fatalf("missing persisted audit stage %s", marker)
		}
	}
}

func TestEnterpriseCredentialChangeResetsUnlimitedQuota(t *testing.T) {
	isolatedGatewayConfig(t)
	oldAccounts := accounts
	t.Cleanup(func() { accounts = oldAccounts })
	auth := StoredAuth{Edition: "cn", Account: StoredAccount{UID: "same-user", EnterpriseID: "old-ent"},
		Auth: StoredTokens{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}}
	old, _ := json.Marshal(auth)
	if err := os.WriteFile("workbuddy-enterprise.json", old, 0600); err != nil {
		t.Fatal(err)
	}
	acc := &Account{Path: "workbuddy-enterprise.json", Auth: &auth, QuotaKnown: true, QuotaUnlimited: true, QuotaRemaining: -1,
		ModelStates: map[string]*modelRuntimeState{"m": {CostClass: modelCostPaid}}}
	accounts = []*Account{acc}
	updated := auth
	updated.Account.EnterpriseID = "new-ent"
	content, _ := json.Marshal(updated)
	if err := saveEditedCredential(context.Background(), acc.Path, credentialRevision(old), string(content), "enterprise-change"); err != nil {
		t.Fatal(err)
	}
	if acc.QuotaUnlimited || acc.QuotaKnown || acc.QuotaRemaining != 0 || acc.ModelStates != nil {
		t.Fatal("changed enterprise tenant retained old unlimited balance")
	}
}
