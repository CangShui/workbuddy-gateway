package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fullRequestTestLog(t *testing.T) (string, func() []map[string]any) {
	t.Helper()
	isolatedGatewayConfig(t)
	oldFull := debugFullRequests.Load()
	t.Cleanup(func() {
		debugFullRequests.Store(oldFull)
		_ = switchDebugLogging(false)
	})
	if err := switchDebugLogging(true); err != nil {
		t.Fatal(err)
	}
	debugFullRequests.Store(true)
	path := filepath.Join(logDir, "debug-"+time.Now().Format("2006-01-02")+".jsonl")
	return path, func() []map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var records []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			records = append(records, record)
		}
		return records
	}
}

func TestFullRequestsCaptureCompleteAndNonMutating(t *testing.T) {
	_, records := fullRequestTestLog(t)
	text := strings.Repeat("长对话代码", 25000) + "END-OF-PROMPT"
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}},
		"reasoning_effort": "max", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "test", "parameters": map[string]any{"type": "object"}}}},
		"metadata": map[string]any{"password": "private-password"}, "image_url": "https://example.test/image?token=private-url&size=large"})
	r := httptest.NewRequest("POST", "/v1/responses?api_key=private-query", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer private-auth")
	r.Header.Set("Cookie", "private-cookie")
	r.Header.Set("X-Custom-Token", "private-token")
	before := r.Header.Clone()
	ensureDebugRequestContext(r, "full-request-test", time.Now())
	debugFullIncomingHeaders(r)
	debugFullIncomingBody(r, raw, true, false)
	out := httptest.NewRequest("POST", "https://upstream.test/v2/chat/completions", bytes.NewReader(raw))
	out.Header.Set("X-Request-ID", "upstream-id")
	debugFullUpstreamRequest(r, out, raw, 0)
	debugFullUpstreamResponse(r, out, &http.Response{StatusCode: 400, Header: http.Header{"Set-Cookie": []string{"private-response-cookie"}}}, 0, true, true, nil)
	debugFullUpstreamError(r, &http.Response{StatusCode: 400, Request: out}, []byte(`{"code":11128,"msg":"blocked"}`))
	all := records()
	data, _ := json.Marshal(all)
	for _, secret := range []string{"private-password", "private-url", "private-query", "private-auth", "private-cookie", "private-token", "private-response-cookie"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	if len(all) != 5 || all[1]["trace_id"] != "full-request-test" || all[2]["trace_id"] != "full-request-test" {
		t.Fatal("missing correlated events")
	}
	body := all[2]["body"].(map[string]any)
	if body["messages"].([]any)[0].(map[string]any)["content"] != text {
		t.Fatal("body truncated")
	}
	if body["reasoning_effort"] != "max" || all[2]["body_truncated"] != false {
		t.Fatal("parameter hidden or truncated")
	}
	if !reflect.DeepEqual(r.Header, before) {
		t.Fatal("headers changed")
	}
	still, _ := io.ReadAll(r.Body)
	if !bytes.Equal(still, raw) {
		t.Fatal("request stream consumed or changed")
	}
	digests := all[0]["request"].(map[string]any)["credential_digests"].(map[string]any)
	if len(digests["header.Authorization"].(string)) != 64 {
		t.Fatal("credential comparison digest lost")
	}
}

func TestFullRequestsDisabledExcludedAndPartial(t *testing.T) {
	_, records := fullRequestTestLog(t)
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	debugFullRequests.Store(false)
	debugFullIncomingBody(r, []byte(`{"input":"DISABLED-TEXT"}`), true, false)
	debugFullRequests.Store(true)
	admin := httptest.NewRequest("POST", "/admin/api/config-edit", nil)
	debugFullIncomingBody(admin, []byte(`{"input":"ADMIN-TEXT"}`), true, false)
	debugFullIncomingBody(r, []byte(`{"input":"INCOMPLETE-SECRET`), false, true)
	all := records()
	raw, _ := json.Marshal(all)
	for _, value := range []string{"DISABLED-TEXT", "ADMIN-TEXT", "INCOMPLETE-SECRET"} {
		if bytes.Contains(raw, []byte(value)) {
			t.Fatal("excluded data captured")
		}
	}
	if len(all) != 1 || all[0]["body_capture"] != "invalid_or_partial_json_omitted" || all[0]["body_read_partial"] != true {
		t.Fatal("partial input not explicit")
	}
}

func TestFullRequestFreeTextSecretPatterns(t *testing.T) {
	token := "github_pat_" + strings.Repeat("a", 40)
	pem := "-----BEGIN PRIVATE KEY-----\nsynthetic private content\n-----END PRIVATE KEY-----"
	got := fullRequestRedact(map[string]any{"content": "test " + token + " " + pem, "number": json.Number("9007199254740993")})
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "synthetic private content") {
		t.Fatal("free text credential leaked")
	}
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatal("number precision lost")
	}
}

func TestFullRequestsConfigHotSwitch(t *testing.T) {
	fullRequestTestLog(t)
	var c runtimeFileConfig
	c.Debug.Enabled = true
	c.Debug.FullRequests = true
	if err := applyRuntimeConfig(c, true, "toggle-test"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	if !fullRequestLoggingEnabled(r) {
		t.Fatal("not enabled")
	}
	c.Debug.FullRequests = false
	if err := applyRuntimeConfig(c, true, "toggle-test"); err != nil {
		t.Fatal(err)
	}
	if fullRequestLoggingEnabled(r) {
		t.Fatal("not disabled")
	}
	c.Debug.FullRequests = true
	c.Debug.Enabled = false
	if err := applyRuntimeConfig(c, true, "toggle-test"); err != nil {
		t.Fatal(err)
	}
	if fullRequestLoggingEnabled(r) {
		t.Fatal("debug master switch ignored")
	}
}

func TestFullRequestsTransportReadsNeitherRequestNorResponse(t *testing.T) {
	_, records := fullRequestTestLog(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	out := httptest.NewRequest("POST", "https://upstream.test", nil)
	body := io.NopCloser(strings.NewReader("untouched stream"))
	debugFullUpstreamResponse(r, out, &http.Response{StatusCode: 200, Body: body}, 1, true, true, nil)
	got, _ := io.ReadAll(body)
	if string(got) != "untouched stream" {
		t.Fatal("response stream consumed")
	}
	debugFullUpstreamResponse(r, out, nil, 2, false, false, errors.New("network unavailable"))
	all := records()
	if len(all) != 2 || all[1]["network_error"] != true || all[1]["headers_written"] != false {
		t.Fatal("missing transport failure")
	}
}

func TestFullRequestsThreeProtocolChains(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			isolatePolicies(t)
			_, records := fullRequestTestLog(t)
			resetStatsState(t)
			oldRR := rrIndex
			t.Cleanup(func() { rrIndex = oldRR })
			accounts = []*Account{{Path: "synthetic.json", Edition: "cn", Auth: &StoredAuth{
				Auth: StoredTokens{AccessToken: "integration-secret", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			}}}
			var actual []byte
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				actual, _ = io.ReadAll(req.Body)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Request: req,
					Body: io.NopCloser(strings.NewReader(conversationFixtureSSE(false)))}, nil
			})}
			body := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"full input"}]}`
			if route == "/v1/responses" {
				body = `{"model":"deepseek-v4.1-flash","stream":false,"max_output_tokens":64,"input":"full input","reasoning":{"effort":"max"}}`
			}
			req := httptest.NewRequest("POST", route, strings.NewReader(body))
			w := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("HTTP %d", w.Code)
			}
			events := map[string]map[string]any{}
			for _, r := range records() {
				if strings.HasPrefix(r["event"].(string), "full_request_") {
					if r["trace_id"] != w.Header().Get("X-Trace-ID") {
						t.Fatal("trace mismatch")
					}
					events[r["event"].(string)] = r
				}
			}
			if events["full_request_inbound"] == nil || events["full_request_upstream_result"] == nil {
				t.Fatal("missing chain")
			}
			var actualBody any
			_ = json.Unmarshal(actual, &actualBody)
			if !reflect.DeepEqual(events["full_request_upstream"]["body"], actualBody) {
				t.Fatal("log differs from actual outbound body")
			}
			if events["full_request_inbound"]["body"].(map[string]any)["stream"] != false ||
				events["full_request_upstream"]["body"].(map[string]any)["stream"] != true {
				t.Fatal("pre/post conversion not distinguished")
			}
		})
	}
}

func TestFullRequestsAuthRejectionDoesNotReadBody(t *testing.T) {
	_, records := fullRequestTestLog(t)
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "expected-secret"}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"UNAUTHORIZED-BODY"}`))
	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("status %d", w.Code)
	}
	var seenHeaders, seenReject, seenReturn bool
	for _, v := range records() {
		switch v["event"] {
		case "full_request_inbound_headers":
			seenHeaders = true
		case "authentication_rejected":
			seenReject = true
		case "response_returned":
			seenReturn = true
		case "full_request_inbound", "full_request_upstream":
			t.Fatal("unauthorized body read")
		}
	}
	if !seenHeaders || !seenReject || !seenReturn {
		t.Fatal("missing middleware audit chain")
	}
}
