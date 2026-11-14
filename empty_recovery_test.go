package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const recoveryHTML = "<!doctype html><html><body><canvas id=\"rain\"></canvas><script>document.body.dataset.ready='true'</script></body></html>"

func recoverySuccessSSE() string {
	chunk, _ := json.Marshal(map[string]any{
		"id": "rescue", "choices": []any{map[string]any{"index": 0,
			"delta": map[string]any{"content": recoveryHTML}, "finish_reason": "stop"}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 32, "total_tokens": 42, "credit": 0.05,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 0}},
	})
	return "data: " + string(chunk) + "\n\ndata: [DONE]\n\n"
}

func setRecoveryTestConfig(t *testing.T, enabled bool, profile string) {
	t.Helper()
	isolatePolicies(t)
	resetStatsState(t)
	loadOptionalStats()
	text := fmt.Sprintf(`{"concurrent":{"MaxAcc":1,"MaxGlobal":1},"upstream":{"requestProfile":%q,"emptyResponseRecovery":%t,"transientRetries":0,"enableFirstByteRetry":false}}`, profile, enabled)
	writeRuntimeFixture(t, text)
	watcher := runtimeConfigWatcher{}
	if !watcher.reload() {
		t.Fatal("recovery configuration was not applied")
	}
}

func recoveryRequestBody(path string, stream bool) map[string]any {
	body := map[string]any{"model": "deepseek-v4.1-flash", "stream": stream}
	messages := []any{map[string]any{"role": "user", "content": "rain HTML"}}
	switch path {
	case "/v1/responses":
		body["input"], body["max_output_tokens"] = "rain HTML", 128000
		body["reasoning"] = map[string]any{"effort": "max", "summary": "detailed"}
	case "/v1/messages":
		body["messages"], body["max_tokens"] = messages, 128000
		body["thinking"] = map[string]any{"type": "enabled", "effort": "max"}
	default:
		body["messages"], body["max_tokens"] = messages, 128000
		body["reasoning_effort"], body["reasoning_summary"] = "max", "detailed"
		body["thinking"] = map[string]any{"type": "enabled"}
	}
	return body
}

func decodeRecoverySent(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	reader := io.Reader(req.Body)
	if req.Header.Get("Content-Encoding") == "gzip" {
		zipper, err := gzip.NewReader(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer zipper.Close()
		reader = zipper
	}
	var sent map[string]any
	if err := json.NewDecoder(reader).Decode(&sent); err != nil {
		t.Fatal(err)
	}
	return sent
}

func serveRecoveryTest(t *testing.T, path string, stream bool, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = recoveryRequestBody(path, stream)
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
	req.Header.Set("X-Trace-ID", "empty-recovery-regression")
	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, req)
	return w
}

func useRecoveryAccount(t *testing.T) {
	t.Helper()
	oldRR := rrIndex
	t.Cleanup(func() { rrIndex = oldRR })
	accounts = []*Account{{Path: "recovery-synthetic.json", Auth: &StoredAuth{
		Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}}
}

func TestEmptyRecoveryAllProtocols(t *testing.T) {
	for _, profile := range []string{"legacy", "official"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s%s/stream=%t", profile, path, stream), func(t *testing.T) {
					setRecoveryTestConfig(t, true, profile)
					useRecoveryAccount(t)
					calls := 0
					var first map[string]any
					var firstHeaders http.Header
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						sent := decodeRecoverySent(t, req)
						data := budgetFixture(`{}`, "length")
						if calls == 1 {
							first, firstHeaders = sent, req.Header.Clone()
							if sent["reasoning_effort"] != "max" || sent["verbosity"] != nil {
								t.Errorf("first request must preserve MAX without injecting verbosity: %v", sent)
							}
						} else {
							if calls != 2 {
								t.Fatalf("unbounded generation: %d", calls)
							}
							thinking, _ := sent["thinking"].(map[string]any)
							if thinking["type"] != "disabled" || sent["reasoning_effort"] != nil ||
								sent["reasoning_summary"] != nil || sent["reasoning"] != nil {
								t.Errorf("recovery must explicitly disable all thinking controls: %v", sent)
							}
							for _, key := range []string{"model", "max_tokens", "messages", "tools"} {
								if !reflect.DeepEqual(first[key], sent[key]) {
									t.Errorf("recovery changed %s", key)
								}
							}
							if req.Header.Get("Authorization") != firstHeaders.Get("Authorization") ||
								req.Header.Get(conversationIDHeader) != firstHeaders.Get(conversationIDHeader) ||
								req.Header.Get(conversationRequestIDHeader) != firstHeaders.Get(conversationRequestIDHeader) ||
								req.Header.Get(conversationMessageIDHeader) == firstHeaders.Get(conversationMessageIDHeader) {
								t.Error("recovery must use same account/turn and a new model-call message ID")
							}
							data = recoverySuccessSSE()
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data))}, nil
					})}
					w := serveRecoveryTest(t, path, stream, nil)
					out := w.Body.String()
					if w.Code != 200 || calls != 2 || !strings.Contains(out, "canvas") ||
						!strings.Contains(out, `"effective_thinking":"disabled"`) ||
						strings.Contains(out, "synthetic reasoning") || strings.Contains(out, `"code":"output_budget_exhausted"`) {
						t.Fatalf("recovery did not deliver final HTML transparently: status=%d calls=%d %s", w.Code, calls, out)
					}
					if stream {
						marker := "data: [DONE]"
						if path == "/v1/messages" {
							marker = "event: message_stop"
						}
						if strings.Count(out, marker) != 1 {
							t.Errorf("must end exactly once: %s", out)
						}
					}
					requestStats.Lock()
					rows := append([]statsRequest(nil), requestStats.document.Requests...)
					requestStats.Unlock()
					if len(rows) != 2 || rows[0].Complete || !rows[1].Complete ||
						rows[0].Credit == nil || rows[1].Credit == nil ||
						math.Abs(*rows[0].Credit+*rows[1].Credit-1.30) > 0.000001 {
						t.Fatalf("must retain failed and recovered generation costs separately: %+v", rows)
					}
					modelStatsMu.Lock()
					var samples int64
					var tokenTime time.Duration
					if state := modelStats["deepseek-v4.1-flash"]; state != nil {
						for _, bucket := range state.Buckets {
							samples += bucket.TTFTSamples
							tokenTime += bucket.TTFTSum
						}
					}
					modelStatsMu.Unlock()
					if samples != 1 || rows[1].TTFTMS == nil ||
						math.Abs(*rows[1].TTFTMS-durationMilliseconds(tokenTime)) > 0.001 {
						t.Fatalf("recovery must retain one consistent first-token metric: samples=%d duration=%s row=%+v", samples, tokenTime, rows[1])
					}
					accountMu.Lock()
					blocked := accounts[0].Disabled || !accounts[0].CooldownUntil.IsZero()
					accountMu.Unlock()
					if blocked {
						t.Fatal("budget exhaustion must not disable/cool down the account")
					}
					files, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
					var audit strings.Builder
					for _, file := range files {
						data, err := os.ReadFile(file)
						if err != nil {
							t.Fatal(err)
						}
						audit.Write(data)
					}
					if !strings.Contains(audit.String(), "[空正文恢复] traceId=empty-recovery-regression") ||
						strings.Contains(audit.String(), "synthetic reasoning") || strings.Contains(audit.String(), "rain HTML") {
						t.Fatal("recovery audit must retain TraceID and stages, not prompt or reasoning contents")
					}
				})
			}
		}
	}
}

func TestEmptyRecoveryDoesNotReplayOtherResults(t *testing.T) {
	for _, tc := range []struct {
		name, delta, finish string
		enabled             bool
		thinkingDisabled    bool
		budget              int
	}{
		{"disabled", `{}`, "length", false, false, 128000},
		{"text", `{"content":"partial"}`, "length", true, false, 128000},
		{"tool_metadata", `{"tool_calls":[{"index":0,"id":"call"}]}`, "length", true, false, 128000},
		{"tool_arguments", `{"tool_calls":[{"index":0,"function":{"name":"lookup","arguments":"{"}}]}`, "length", true, false, 128000},
		{"normal_stop", `{}`, "stop", true, false, 128000},
		{"thinking_already_disabled", `{}`, "length", true, true, 128000},
		{"probe", `{}`, "length", true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRecoveryTestConfig(t, tc.enabled, "official")
			useRecoveryAccount(t)
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(budgetFixture(tc.delta, tc.finish)))}, nil
			})}
			body := recoveryRequestBody("/v1/chat/completions", true)
			body["max_tokens"] = tc.budget
			if tc.thinkingDisabled {
				body["thinking"] = map[string]any{"type": "disabled"}
			}
			serveRecoveryTest(t, "/v1/chat/completions", true, body)
			if calls != 1 {
				t.Fatalf("must not recover this result: calls=%d", calls)
			}
		})
	}
}

func TestEmptyRecoveryStopsAfterFailedRecovery(t *testing.T) {
	for _, failure := range []string{"length", "http", "missing_finish"} {
		t.Run(failure, func(t *testing.T) {
			setRecoveryTestConfig(t, true, "official")
			useRecoveryAccount(t)
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				status, data := 200, budgetFixture(`{}`, "length")
				if calls > 2 {
					t.Fatalf("must not retry a failed recovery: %d", calls)
				}
				if calls == 2 && failure == "http" {
					status, data = 500, `{"error":"synthetic upstream error"}`
				}
				if calls == 2 && failure == "missing_finish" {
					data = `data: {"choices":[{"delta":{"content":"incomplete"}}]}` + "\n\n"
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data))}, nil
			})}
			w := serveRecoveryTest(t, "/v1/chat/completions", true, nil)
			if calls != 2 || !strings.Contains(w.Body.String(), "output_budget_exhausted") ||
				strings.Contains(w.Body.String(), "data: [DONE]") {
				t.Fatalf("failed recovery must end explicitly: calls=%d %s", calls, w.Body)
			}
		})
	}
}

func TestEmptyRecoveryConfigDefaultUpgradeAndSnapshot(t *testing.T) {
	isolatePolicies(t)
	for _, raw := range []string{`{}`, `{"upstream":{}}`, `{"upstream":{"emptyResponseRecovery":false}}`} {
		c, err := parseRuntimeConfig([]byte(raw))
		if err != nil || c.Upstream.EmptyResponseRecovery {
			t.Fatalf("recovery must default off: %v %v", c, err)
		}
	}
	for _, raw := range []string{`{}`, `{"upstream":null}`, `{"upstream":{"idleTimeoutSeconds":45}}`, `{"upstream":{"emptyResponseRecovery":true}}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ensureRuntimeConfigFile(path); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		c, err := parseRuntimeConfig(got)
		if err != nil || c.Upstream.EmptyResponseRecovery != strings.Contains(raw, ":true") {
			t.Fatalf("upgrade must preserve configured value and add false: %s %v", got, err)
		}
		before := string(got)
		if err := ensureRuntimeConfigFile(path); err != nil {
			t.Fatal(err)
		}
		got, _ = os.ReadFile(path)
		if before != string(got) {
			t.Fatal("upgrade is not idempotent")
		}
	}
	r := httptest.NewRequest("POST", "/", nil)
	emptyResponseRecovery.Store(false)
	if snapshotEmptyResponseRecovery(r) {
		t.Fatal("default must be off")
	}
	emptyResponseRecovery.Store(true)
	if snapshotEmptyResponseRecovery(r) || !snapshotEmptyResponseRecovery(httptest.NewRequest("POST", "/", nil)) {
		t.Fatal("hot reload must not change an in-flight recovery policy")
	}
}

type delayedRecoveryReader struct {
	io.Reader
	once sync.Once
}

func (r *delayedRecoveryReader) Read(p []byte) (int, error) {
	r.once.Do(func() { time.Sleep(35 * time.Millisecond) })
	return r.Reader.Read(p)
}

func TestEmptyRecoveryHeartbeatAllStreamProtocols(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			setRecoveryTestConfig(t, true, "official")
			useRecoveryAccount(t)
			upstreamConfigMu.Lock()
			streamKeepalive = 3 * time.Millisecond
			upstreamConfigMu.Unlock()
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				var reader io.Reader = strings.NewReader(recoverySuccessSSE())
				if calls == 1 {
					reader = &delayedRecoveryReader{Reader: strings.NewReader(budgetFixture(`{}`, "length"))}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(reader)}, nil
			})}
			w := serveRecoveryTest(t, path, true, nil)
			if calls != 2 || !strings.Contains(w.Body.String(), ": keepalive\n\n") {
				t.Fatalf("buffered reasoning must keep the SSE connection alive: %s", w.Body)
			}
			length := w.Body.Len()
			time.Sleep(10 * time.Millisecond)
			if w.Body.Len() != length {
				t.Fatal("heartbeat wrote after handler completed")
			}
		})
	}
}

func TestEmptyRecoveryNormalChatStreamUnchanged(t *testing.T) {
	var baseline string
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			setRecoveryTestConfig(t, enabled, "official")
			useRecoveryAccount(t)
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(budgetFixture(`{"content":"normal response"}`, "stop")))}, nil
			})}
			w := serveRecoveryTest(t, "/v1/chat/completions", true, nil)
			if !enabled {
				baseline = w.Body.String()
			} else if w.Body.String() != baseline {
				t.Fatalf("successful upstream frames changed: %s", w.Body)
			}
			if calls != 1 {
				t.Fatalf("successful output must not trigger recovery: %d", calls)
			}
		})
	}
}

func TestEmptyRecoveryBufferedFirstTokenKeepsObservedTime(t *testing.T) {
	source := strings.NewReader(budgetFixture(`{"content":"answer"}`, "stop"))
	body := &prefixedResponseBody{Reader: source, Closer: io.NopCloser(source), firstToken: 42 * time.Millisecond}
	reader := newTTFTReader(body, time.Now().Add(-time.Second))
	observeFirstToken(reader, firstTokenChunk(t, `{"content":"answer"}`))
	if reader.duration() != 42*time.Millisecond {
		t.Fatalf("buffer release replaced the actual first token time: %s", reader.duration())
	}
}

func TestEmptyRecoveryPreservesUpstreamFragmentation(t *testing.T) {
	var frames strings.Builder
	for i := 0; i < 16; i++ {
		fmt.Fprint(&frames, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part\"},\"finish_reason\":null}]}\n\n")
	}
	fmt.Fprint(&frames, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0.05}}\n\ndata: [DONE]\n\n")
	response := recoveredSSE(&http.Response{}, []byte(frames.String()), map[string]any{"credit": 1.3}, time.Millisecond)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"content":"part"`) != 16 || strings.Count(string(raw), "data: [DONE]") != 1 {
		t.Fatal("recovery collapsed or duplicated original output frames")
	}
	completion, err := aggregateCompletion(strings.NewReader(string(raw)), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	obj := decodeObject(t, completion)
	msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != strings.Repeat("part", 16) || obj["usage"].(map[string]any)["credit"] != 1.3 {
		t.Fatalf("recovery changed body or did not replace usage: %s", completion)
	}
}

func TestEmptyRecoveryKeepsUnknownCreditUnknown(t *testing.T) {
	for _, missing := range []string{"first", "second", "neither"} {
		first := map[string]any{"completion_tokens": float64(128000), "credit": float64(0)}
		second := map[string]any{"completion_tokens": float64(32), "credit": float64(0), "quota": map[string]any{"percent": 2}}
		if missing == "first" {
			delete(first, "credit")
		} else if missing == "second" {
			delete(second, "credit")
		}
		meta := map[string]any{}
		merged := combineRecoveryUsage(first, second, meta)
		if _, exists := merged["credit"]; exists != (missing == "neither") {
			t.Errorf("missing=%s: unknown fee must not become free: %v", missing, merged)
		}
		if meta["usage_complete"] != (missing == "neither") || merged["completion_tokens"] != int64(128032) {
			t.Errorf("raw usage completeness or sum lost: %v", merged)
		}
		if !reflect.DeepEqual(merged["quota"], second["quota"]) {
			t.Error("quota snapshot must come from the latest generation, not be added")
		}
	}
}

func TestEmptyRecoveryPolicySnapshotControlsActualReplay(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			setRecoveryTestConfig(t, enabled, "official")
			useRecoveryAccount(t)
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				data := recoverySuccessSSE()
				if calls == 1 {
					emptyResponseRecovery.Store(!enabled)
					data = budgetFixture(`{}`, "length")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
			})}
			serveRecoveryTest(t, "/v1/chat/completions", true, nil)
			want := 1
			if enabled {
				want = 2
			}
			if calls != want {
				t.Fatalf("in-flight request changed policy after hot reload: got=%d want=%d", calls, want)
			}
		})
	}
}

func TestEmptyRecoveryToolResultAllProtocols(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", path, stream), func(t *testing.T) {
				setRecoveryTestConfig(t, true, "official")
				useRecoveryAccount(t)
				calls := 0
				cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					data := budgetFixture(`{}`, "length")
					if calls == 2 {
						data = budgetFixture(`{"tool_calls":[{"index":0,"id":"call_recovered","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, "tool_calls")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
				})}
				w := serveRecoveryTest(t, path, stream, nil)
				out := w.Body.String()
				if calls != 2 || w.Code != 200 || !strings.Contains(out, "call_recovered") || strings.Contains(out, "output_budget_exhausted") {
					t.Fatalf("recovered tool call lost: %s", out)
				}
				if stream && path == "/v1/responses" && strings.Count(out, "event: response.function_call_arguments.done") != 1 {
					t.Fatalf("must complete the recovered tool exactly once: %s", out)
				}
				if path == "/v1/messages" && strings.Count(out, `"type":"tool_use"`) != 1 {
					t.Fatalf("must expose the recovered tool exactly once: %s", out)
				}
			})
		}
	}
}

func TestEmptyRecoveryNetworkFailureIsNotStarvation(t *testing.T) {
	setRecoveryTestConfig(t, true, "official")
	useRecoveryAccount(t)
	calls := 0
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		reader := io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n"),
			responseReadError{context.DeadlineExceeded})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(reader)}, nil
	})}
	w := serveRecoveryTest(t, "/v1/chat/completions", true, nil)
	if calls != 1 || !strings.Contains(w.Body.String(), "stream_interrupted") || strings.Contains(w.Body.String(), "reasoning_recovery") {
		t.Fatalf("network failure must remain a network failure without another generation: %s", w.Body)
	}
}

func TestEmptyRecoveryCanceledRequestDoesNotGenerateAgain(t *testing.T) {
	setRecoveryTestConfig(t, true, "official")
	useRecoveryAccount(t)
	ctx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(recoveryRequestBody("/v1/chat/completions", true))
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw))).WithContext(ctx)
	calls := 0
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(budgetFixture(`{}`, "length")))}, nil
	})}
	newAPIHandler().ServeHTTP(httptest.NewRecorder(), req)
	if calls != 1 {
		t.Fatalf("canceled client must not trigger a second model call: %d", calls)
	}
}
