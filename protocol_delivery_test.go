package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitUpstreamSSELine(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		count      int
	}{
		{"single", `data: {"choices":[]}`, 0},
		{"heartbeat", ": keepalive", 0},
		{"metadata", `event: {"a":1}{"b":2}`, 0},
		{"done", "data: [DONE]", 0},
		{"two", `data: {"a":1}{"b":2}`, 2},
		{"bare", `{"a":1} {"b":2}`, 2},
		{"repeated_prefix", `data: data: {"a":1}{"b":2}`, 2},
		{"nested_escaped", `data: {"a":{"v":[1,{"text":"}{ and \"quoted\" and \\"}]}}{"b":2}`, 2},
		{"brackets_in_text", `data: {"text":"}{ and {}"}`, 0},
		{"truncated_tail", `data: {"a":1}{"b":`, 0},
		{"invalid_tail", `data: {"a":1}junk`, 0},
		{"array_tail", `data: {"a":1}[{"b":2}]`, 0},
		{"crlf", "data: {\"a\":1}\t{\"b\":2}\r\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := splitUpstreamSSELine(tc.line)
			if len(parts) != tc.count {
				t.Fatalf("parts=%v want count=%d", parts, tc.count)
			}
			for _, part := range parts {
				if !json.Valid([]byte(stripDataPrefix(part))) {
					t.Fatalf("split emitted invalid object: %q", part)
				}
			}
		})
	}
}

func TestGluedSSEDoesNotWaitForNextFrame(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		_, _ = io.WriteString(writer, "data: {\"a\":1}{\"b\":2}\n\n")
		// 不发送下一帧，也不关闭流。
	}()
	done := make(chan error, 1)
	go func() {
		scanner := newUpstreamSSEScanner(reader)
		for i := 0; i < 2; i++ {
			if !scanner.Scan() {
				done <- fmt.Errorf("missing object %d: %v", i, scanner.Err())
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("glued objects waited for another upstream frame")
	}
}

func deliverySSE(tools bool) string {
	deltas := []map[string]any{
		{"role": "assistant", "reasoning_content": "brief thought"},
	}
	finish := "stop"
	if tools {
		finish = "tool_calls"
		deltas = append(deltas,
			map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_delivery", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{"city":"`}}}},
			map[string]any{"tool_calls": []any{map[string]any{"index": 0,
				"function": map[string]any{"arguments": `rain"}`}}}},
		)
	} else {
		deltas = append(deltas, map[string]any{"content": `rain }{ "quoted"`}, map[string]any{"content": " animation"})
	}
	var frames []string
	for _, delta := range deltas {
		raw, _ := json.Marshal(map[string]any{"id": "delivery", "choices": []any{
			map[string]any{"index": 0, "delta": delta, "finish_reason": nil},
		}})
		frames = append(frames, string(raw))
	}
	raw, _ := json.Marshal(map[string]any{"id": "delivery", "choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish},
	}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 42, "total_tokens": 52, "credit": 0.05}})
	frames = append(frames, string(raw))
	return ": upstream heartbeat\n\ndata: " + strings.Join(frames, "") + "\n\ndata: [DONE]\n\n"
}

func glueSSEFixture(input string) string {
	var parts []string
	for _, line := range strings.Split(input, "\n") {
		payload := stripDataPrefix(line)
		if strings.HasPrefix(payload, "{") {
			parts = append(parts, payload)
		}
	}
	return "data: " + strings.Join(parts, "") + "\n\ndata: [DONE]\n\n"
}

func deliveryStreamText(t *testing.T, path, output string) (text, args string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		payload := stripDataPrefix(line)
		if !strings.HasPrefix(payload, "{") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("invalid downstream JSON: %v", err)
		}
		switch path {
		case "/v1/chat/completions":
			choices, _ := event["choices"].([]any)
			for _, raw := range choices {
				choice := raw.(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				text += strOf(delta["content"])
				calls, _ := delta["tool_calls"].([]any)
				for _, call := range calls {
					fn, _ := call.(map[string]any)["function"].(map[string]any)
					args += strOf(fn["arguments"])
				}
			}
		case "/v1/responses":
			switch event["type"] {
			case "response.output_text.delta":
				text += strOf(event["delta"])
			case "response.function_call_arguments.delta":
				args += strOf(event["delta"])
			}
		case "/v1/messages":
			if event["type"] == "content_block_delta" {
				delta, _ := event["delta"].(map[string]any)
				text += strOf(delta["text"])
				args += strOf(delta["partial_json"])
			}
		}
	}
	return
}

func TestGluedSSEAllResponsePaths(t *testing.T) {
	for _, profile := range []string{"legacy", "official"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				for _, tools := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s%s/stream=%t/tools=%t", profile, path, stream, tools), func(t *testing.T) {
						setRecoveryTestConfig(t, false, profile)
						useRecoveryAccount(t)
						calls := 0
						cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
							calls++
							return &http.Response{StatusCode: 200, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(deliverySSE(tools)))}, nil
						})}
						w := serveRecoveryTest(t, path, stream, nil)
						out := w.Body.String()
						if calls != 1 || w.Code != 200 || strings.Contains(out, "stream_closed_without_finish") {
							t.Fatalf("delivery failed: status=%d calls=%d output=%s", w.Code, calls, out)
						}
						requestStats.Lock()
						rows := append([]statsRequest(nil), requestStats.document.Requests...)
						requestStats.Unlock()
						if len(rows) != 1 || rows[0].Credit == nil || *rows[0].Credit != 0.05 ||
							rows[0].OutputTokens == nil || *rows[0].OutputTokens != 42 || !rows[0].Complete {
							t.Fatalf("usage or completion state lost: %+v", rows)
						}
						if stream {
							text, args := deliveryStreamText(t, path, out)
							if tools {
								if args != `{"city":"rain"}` {
									t.Fatalf("tool fragments changed: %q", args)
								}
							} else if text != `rain }{ "quoted" animation` {
								t.Fatalf("content changed: %q", text)
							}
						} else {
							var response map[string]any
							if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
								t.Fatal(err)
							}
							if !strings.Contains(out, "rain") || tools && !strings.Contains(out, "lookup") {
								t.Fatalf("aggregate lost payload: %s", out)
							}
						}
					})
				}
			}
		}
	}
}

func TestGluedSSERecovery(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, tools := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/tools=%t", path, stream, tools), func(t *testing.T) {
					setRecoveryTestConfig(t, true, "official")
					useRecoveryAccount(t)
					calls := 0
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						data := deliverySSE(tools)
						if calls == 1 {
							data = glueSSEFixture(budgetFixture(`{}`, "length"))
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data))}, nil
					})}
					w := serveRecoveryTest(t, path, stream, nil)
					if calls != 2 || w.Code != 200 || !strings.Contains(w.Body.String(), "rain") ||
						!strings.Contains(w.Body.String(), `"recovered":true`) {
						t.Fatalf("glued recovery failed: status=%d calls=%d %s", w.Code, calls, w.Body.String())
					}
				})
			}
		}
	}
}

func TestGluedSSEErrorCannotBecomeSuccess(t *testing.T) {
	for _, envelope := range []string{
		`{"error":{"code":11134,"message":"provider temporarily unavailable"}}`,
		`{"code":6020,"msg":"queued","data":{"retry_after":40}}`,
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, envelope), func(t *testing.T) {
					setRecoveryTestConfig(t, true, "official")
					useRecoveryAccount(t)
					calls := 0
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						data := "data: " + envelope + `{"choices":[{"delta":{"content":"must not deliver"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data))}, nil
					})}
					w := serveRecoveryTest(t, path, stream, nil)
					out := w.Body.String()
					if calls != 1 || accounts[0].Disabled || strings.Contains(out, "must not deliver") ||
						strings.Contains(out, "response.completed") || strings.Contains(out, "event: message_stop") ||
						strings.Contains(out, "data: [DONE]") || !strings.Contains(out, "上游错误") {
						t.Fatalf("error lost, recovered, or misclassified: calls=%d %s", calls, out)
					}
				})
			}
		}
	}
}

func TestGluedSSEDoesNotRecoverDeliveredOutput(t *testing.T) {
	for _, delta := range []string{`{"content":"partial HTML"}`, `{"tool_calls":[{"index":0,"id":"call_meta"}]}`} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, delta), func(t *testing.T) {
					setRecoveryTestConfig(t, true, "official")
					useRecoveryAccount(t)
					calls := 0
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						return &http.Response{StatusCode: 200, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader(glueSSEFixture(budgetFixture(delta, "length"))))}, nil
					})}
					serveRecoveryTest(t, path, stream, nil)
					if calls != 1 {
						t.Fatalf("content/tool delta must veto recovery: calls=%d", calls)
					}
				})
			}
		}
	}
}

func TestMessagesStructuredOutputMapping(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"weather": map[string]any{"type": "string", "enum": []any{"rain", "sun"}, "description": "literal }{ text"},
	}, "required": []any{"weather"}, "additionalProperties": false}
	body := map[string]any{
		"messages":      []any{map[string]any{"role": "user", "content": "return JSON"}},
		"thinking":      map[string]any{"type": "disabled"},
		"output_config": map[string]any{"effort": "max", "format": map[string]any{"type": "json_schema", "schema": schema}},
	}
	before, _ := json.Marshal(body)
	out, err := anthropicToChatRequest(body, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	format := out["response_format"].(map[string]any)
	js := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || js["name"] != "response" || js["strict"] != true ||
		!reflect.DeepEqual(js["schema"], schema) || out["reasoning_effort"] != "off" {
		t.Fatalf("schema or disabled thinking changed: %v", out)
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("mapping mutated caller request")
	}
}

func TestMessagesStructuredOutputValidation(t *testing.T) {
	for _, config := range []string{
		`{"format":"json"}`, `{"format":{"type":"text"}}`, `{"format":{"type":"json_schema"}}`,
		`{"format":{"type":"json_schema","schema":[]}}`, `{"format":{"type":"json_schema","schema":null}}`, `[]`,
	} {
		t.Run(config, func(t *testing.T) {
			setRecoveryTestConfig(t, false, "legacy")
			useRecoveryAccount(t)
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("invalid request must not reach upstream")
			})}
			body := recoveryRequestBody("/v1/messages", false)
			var output any
			if err := json.Unmarshal([]byte(config), &output); err != nil {
				t.Fatal(err)
			}
			body["output_config"] = output
			w := serveRecoveryTest(t, "/v1/messages", false, body)
			if w.Code != 400 || calls != 0 || !strings.Contains(w.Body.String(), "invalid_request_error") {
				t.Fatalf("invalid format was not rejected before upstream: %d calls=%d %s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestMessagesStructuredOutputOutbound(t *testing.T) {
	for _, profile := range []string{"legacy", "official"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", profile, stream), func(t *testing.T) {
				setRecoveryTestConfig(t, false, profile)
				useRecoveryAccount(t)
				calls := 0
				cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					sent := decodeRecoverySent(t, req)
					format, _ := sent["response_format"].(map[string]any)
					if format["type"] != "json_schema" || sent["reasoning_effort"] != "max" {
						t.Errorf("structured output or MAX lost: %v", sent)
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(recoverySuccessSSE()))}, nil
				})}
				body := recoveryRequestBody("/v1/messages", stream)
				body["output_config"] = map[string]any{"effort": "max", "format": map[string]any{
					"type": "json_schema", "schema": map[string]any{"type": "object"},
				}}
				w := serveRecoveryTest(t, "/v1/messages", stream, body)
				if calls != 1 || w.Code != 200 {
					t.Fatalf("structured output request failed: %d calls=%d", w.Code, calls)
				}
			})
		}
	}
}

func TestProtocolDeliveryAuditRedactsSchemaAndPayload(t *testing.T) {
	setRecoveryTestConfig(t, false, "official")
	useRecoveryAccount(t)
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(deliverySSE(false)))}, nil
	})}
	body := recoveryRequestBody("/v1/messages", true)
	body["output_config"] = map[string]any{"format": map[string]any{
		"type": "json_schema", "schema": map[string]any{"type": "object", "description": "private-schema-marker"},
	}}
	serveRecoveryTest(t, "/v1/messages", true, body)
	files, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}
	var audit strings.Builder
	for _, file := range files {
		if !strings.HasPrefix(file.Name(), "gateway-") || !strings.HasSuffix(file.Name(), ".log") {
			continue
		}
		raw, err := os.ReadFile(logDir + "/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		audit.Write(raw)
	}
	text := audit.String()
	for _, marker := range []string{"[请求到达]", "[中间件-鉴权]", "[协议转换]", "[流式协议]", "[响应返回]", "traceId=empty-recovery-regression"} {
		if !strings.Contains(text, marker) {
			t.Errorf("audit stage missing: %s", marker)
		}
	}
	for _, secret := range []string{"private-schema-marker", `rain }{ "quoted"`, "brief thought"} {
		if strings.Contains(text, secret) {
			t.Errorf("audit leaked schema, response, or reasoning: %q", secret)
		}
	}
}

func TestAccountRestrictionClassification(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, true},
		{`{"code":"11140","message":"Unusual Activity detected"}`, true},
		{`{"error":{"code":11140,"message":"request illegal"}}`, true},
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":11140,"msg":"content failed safety review"}`, false},
		{`{"code":11140,"msg":"该模型已被限制使用"}`, false},
		{`{"code":11134,"msg":"request illegal"}`, false},
		{`{"code":111400,"msg":"unusual activity"}`, false},
		{`{"msg":"unusual activity"}`, false},
		{`{"choices":[{"delta":{"content":"11140 unusual activity request illegal"}}]}`, false},
		{`{"code":6004,"msg":"模型繁忙"}`, false},
		{`{"error":{"code":"11140","message":"账号存在异常行为"}}`, true},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			if got := isAccountRestrictionPayload([]byte(tc.payload)); got != tc.want {
				t.Fatalf("restricted=%t want %t", got, tc.want)
			}
			if !tc.want && strings.Contains(tc.payload, `"code":11140,`) && isAuthFailure(403, tc.payload) {
				t.Fatal("non-account 11140 must not become an auth failure")
			}
		})
	}
}

func TestAccountRestrictionStopsOnlyCurrentRequestAndAccount(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, status := range []int{403, 200} {
				t.Run(fmt.Sprintf("%s/stream=%t/status=%d", path, stream, status), func(t *testing.T) {
					setRecoveryTestConfig(t, true, "official")
					useRecoveryAccount(t)
					first := accounts[0]
					first.Path = "restricted-synthetic.json"
					if err := os.WriteFile(first.Path, []byte("synthetic preserved credential"), 0600); err != nil {
						t.Fatal(err)
					}
					second := &Account{Path: "healthy-synthetic.json", Auth: &StoredAuth{
						Edition: "cn", Auth: StoredTokens{AccessToken: "other-synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix()},
					}}
					accounts = append(accounts, second)
					rrIndex = 0
					calls := 0
					restriction := `{"error":{"code":11140,"message":"账号存在异常行为，已被限制使用"}}`
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						data, code := recoverySuccessSSE(), 200
						if calls == 1 {
							code, data = status, restriction
							if status == 200 {
								data = "data: " + restriction + `{"choices":[{"delta":{"content":"must not deliver"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
							}
						}
						return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data))}, nil
					})}
					w := serveRecoveryTest(t, path, stream, nil)
					out := w.Body.String()
					errorMarker := "account_restricted"
					if path == "/v1/messages" && (!stream || status == 403) {
						errorMarker = "permission_error"
					}
					if calls != 1 || !first.Disabled || second.Disabled || !second.CooldownUntil.IsZero() ||
						!strings.Contains(out, errorMarker) || strings.Contains(out, "must not deliver") ||
						strings.Contains(out, "response.completed") || strings.Contains(out, "event: message_stop") ||
						strings.Contains(out, "data: [DONE]") {
						t.Fatalf("restriction rotated or forged success: calls=%d first=%t second=%t %s", calls, first.Disabled, second.Disabled, out)
					}
					if status == 403 || !stream {
						if w.Code != 403 {
							t.Errorf("restriction status=%d want 403", w.Code)
						}
					}
					raw, err := os.ReadFile(first.Path)
					if err != nil || string(raw) != "synthetic preserved credential" {
						t.Fatal("restriction deleted or modified credential")
					}
					if _, err := os.Stat(markerPath(first.Path)); err != nil {
						t.Fatal("restriction state was not persisted")
					}
					w = serveRecoveryTest(t, path, stream, nil)
					if calls != 2 || w.Code != 200 || !strings.Contains(w.Body.String(), "canvas") {
						t.Fatalf("healthy account was paused: status=%d calls=%d %s", w.Code, calls, w.Body.String())
					}
				})
			}
		}
	}
}

func TestAccountRestrictionDuringRecovery(t *testing.T) {
	for _, status := range []int{200, 403} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d%s/stream=%t", status, path, stream), func(t *testing.T) {
					setRecoveryTestConfig(t, true, "official")
					useRecoveryAccount(t)
					calls := 0
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						data, code := budgetFixture(`{}`, "length"), 200
						if calls > 1 {
							code = status
							data = `{"code":11140,"msg":"request illegal"}`
							if status == 200 {
								data = "data: " + data + "\n\ndata: [DONE]\n\n"
							}
						}
						return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data))}, nil
					})}
					w := serveRecoveryTest(t, path, stream, nil)
					if calls != 2 || !accounts[0].Disabled ||
						strings.Contains(w.Body.String(), "response.completed") ||
						strings.Contains(w.Body.String(), "event: message_stop") ||
						strings.Contains(w.Body.String(), "data: [DONE]") {
						t.Fatalf("recovery restriction lost or retried: calls=%d %s", calls, w.Body.String())
					}
				})
			}
		}
	}
}
