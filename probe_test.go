package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sseWithUsage(usage string) string {
	return strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":""}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hello world paragraph"},"finish_reason":""}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + usage + `}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
}

func withProbeAccount(t *testing.T, acc *Account, upstream http.HandlerFunc) *httptest.Server {
	t.Helper()
	chdirTemp(t)
	server := httptest.NewServer(upstream)
	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc.Auth.Edition = "cn"
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	t.Cleanup(func() {
		server.Close()
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	return server
}

func probeAccount() *Account {
	return &Account{Path: "intl.json", Auth: &StoredAuth{
		Edition: "intl",
		Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account: StoredAccount{UID: "u1"},
	}}
}

func TestProbeAccountModelLearnsFreeAndPaid(t *testing.T) {
	acc := probeAccount()
	withProbeAccount(t, acc, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseWithUsage(`{"credit":0,"total_tokens":500}`)))
	})
	res := probeAccountModel(context.Background(), acc, "hy3")
	if res.Status != "free" || res.Tokens != 500 {
		t.Fatalf("expected free, got %+v", res)
	}
	accountMu.Lock()
	state := acc.ModelStates["hy3"]
	accountMu.Unlock()
	if state == nil || state.CostClass != modelCostFree {
		t.Fatalf("free not learned: %+v", state)
	}
}

func TestProbeAccountModelTinySampleNotFree(t *testing.T) {
	acc := probeAccount()
	withProbeAccount(t, acc, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseWithUsage(`{"credit":0,"total_tokens":20}`)))
	})
	res := probeAccountModel(context.Background(), acc, "hy3")
	if res.Status != "unknown" {
		t.Fatalf("tiny sample must stay unknown, got %+v", res)
	}
	accountMu.Lock()
	state := acc.ModelStates["hy3"]
	accountMu.Unlock()
	if state != nil && state.CostClass == modelCostFree {
		t.Fatalf("tiny sample must not be free: %+v", state)
	}
}

func TestProbeAccountModelQuotaBlocksOnlyThatModel(t *testing.T) {
	acc := probeAccount()
	withProbeAccount(t, acc, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`))
	})
	res := probeAccountModel(context.Background(), acc, "deepseek-v4.1-flash")
	if res.Status != "quota" || res.HTTP != http.StatusTooManyRequests {
		t.Fatalf("expected quota, got %+v", res)
	}
	accountMu.Lock()
	blocked := acc.ModelStates["deepseek-v4.1-flash"]
	other := acc.ModelStates["hy3"]
	accountMu.Unlock()
	if blocked == nil || !blocked.QuotaBlocked {
		t.Fatalf("model should be quota blocked: %+v", blocked)
	}
	if other != nil {
		t.Fatalf("other model must not be touched: %+v", other)
	}
}

func TestHandleAdminProbeRejectsNonLoopback(t *testing.T) {
	chdirTemp(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/probe", strings.NewReader(`{}`))
	req.RemoteAddr = "203.0.113.9:12345"
	rec := httptest.NewRecorder()
	handleAdminProbe(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback should be rejected, got %d", rec.Code)
	}
}

func TestHandleAdminProbeReturnsResults(t *testing.T) {
	acc := probeAccount()
	withProbeAccount(t, acc, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseWithUsage(`{"credit":1.5,"total_tokens":800}`)))
	})
	body := `{"auth":"intl.json","models":["hy3"]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/probe", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	handleAdminProbe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out probeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Status != "paid" || out.Results[0].Credit != 1.5 {
		t.Fatalf("unexpected probe response: %+v", out)
	}
	if out.Summary["paid"] != 1 {
		t.Fatalf("unexpected summary: %+v", out.Summary)
	}
}

func TestProbeAuthMatches(t *testing.T) {
	acc := &Account{Path: "/opt/workbuddy-gateway/workbuddy4.json"}
	if !probeAuthMatches(acc, "") || !probeAuthMatches(acc, "workbuddy4.json") {
		t.Fatal("basename match should succeed")
	}
	if probeAuthMatches(acc, "workbuddy5.json") {
		t.Fatal("different file must not match")
	}
}
