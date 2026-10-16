package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func conversationTuple(req *http.Request) [3]string {
	return [3]string{
		req.Header.Get(conversationIDHeader),
		req.Header.Get(conversationRequestIDHeader),
		req.Header.Get(conversationMessageIDHeader),
	}
}

func assertConversationTuple(t *testing.T, tuple [3]string) {
	t.Helper()
	for i, value := range tuple {
		if id, err := uuid.Parse(value); err != nil || id == uuid.Nil {
			t.Errorf("header %d should carry a nonzero UUID, got %q err=%v", i, value, err)
		}
	}
}

func TestConversationIDsLifecycleAndSource(t *testing.T) {
	isolatedGatewayConfig(t)
	clientID, turnID := uuid.NewString(), uuid.NewString()
	messageID := strings.ReplaceAll(uuid.NewString(), "-", "")
	req := httptest.NewRequest("POST", "/", nil)
	ensureDebugRequestContext(req, "conversation-header-test", time.Now())
	req.Header.Set(conversationIDHeader, clientID)
	req.Header.Set(conversationRequestIDHeader, turnID)
	req.Header.Set(conversationMessageIDHeader, messageID)
	before := req.Header.Clone()
	h := prepareConversationContext(req, nil)
	if h.ID != clientID || h.RequestID != turnID || h.MessageID != messageID {
		t.Fatalf("explicit official-format IDs were not preserved: %+v", h)
	}
	if !reflect.DeepEqual(req.Header, before) || prepareConversationContext(req, nil) != h {
		t.Fatal("prepare must not mutate inbound headers or regenerate the same logical request")
	}
	for _, prof := range []*upstreamProfile{&profileCN, &profileINTL} {
		out, _ := http.NewRequestWithContext(req.Context(), "POST", prof.chatURL(), nil)
		backendHeaders(out, &StoredAuth{}, prof)
		if got := conversationTuple(out); got != [3]string{clientID, turnID, messageID} {
			t.Fatalf("site=%s got=%v", prof.Key, got)
		}
	}
	a := prepareConversationContext(httptest.NewRequest("POST", "/", nil), nil)
	b := prepareConversationContext(httptest.NewRequest("POST", "/", nil), nil)
	if a.ID == b.ID || a.RequestID == b.RequestID || a.MessageID == b.MessageID || len(a.MessageID) != 32 {
		t.Fatalf("independent HTTP calls must be separate, with official 32-char message IDs: %+v %+v", a, b)
	}
	files, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	var audit string
	for _, file := range files {
		raw, _ := os.ReadFile(file)
		audit += string(raw)
	}
	if !strings.Contains(audit, "[会话头准备]") || !strings.Contains(audit, "conversation-header-test") {
		t.Fatal("missing trace-linked persisted conversation audit")
	}
	for _, secret := range []string{clientID, turnID, messageID} {
		if strings.Contains(audit, secret) {
			t.Fatal("raw client conversation IDs must not be logged")
		}
	}
}

func TestConversationSessionSeedsAreStableIsolatedAndValidated(t *testing.T) {
	isolatedGatewayConfig(t)
	makeRequest := func(key, header, seed string, body map[string]any) *conversationHeaders {
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if header != "" {
			req.Header.Set(header, seed)
		}
		return prepareConversationContext(req, body)
	}
	seed := "synthetic-private-session"
	first := makeRequest("caller-a", "X-Session-ID", seed, nil)
	second := makeRequest("caller-a", "X-Session-ID", seed, nil)
	otherCaller := makeRequest("caller-b", "X-Session-ID", seed, nil)
	if first.ID != second.ID || first.RequestID == second.RequestID || first.MessageID == second.MessageID {
		t.Fatal("same client session should keep conversation ID, but fresh HTTP calls need fresh turn/message IDs")
	}
	if first.ID == otherCaller.ID {
		t.Fatal("text session keys from different callers must not collapse into one upstream conversation")
	}
	for _, value := range []string{"", strings.Repeat("a", 513), "bad\nheader", "bad\x00header"} {
		if usableConversationSeed(value) != "" {
			t.Fatalf("unsafe/oversized seed accepted: %q", value)
		}
	}
	body := map[string]any{"metadata": map[string]any{"session_id": seed}, "model": "m"}
	old, _ := json.Marshal(body)
	fromMetadata := makeRequest("caller-a", "", "", body)
	newBody, _ := json.Marshal(body)
	if fromMetadata.ID != first.ID || string(old) != string(newBody) {
		t.Fatal("metadata session must survive conversion preparation without changing the JSON body")
	}
	// Valid explicit conversation header wins over a conflicting metadata session.
	explicit := uuid.NewString()
	h := makeRequest("caller-a", conversationIDHeader, explicit, body)
	if h.ID != explicit {
		t.Fatal("client conversation header must have priority")
	}
}

func conversationFixtureSSE(tool bool) string {
	if tool {
		return "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-test\",\"type\":\"function\",\"function\":{\"name\":\"noop\",\"arguments\":\"{}\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":5,\"total_tokens\":25,\"credit\":0.1}}\n\n" +
			"data: [DONE]\n\n"
	}
	return "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":5,\"total_tokens\":25,\"credit\":0.1}}\n\n" +
		"data: [DONE]\n\n"
}

// 3 协议 × 流式/聚合 × 国内/国际 × 文本/工具，共 24 条真实处理器路径。
func TestConversationHeadersAllProtocolsStreamsSitesAndTools(t *testing.T) {
	for _, site := range []string{"cn", "intl"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				for _, tool := range []bool{false, true} {
					name := site + path
					if stream {
						name += "/stream"
					} else {
						name += "/aggregate"
					}
					if tool {
						name += "/tool"
					} else {
						name += "/text"
					}
					t.Run(name, func(t *testing.T) {
						isolatePolicies(t)
						resetStatsState(t)
						oldRR := rrIndex
						t.Cleanup(func() { rrIndex = oldRR })
						accounts = []*Account{{Path: "synthetic.json", Edition: site, Auth: &StoredAuth{
							Edition: site, Auth: StoredTokens{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()},
						}}}
						sessionID := uuid.NewString()
						calls := 0
						cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
							calls++
							assertConversationTuple(t, conversationTuple(req))
							if req.Header.Get(conversationIDHeader) != sessionID {
								t.Error("original metadata was lost during protocol translation")
							}
							if req.URL.Host != strings.TrimPrefix(profileForEdition(site).Base, "https://") {
								t.Errorf("wrong upstream site: %s", req.URL.Host)
							}
							var payload map[string]any
							if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
								t.Fatal(err)
							}
							if payload["stream"] != true {
								t.Error("upstream must stay SSE even when client asks for aggregate JSON")
							}
							if _, ok := payload["tools"]; ok != tool {
								t.Errorf("tools lost or unexpectedly injected: tool=%t payload tools=%v", tool, ok)
							}
							if tool && payload["reasoning_effort"] != "high" {
								t.Errorf("explicit thinking mode changed: %v", payload["reasoning_effort"])
							}
							return &http.Response{StatusCode: 200, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(conversationFixtureSSE(tool)))}, nil
						})}
						body := map[string]any{"model": "deepseek-v4.1-flash", "stream": stream,
							"metadata": map[string]any{"session_id": sessionID}}
						switch path {
						case "/v1/responses":
							body["input"], body["max_output_tokens"] = "test", 64
							if tool {
								body["reasoning"] = map[string]any{"effort": "high"}
								body["tools"] = []any{map[string]any{"type": "function", "name": "noop",
									"parameters": map[string]any{"type": "object"}}}
							}
						case "/v1/messages":
							body["messages"] = []any{map[string]any{"role": "user", "content": "test"}}
							body["max_tokens"] = 64
							if tool {
								body["thinking"] = map[string]any{"type": "enabled", "effort": "high"}
								body["tools"] = []any{map[string]any{"name": "noop", "input_schema": map[string]any{"type": "object"}}}
							}
						default:
							body["messages"] = []any{map[string]any{"role": "user", "content": "test"}}
							body["max_tokens"] = 64
							if tool {
								body["reasoning_effort"] = "high"
								body["tools"] = []any{map[string]any{"type": "function",
									"function": map[string]any{"name": "noop", "parameters": map[string]any{"type": "object"}}}}
							}
						}
						raw, _ := json.Marshal(body)
						req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
						req.Header.Set("Content-Type", "application/json")
						w := httptest.NewRecorder()
						newAPIHandler().ServeHTTP(w, req)
						if w.Code != 200 || calls != 1 {
							t.Fatalf("code=%d upstreamCalls=%d body=%s", w.Code, calls, w.Body.String())
						}
						if strings.Contains(w.Body.String(), "\"error\":{") {
							t.Fatalf("protocol translation produced an error: %s", w.Body)
						}
						if !stream && !json.Valid(w.Body.Bytes()) {
							t.Fatal("aggregate response must be complete valid JSON")
						}
					})
				}
			}
		}
	}
}

func TestConversationIDsSurviveSafeRetryAndAccountSiteSwitch(t *testing.T) {
	isolatePolicies(t)
	oldBackoff := upstreamRetryBackoff
	upstreamRetryBackoff = time.Millisecond
	t.Cleanup(func() { upstreamRetryBackoff = oldBackoff })
	upstreamConfigMu.Lock()
	upstreamTransientRetries = 1
	upstreamConfigMu.Unlock()
	var tuples [][3]string
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		tuples = append(tuples, conversationTuple(req))
		if len(tuples) == 1 {
			return nil, io.EOF // 请求头尚未发送，允许安全重试。
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	for _, site := range []string{"cn", "intl"} {
		acc := &Account{Path: "test-" + site + ".json", Auth: &StoredAuth{
			Edition: site, Auth: StoredTokens{AccessToken: "test-" + site}}}
		resp, cancel, err := doUpstreamRequest(req, acc, profileForEdition(site), []byte(`{"model":"m"}`), 1, "test-retry")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		cancel()
	}
	if len(tuples) != 3 {
		t.Fatalf("expected retry + site switch, got %v", tuples)
	}
	for _, tuple := range tuples {
		assertConversationTuple(t, tuple)
		if tuple != tuples[0] {
			t.Fatalf("IDs changed within same logical call: %v", tuples)
		}
	}
}

func TestConversationIDsSurviveActualAccountFallback(t *testing.T) {
	isolatePolicies(t)
	resetStatsState(t)
	oldRR := rrIndex
	rrIndex = 0
	t.Cleanup(func() { rrIndex = oldRR })
	accounts = []*Account{
		{Path: "intl.json", Edition: "intl", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "intl", ExpiresAt: time.Now().Add(time.Hour).Unix()}}},
		{Path: "cn.json", Edition: "cn", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "cn", ExpiresAt: time.Now().Add(time.Hour).Unix()}}},
	}
	var tuples [][3]string
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		tuples = append(tuples, conversationTuple(req))
		if len(tuples) == 1 {
			return &http.Response{StatusCode: 429, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"code":14003,"msg":"too many requests"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(conversationFixtureSSE(false)))}, nil
	})}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"test"}]}`))
	newAPIHandler().ServeHTTP(w, req)
	if w.Code != 200 || len(tuples) != 2 || tuples[0] != tuples[1] {
		t.Fatalf("fallback failed or IDs changed: status=%d tuples=%v", w.Code, tuples)
	}
}

func TestConversationHeadersCoverBothProbePaths(t *testing.T) {
	isolatePolicies(t)
	acc := &Account{Path: "probe.json", Edition: "intl", Auth: &StoredAuth{
		Edition: "intl", Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}
	accounts = []*Account{acc}
	var tuples [][3]string
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		tuple := conversationTuple(req)
		assertConversationTuple(t, tuple)
		tuples = append(tuples, tuple)
		return &http.Response{StatusCode: 200, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(conversationFixtureSSE(false)))}, nil
	})}
	probeAccountModel(context.Background(), acc, "probe-model")
	probeModelPrice(acc, "probe-model")
	if len(tuples) != 2 || tuples[0] == tuples[1] {
		t.Fatalf("both independent probe invocations need fresh complete header families: %v", tuples)
	}
}
