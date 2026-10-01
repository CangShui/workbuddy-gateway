package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProtocolAuthenticationAndAudit(t *testing.T) {
	oldKey := cfg.APIKey
	cfg.APIKey = "test-gateway-key"
	t.Cleanup(func() { cfg.APIKey = oldKey })
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses"} {
		for _, credential := range []string{"missing", "wrong", "bearer", "x-api-key"} {
			t.Run(path+"/"+credential, func(t *testing.T) {
				var audit bytes.Buffer
				oldWriter := log.Writer()
				log.SetOutput(&audit)
				t.Cleanup(func() { log.SetOutput(oldWriter) })
				called := false
				handler := requestAuditMiddleware(corsMiddleware(authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					if debugTraceID(r) != "integration-trace" {
						t.Errorf("TraceID lost: %q", debugTraceID(r))
					}
					if requestIDFor(r) != requestIDFor(r) {
						t.Error("requestID changed within one request")
					}
					w.WriteHeader(http.StatusNoContent)
				}))))
				req := httptest.NewRequest(http.MethodPost, path, nil)
				req.Header.Set("X-Trace-ID", "integration-trace")
				switch credential {
				case "wrong":
					req.Header.Set("x-api-key", "incorrect-secret")
				case "bearer":
					req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
				case "x-api-key":
					req.Header.Set("x-api-key", cfg.APIKey)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				authorized := credential == "bearer" || credential == "x-api-key"
				if called != authorized {
					t.Fatalf("business handler called=%v authorized=%v", called, authorized)
				}
				if authorized {
					if rec.Code != http.StatusNoContent {
						t.Fatalf("authorized status=%d", rec.Code)
					}
				} else {
					var body map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if rec.Code != http.StatusUnauthorized {
						t.Fatalf("unauthorized status=%d", rec.Code)
					}
					anthropic := strings.HasPrefix(path, "/v1/messages")
					if (body["type"] == "error") != anthropic {
						t.Fatalf("wrong protocol error shape: %s", rec.Body.String())
					}
					errBody := body["error"].(map[string]any)
					wantType := "invalid_api_key"
					if anthropic {
						wantType = "authentication_error"
					}
					if errBody["type"] != wantType {
						t.Fatalf("error type=%v want=%s", errBody["type"], wantType)
					}
					if !strings.Contains(audit.String(), "请求被拦截") {
						t.Error("missing rejection audit")
					}
				}
				for _, marker := range []string{"请求到达", "中间件-CORS", "响应返回", "traceId=integration-trace"} {
					if !strings.Contains(audit.String(), marker) {
						t.Errorf("missing audit marker %s", marker)
					}
				}
				if strings.Contains(audit.String(), cfg.APIKey) || strings.Contains(audit.String(), "incorrect-secret") {
					t.Fatal("audit leaked an API key")
				}
			})
		}
	}
}

func TestMessagesToolHistoryReachesUpstream(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"lookup","input":{"city":"北京"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":"晴天"},{"type":"text","text":"继续"}]}
	]}`
	messages := captureUpstreamMessages(t, "/v1/messages", body)
	if len(messages) != 4 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	call := messages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	result := messages[2].(map[string]any)
	if call["id"] != "call_a" || result["role"] != "tool" || result["tool_call_id"] != "call_a" || result["content"] != "晴天" {
		t.Fatalf("tool history not preserved: %#v", messages)
	}
}

func TestAnthropicRequestOptions(t *testing.T) {
	var body map[string]any
	err := json.Unmarshal([]byte(`{
		"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"描述"}]}],
		"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":8192},
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}
	}`), &body)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := anthropicToChatRequest(body, "m")
	if err != nil {
		t.Fatal(err)
	}
	if chat["max_tokens"] != 1024 || chat["reasoning_effort"] != "medium" || chat["tool_choice"] != "required" {
		t.Fatalf("options not converted: %#v", chat)
	}
	fn := chat["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "lookup" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("tool schema lost: %#v", fn)
	}
	parts := chat["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("image lost: %#v", parts)
	}
}

func TestAnthropicNamedToolChoiceUsesUpstreamString(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal([]byte(`{
		"messages":[{"role":"user","content":"call lookup"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"other","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"lookup"}
	}`), &body); err != nil {
		t.Fatal(err)
	}
	chat, err := anthropicToChatRequest(body, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	if chat["tool_choice"] != "required" {
		t.Fatalf("upstream requires a string tool_choice: %#v", chat["tool_choice"])
	}
	tools := chat["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named choice must only expose the selected tool: %#v", tools)
	}
	body["tool_choice"] = map[string]any{"type": "tool", "name": "missing"}
	if _, err := anthropicToChatRequest(body, "deepseek-v4.1-flash"); err == nil {
		t.Fatal("undefined named tool must be rejected before contacting upstream")
	}
}

const messagesTestSSE = `data: {"model":"m","choices":[{"delta":{"reasoning_content":"分析"},"finish_reason":""}]}

data: {"choices":[{"delta":{"content":"结果"},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}

data: [DONE]

`

func TestMessagesStreamingAndAggregate(t *testing.T) {
	for _, stream := range []bool{true, false} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(messagesTestSSE))}
		acc := &Account{Path: "messages-test.json"}
		if stream {
			streamMessagesResponse(rec, req, resp, "messages-test", 1, acc, &profileCN, time.Now(), true)
			out := rec.Body.String()
			for _, event := range []string{"message_start", "thinking_delta", "signature_delta", "text_delta", "input_json_delta", "message_delta", "message_stop"} {
				if !strings.Contains(out, `"type":"`+event+`"`) {
					t.Errorf("missing SSE event %s: %s", event, out)
				}
			}
			if strings.Index(out, "signature_delta") > strings.Index(out, "event: content_block_stop") {
				t.Fatal("thinking signature emitted after block stop")
			}
			if !strings.Contains(out, `"stop_reason":"tool_use"`) || !strings.Contains(out, `"input_tokens":60`) {
				t.Fatalf("stop/usage mapping failed: %s", out)
			}
		} else {
			writeMessagesAggregate(rec, req, resp, "messages-test", 1, acc, &profileCN, time.Now(), true)
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			content := out["content"].([]any)
			if len(content) != 3 || content[0].(map[string]any)["thinking"] != "分析" || content[1].(map[string]any)["text"] != "结果" {
				t.Fatalf("aggregate lost content: %#v", out)
			}
			tool := content[2].(map[string]any)
			if tool["input"].(map[string]any)["city"] != "北京" || out["stop_reason"] != "tool_use" {
				t.Fatalf("tool aggregation failed: %#v", out)
			}
			usage := out["usage"].(map[string]any)
			if usage["input_tokens"] != float64(60) || usage["cache_read_input_tokens"] != float64(40) || usage["output_tokens"] != float64(20) {
				t.Fatalf("usage mismatch: %#v", usage)
			}
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

type messagesFailReader struct{}

func (messagesFailReader) Read([]byte) (int, error) {
	return 0, errors.New("test interrupted upstream")
}

func TestMessagesIncompleteResponseIsNotSuccess(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, interrupted := range []bool{true, false} {
			var reader io.Reader = strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			if interrupted {
				reader = io.MultiReader(reader, messagesFailReader{})
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{Body: io.NopCloser(reader)}
			acc := &Account{Path: "messages-test.json"}
			if stream {
				streamMessagesResponse(rec, req, resp, "messages-test", 2, acc, &profileCN, time.Now(), false)
				if strings.Contains(rec.Body.String(), "event: message_stop") || !strings.Contains(rec.Body.String(), "event: error") {
					t.Fatalf("incomplete stream reported success: %s", rec.Body.String())
				}
			} else {
				writeMessagesAggregate(rec, req, resp, "messages-test", 2, acc, &profileCN, time.Now(), false)
				if rec.Code < 500 || !strings.Contains(rec.Body.String(), `"type":"error"`) {
					t.Fatalf("incomplete aggregate reported success: %s", rec.Body.String())
				}
			}
		}
	}
}

func TestMessagesCountTokensAndValidation(t *testing.T) {
	for _, tc := range []struct {
		path, method, body string
		status             int
	}{
		{"/v1/messages/count_tokens", "POST", `{"messages":[{"role":"user","content":"你好世界"}]}`, 200},
		{"/v1/messages/count_tokens", "POST", `{`, 400},
		{"/v1/messages/count_tokens", "GET", ``, 405},
		{"/v1/messages", "POST", `{`, 400},
		{"/v1/messages", "POST", `{"messages":[]}`, 400},
		{"/v1/messages", "GET", ``, 405},
	} {
		rec := httptest.NewRecorder()
		handler := handleMessages
		if tc.path == "/v1/messages/count_tokens" {
			handler = handleCountTokens
		}
		requestAuditMiddleware(http.HandlerFunc(handler)).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.status {
			t.Fatalf("%s %s status=%d want=%d body=%s", tc.method, tc.path, rec.Code, tc.status, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if tc.status == 200 {
			if tokens, ok := out["input_tokens"].(float64); !ok || tokens <= 0 {
				t.Fatalf("missing token estimate: %#v", out)
			}
		} else if out["type"] != "error" {
			t.Fatalf("wrong error shape: %#v", out)
		}
	}
}
