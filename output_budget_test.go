package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func budgetFixture(delta, finish string) string {
	return `data: {"id":"budget","choices":[{"index":0,"delta":{"reasoning_content":"synthetic reasoning"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"budget","choices":[{"index":0,"delta":` + delta + `,"finish_reason":"` + finish + `"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":128000,"total_tokens":128012,"completion_tokens_details":{"reasoning_tokens":128000},"credit":1.25}}` + "\n\n" +
		"data: [DONE]\n\n"
}

// 真实路由回放预算耗尽：不访问模型、不新增生成、不降档，费用仍入账。
func TestOutputBudgetAllResponsePaths(t *testing.T) {
	for _, profile := range []string{requestProfileLegacy, requestProfileOfficial} {
		for _, tc := range []struct {
			name, delta, finish string
			empty               bool
		}{
			{"reasoning_only", `{}`, "length", true},
			{"whitespace", `{"content":" \n"}`, "length", true},
			{"terminal_text", `{"content":"partial HTML"}`, "length", false},
			{"partial_tool", `{"tool_calls":[{"index":0,"id":"call_budget","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}`, "length", false},
			{"normal_text", `{"content":"complete HTML"}`, "stop", false},
			{"normal_tool", `{"tool_calls":[{"index":0,"id":"call_budget","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, "tool_calls", false},
		} {
			for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s%s/stream=%t", profile, tc.name, path, stream), func(t *testing.T) {
						isolatePolicies(t)
						setRequestProfile(profile)
						resetStatsState(t)
						loadOptionalStats()
						oldRR := rrIndex
						t.Cleanup(func() { rrIndex = oldRR })
						accounts = []*Account{{Path: "budget-synthetic.json", Auth: &StoredAuth{
							Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix()},
						}}}
						calls := 0
						cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
							calls++
							var sent map[string]any
							reader := io.Reader(req.Body)
							if req.Header.Get("Content-Encoding") == "gzip" {
								compressed, err := gzip.NewReader(req.Body)
								if err != nil {
									t.Fatal(err)
								}
								defer compressed.Close()
								reader = compressed
							}
							if err := json.NewDecoder(reader).Decode(&sent); err != nil {
								t.Fatal(err)
							}
							if sent["reasoning_effort"] != "max" || sent["max_tokens"] != float64(128000) {
								t.Errorf("must preserve MAX and explicit budget: %v", sent)
							}
							if th, ok := sent["thinking"].(map[string]any); ok && th["type"] == "disabled" {
								t.Error("must not silently disable thinking")
							}
							return &http.Response{StatusCode: 200, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(budgetFixture(tc.delta, tc.finish)))}, nil
						})}
						body := map[string]any{"model": "deepseek-v4.1-flash", "stream": stream}
						switch path {
						case "/v1/responses":
							body["input"], body["max_output_tokens"] = "solar system HTML", 128000
							body["reasoning"] = map[string]any{"effort": "max"}
						case "/v1/messages":
							body["messages"] = []any{map[string]any{"role": "user", "content": "solar system HTML"}}
							body["max_tokens"] = 128000
							body["thinking"] = map[string]any{"type": "enabled", "effort": "max"}
						default:
							body["messages"] = []any{map[string]any{"role": "user", "content": "solar system HTML"}}
							body["max_tokens"], body["reasoning_effort"] = 128000, "max"
							body["thinking"] = map[string]any{"type": "enabled"}
						}
						raw, _ := json.Marshal(body)
						req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
						req.Header.Set("X-Trace-ID", "output-budget-regression")
						w := httptest.NewRecorder()
						newAPIHandler().ServeHTTP(w, req)
						out := w.Body.String()
						if calls != 1 {
							t.Fatalf("budget exhaustion must not replay or switch accounts: calls=%d", calls)
						}
						accountMu.Lock()
						accountBlocked := accounts[0].Disabled || !accounts[0].CooldownUntil.IsZero() ||
							modelStateLocked(accounts[0], "deepseek-v4.1-flash").QuotaBlocked
						accountMu.Unlock()
						if accountBlocked {
							t.Error("output-budget exhaustion must not disable or cool down the account")
						}
						truncated := tc.finish == "length"
						switch {
						case path == "/v1/responses":
							if w.Code != 200 {
								t.Fatalf("Responses status=%d: %s", w.Code, out)
							}
							var response map[string]any
							if stream {
								for _, line := range strings.Split(out, "\n") {
									if !strings.HasPrefix(line, "data: {") {
										continue
									}
									var event map[string]any
									_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
									if r, ok := event["response"].(map[string]any); ok {
										response = r
									}
								}
							} else {
								response = decodeObject(t, w.Body.Bytes())
							}
							want := "completed"
							if truncated {
								want = "incomplete"
								if stream && (strings.Contains(out, "response.completed") ||
									strings.Contains(out, "response.function_call_arguments.done")) {
									t.Fatalf("truncated response must not signal completed output/tools: %s", out)
								}
								details, _ := response["incomplete_details"].(map[string]any)
								if details["reason"] != "max_output_tokens" {
									t.Errorf("missing output-budget reason: %v", response)
								}
							}
							if response["status"] != want {
								t.Errorf("response status=%v want=%s", response["status"], want)
							}
							for _, rawItem := range response["output"].([]any) {
								item := rawItem.(map[string]any)
								if truncated && item["status"] != "incomplete" {
									t.Errorf("truncated item marked complete: %v", item)
								}
							}
						case tc.empty:
							if !strings.Contains(out, `"code":"output_budget_exhausted"`) {
								t.Errorf("reasoning-only truncation needs an explicit error: %s", out)
							}
							if !stream && w.Code != http.StatusBadRequest {
								t.Errorf("empty budget error must not invite HTTP 5xx retries: %d", w.Code)
							}
							if stream && (strings.Contains(out, "[DONE]") || strings.Contains(out, "event: message_stop") ||
								strings.Contains(out, `"finish_reason":"length"`)) {
								t.Errorf("empty budget error must not also claim normal completion: %s", out)
							}
						default:
							if w.Code != 200 || strings.Contains(out, "output_budget_exhausted") {
								t.Errorf("valid text/tool result changed: status=%d %s", w.Code, out)
							}
							if truncated && path == "/v1/messages" && !strings.Contains(out, `"stop_reason":"max_tokens"`) {
								t.Errorf("Anthropic truncation reason lost: %s", out)
							}
						}
						requestStats.Lock()
						rows := append([]statsRequest(nil), requestStats.document.Requests...)
						requestStats.Unlock()
						if len(rows) != 1 {
							t.Fatalf("expected one billed generation: %v", rows)
						}
						if rows[0].Complete == truncated || rows[0].Credit == nil || *rows[0].Credit != 1.25 ||
							rows[0].OutputTokens == nil || *rows[0].OutputTokens != 128000 {
							t.Errorf("must retain cost but distinguish incomplete delivery: %+v", rows[0])
						}
					})
				}
			}
		}
	}
}

func TestOutputBudgetCustomToolIsIncomplete(t *testing.T) {
	isolatePolicies(t)
	toolMap := map[string]responsesToolMeta{
		"apply_patch": {UpstreamName: "apply_patch", Name: "apply_patch", Kind: responsesToolCustom},
	}
	sse := budgetFixture(`{"tool_calls":[{"index":0,"id":"call_custom","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"partial\"}"}}]}`, "length")
	req := withResponsesToolMap(httptest.NewRequest("POST", "/v1/responses", nil), toolMap)
	w := httptest.NewRecorder()
	streamResponsesResponse(w, req, &http.Response{Body: io.NopCloser(strings.NewReader(sse))},
		"deepseek-v4.1-flash", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now())
	out := w.Body.String()
	if !strings.Contains(out, "response.incomplete") ||
		!strings.Contains(out, `"type":"custom_tool_call"`) ||
		strings.Contains(out, "response.custom_tool_call_input.done") {
		t.Fatalf("truncated custom tools must not be announced as executable complete calls: %s", out)
	}
	raw, err := aggregateCompletion(strings.NewReader(sse), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := chatCompletionToResponsesWithTools(raw, "deepseek-v4.1-flash", toolMap)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeObject(t, encoded)
	item := response["output"].([]any)[1].(map[string]any) // reasoning + custom tool
	if item["type"] != "custom_tool_call" || item["status"] != "incomplete" || item["input"] != "partial" {
		t.Fatalf("aggregate custom tool lost truncation: %v", item)
	}
}

func TestOutputBudgetAuditDoesNotLogContents(t *testing.T) {
	isolatedGatewayConfig(t)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(req, "budget-audit-trace", time.Now())
	logOutputBudget(req, 1, "deepseek-v4.1-flash", "length", completionOutput{}, map[string]any{
		"completion_tokens": 128000, "completion_tokens_details": map[string]any{"reasoning_tokens": 128000},
		"private_field": "private-reasoning-sentinel",
	})
	files, err := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	if err != nil || len(files) == 0 {
		t.Fatalf("audit log missing: %v", err)
	}
	var text strings.Builder
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text.Write(data)
	}
	if !strings.Contains(text.String(), "budget-audit-trace") ||
		!strings.Contains(text.String(), "输出Token=128000 推理Token=128000") ||
		strings.Contains(text.String(), "private-reasoning-sentinel") {
		t.Fatalf("budget audit must retain TraceID and counts, not sensitive fields: %s", text.String())
	}
}
