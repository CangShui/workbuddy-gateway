package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRestoreOffConfigDoesNotChangeRequestOrChatFrames(t *testing.T) {
	off, on := false, true
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	for _, tc := range []struct {
		name string
		cfg  runtimeIdentityConfig
	}{
		{"omitted", runtimeIdentityConfig{}},
		{"false", runtimeIdentityConfig{ResponseRestore: &off}},
		{"identity_disabled", runtimeIdentityConfig{Enabled: &off, ResponseRestore: &on}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setIdentityConfig(tc.cfg)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			body := decodeObject(t, []byte(`{"messages":[{"role":"system","content":"You are Codex."}],"stream":true}`))
			before, _ := json.Marshal(body)
			planIdentityRestore(req, body)
			after, _ := json.Marshal(body)
			if identityRestoreFor(req).active() || !bytes.Equal(before, after) {
				t.Fatal("disabled feature changed request or created a restore plan")
			}
			if req.Context().Value(identityRequestContextKey{}) != nil || identitySnapshot().restoreTargets != nil {
				t.Fatal("disabled feature allocated request state or restore rules")
			}
			raw := `{"choices":[{"index":0,"delta":{"content":"WorkBuddy"},"finish_reason":"stop"}]}`
			resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: " + raw + "\n\ndata: [DONE]\n\n"))}
			rec := httptest.NewRecorder()
			streamChatResponseWithReplay(rec, req, resp, "m", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now(), nil)
			want := "data: " + cleanChunkJSON(raw) + "\n\ndata: [DONE]\n\n"
			if rec.Body.String() != want {
				t.Fatalf("disabled feature changed SSE bytes: %q", rec.Body.String())
			}
		})
	}
}

func TestRestoreDecisionAuditIsWrittenWithoutPrompt(t *testing.T) {
	isolatedGatewayConfig(t)
	enableRestore(t, map[string]string{"Codex": "WorkBuddy", "Claude Code": "WorkBuddy"})
	for _, instruction := range []string{
		"You are Codex.\nprivate-prompt-sentinel",
		"You are Codex.\nYou are Claude Code.\nprivate-prompt-sentinel",
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ensureDebugRequestContext(req, "restore-audit-test", time.Now())
		planIdentityRestore(req, map[string]any{"instructions": instruction})
	}
	var audit strings.Builder
	files, err := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		audit.Write(raw)
	}
	for _, token := range []string{"[响应身份还原]", "traceId=restore-audit-test", "阶段=计划建立", "阶段=身份判定", "结果=跳过"} {
		if !strings.Contains(audit.String(), token) {
			t.Fatalf("missing audit %q", token)
		}
	}
	if strings.Contains(audit.String(), "private-prompt-sentinel") {
		t.Fatal("audit leaked prompt")
	}
}

func TestRestorePlanKeepsRequestSnapshotOnReload(t *testing.T) {
	enableRestore(t, map[string]string{"Codex": "WorkBuddy"})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	planIdentityRestore(req, map[string]any{"instructions": "You are Codex."})
	off := false
	setIdentityConfig(runtimeIdentityConfig{ResponseRestore: &off, Target: "OtherTarget"})
	planIdentityRestore(req, map[string]any{"instructions": "You are Claude Code."})
	plan := identityRestoreFor(req)
	if !plan.active() || restoreIdentityText(plan.rules, "WorkBuddy") != "Codex" ||
		identityStateForRequest(req).settings.target != "WorkBuddy" {
		t.Fatal("reload changed the active request's inbound settings or restore plan")
	}
	next := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	planIdentityRestore(next, map[string]any{"instructions": "You are Codex."})
	if identityRestoreFor(next).active() {
		t.Fatal("new request did not use disabled setting")
	}
}

func TestRestoreRequiresUniqueExplicitDeclaration(t *testing.T) {
	enableRestore(t, map[string]string{
		"Claude Code": "WorkBuddy", "Claude": "WorkBuddy", "Codex": "WorkBuddy",
		"Grok Build": "WorkBuddy", "Grok": "WorkBuddy", "DeepSeek Harness": "WorkBuddy",
	})
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"incidental_before", `{"instructions":"Do not run Claude Code.\nYou are Codex, a coding agent."}`, "Codex"},
		{"incidental_after", `{"instructions":"You are Codex, a coding agent.\nDo not run Claude Code."}`, "Codex"},
		{"incidental_same_line", `{"instructions":"Do not run Claude Code. You are Codex, a coding agent."}`, "Codex"},
		{"description_mentions", `{"instructions":"You are Codex, a coding agent. Do not run Claude Code."}`, "Codex"},
		{"claude_official", `{"instructions":"You are Claude Code, Anthropic's official CLI for Claude."}`, "Claude Code"},
		{"system_incidental", `{"messages":[{"role":"system","content":"Do not run Claude Code."},{"role":"developer","content":"You are Codex, a coding agent."}]}`, "Codex"},
		{"developer_incidental", `{"messages":[{"role":"system","content":"You are Codex."},{"role":"developer","content":"Do not run Claude Code."}]}`, "Codex"},
		{"conflicting_roles", `{"messages":[{"role":"system","content":"You are Codex."},{"role":"developer","content":"You are Claude Code."}]}`, ""},
		{"conflicting_lines", `{"instructions":"You are Codex.\nYou are Claude Code."}`, ""},
		{"conflicting_line", `{"instructions":"You are Codex or Claude Code."}`, ""},
		{"conflicting_sentences", `{"instructions":"You are Codex. You are Claude Code."}`, ""},
		{"conflicting_sources", `{"instructions":"You are Codex.","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Claude Code."}]}]}`, ""},
		{"repeated_identity", `{"instructions":"You are Codex.\nYou are Codex, a coding agent."}`, "Codex"},
		{"mere_mention", `{"system":"Use Codex instead of Claude Code."}`, ""},
		{"negation", `{"instructions":"You are not Codex. Do not use Claude Code."}`, ""},
		{"quoted_example", `{"instructions":"> You are Claude Code.\nYou are Codex."}`, "Codex"},
		{"code_example", "{\"instructions\":\"```text\\nYou are Claude Code.\\n```\\nYou are Codex.\"}", "Codex"},
		{"code_only", "{\"instructions\":\"```\\nYou are Codex.\\n```\"}", ""},
		{"chinese", `{"system":"你是 Codex。请用中文回答。"}`, "Codex"},
		{"markdown_name", `{"system":"You are **Codex**, a coding agent."}`, "Codex"},
		{"longest_name", `{"system":"You are Grok Build."}`, "Grok Build"},
		{"powered_by", `{"system":"You are an AI agent powered by DeepSeek Harness."}`, "DeepSeek Harness"},
		{"contraction", `{"system":"You're Codex."}`, "Codex"},
		{"unknown", `{"system":"You are OtherClient."}`, ""},
		{"user_not_identity", `{"messages":[{"role":"user","content":"You are Codex."}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			planIdentityRestore(r, decodeObject(t, []byte(tc.body)))
			plan := identityRestoreFor(r)
			if tc.want == "" {
				if plan.active() {
					t.Fatalf("unexpected plan: %#v", plan.rules)
				}
				return
			}
			if !plan.active() || len(plan.rules) != 1 || plan.rules[0].to != tc.want {
				t.Fatalf("plan=%#v want one identity %q", plan.rules, tc.want)
			}
			if got := restoreIdentityText(plan.rules, "I am WorkBuddy."); got != "I am "+tc.want+"." {
				t.Fatalf("restored=%q", got)
			}
		})
	}
}

func TestRestorePreservesAddressesAtEveryStreamSplit(t *testing.T) {
	plan := standardRestorePlan()
	for _, tc := range []struct{ in, want string }{
		{"/opt/WorkBuddy/bin", "/opt/WorkBuddy/bin"},
		{`C:\Apps\WorkBuddy\settings.json`, `C:\Apps\WorkBuddy\settings.json`},
		{"https://example.test/WorkBuddy/docs", "https://example.test/WorkBuddy/docs"},
		{"https://WorkBuddy.example", "https://WorkBuddy.example"},
		{"WorkBuddy.example", "WorkBuddy.example"},
		{"WorkBuddy-app.example", "WorkBuddy-app.example"},
		{"service-WorkBuddy.example", "service-WorkBuddy.example"},
		{".WorkBuddy", ".WorkBuddy"},
		{"www.WorkBuddy.example/docs", "www.WorkBuddy.example/docs"},
		{"WorkBuddy.json", "WorkBuddy.json"},
		{"./WorkBuddy ../WorkBuddy ~/WorkBuddy", "./WorkBuddy ../WorkBuddy ~/WorkBuddy"},
		{`\\server\WorkBuddy\share`, `\\server\WorkBuddy\share`},
		{`"C:\Program Files\WorkBuddy App\settings.json"`, `"C:\Program Files\WorkBuddy App\settings.json"`},
		{"https://example.test/?client=WorkBuddy#WorkBuddy", "https://example.test/?client=WorkBuddy#WorkBuddy"},
		{"https://example.test/a(b)?client=WorkBuddy", "https://example.test/a(b)?client=WorkBuddy"},
		{"[WorkBuddy](https://example.test/WorkBuddy/docs)", "[Claude Code](https://example.test/WorkBuddy/docs)"},
		{"[WorkBuddy](../WorkBuddy)", "[Claude Code](../WorkBuddy)"},
		{"I am WorkBuddy. WorkBuddy helps.", "I am Claude Code. Claude Code helps."},
		{"I'm WorkBuddy. Visit https://example.test/WorkBuddy then WorkBuddy.", "I'm Claude Code. Visit https://example.test/WorkBuddy then Claude Code."},
		{"我是WorkBuddy。路径 /opt/WorkBuddy/bin。WorkBuddy", "我是Claude Code。路径 /opt/WorkBuddy/bin。Claude Code"},
		{"WorkBuddy.", "Claude Code."},
		{"WorkBuddy.😀", "Claude Code.😀"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := restoreIdentityText(plan.rules, tc.in); got != tc.want {
				t.Fatalf("aggregate=%q want %q", got, tc.want)
			}
			for first := 0; first <= len(tc.in); first++ {
				for second := first; second <= len(tc.in); second++ {
					s := newIdentityRestorer(plan)
					got := s.Write(tc.in[:first]) + s.Write(tc.in[first:second]) + s.Write(tc.in[second:]) + s.Flush()
					if got != tc.want {
						t.Fatalf("cuts=%d,%d got=%q want=%q", first, second, got, tc.want)
					}
					if len(s.carry) != 0 {
						t.Fatal("flush left pending text")
					}
				}
			}
		})
	}
}

func TestRestoreAddressContextDoesNotBufferLongTokens(t *testing.T) {
	plan := standardRestorePlan()
	prefix := "https://example.test/" + strings.Repeat("a", 20000) + "?client="
	s := newIdentityRestorer(plan)
	first := s.Write(prefix)
	if len(s.carry) > plan.hold+utf8.UTFMax+1 {
		t.Fatalf("buffered long address: %d", len(s.carry))
	}
	got := first + s.Write("Work") + s.Write("Buddy") + s.Flush()
	if got != prefix+"WorkBuddy" {
		t.Fatalf("lost address context across chunks")
	}
}

func TestRestoreAddressesAcrossResponseProtocols(t *testing.T) {
	const original = "I am WorkBuddy. /opt/WorkBuddy/bin C:\\Apps\\WorkBuddy\\settings.json https://example.test/?client=WorkBuddy"
	const expected = "I am Claude Code. /opt/WorkBuddy/bin C:\\Apps\\WorkBuddy\\settings.json https://example.test/?client=WorkBuddy"
	var upstream strings.Builder
	// 单字节分片覆盖协议回译器内部的地址状态连续性。
	for _, channel := range []string{"reasoning_content", "content"} {
		for i := range len(original) {
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{channel: original[i : i+1]},
			}}})
			upstream.WriteString("data: " + string(chunk) + "\n\n")
		}
	}
	upstream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(protocol+map[bool]string{true: "_stream", false: "_aggregate"}[stream], func(t *testing.T) {
				req := requestWithPlan(standardRestorePlan())
				rec := httptest.NewRecorder()
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream.String()))}
				acc := &Account{Path: "synthetic.json"}
				switch protocol {
				case "chat":
					if stream {
						streamChatResponseWithReplay(rec, req, resp, "m", 1, acc, &profileCN, time.Now(), nil)
					} else {
						writeChatAggregate(rec, req, resp, "m", 1, acc, &profileCN, time.Now())
					}
				case "responses":
					if stream {
						streamResponsesResponse(rec, req, resp, "m", 1, acc, &profileCN, time.Now())
					} else {
						writeResponsesAggregate(rec, req, resp, "m", 1, acc, &profileCN, time.Now())
					}
				case "messages":
					if stream {
						streamMessagesResponse(rec, req, resp, "m", 1, acc, &profileCN, time.Now(), true)
					} else {
						writeMessagesAggregate(rec, req, resp, "m", 1, acc, &profileCN, time.Now(), true)
					}
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				var content, reasoning strings.Builder
				if !stream {
					obj := decodeObject(t, rec.Body.Bytes())
					switch protocol {
					case "chat":
						message := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
						content.WriteString(strOf(message["content"]))
						reasoning.WriteString(strOf(message["reasoning_content"]))
					case "responses":
						for _, raw := range obj["output"].([]any) {
							item := raw.(map[string]any)
							if item["type"] == "message" {
								content.WriteString(item["content"].([]any)[0].(map[string]any)["text"].(string))
							} else if item["type"] == "reasoning" {
								reasoning.WriteString(item["summary"].([]any)[0].(map[string]any)["text"].(string))
							}
						}
					case "messages":
						for _, raw := range obj["content"].([]any) {
							part := raw.(map[string]any)
							content.WriteString(strOf(part["text"]))
							reasoning.WriteString(strOf(part["thinking"]))
						}
					}
				} else {
					for _, obj := range sseDataObjects(t, rec.Body.String()) {
						switch protocol {
						case "chat":
							for _, raw := range obj["choices"].([]any) {
								delta := raw.(map[string]any)["delta"].(map[string]any)
								content.WriteString(strOf(delta["content"]))
								reasoning.WriteString(strOf(delta["reasoning_content"]))
							}
						case "responses":
							if obj["type"] == "response.output_text.delta" {
								content.WriteString(strOf(obj["delta"]))
							} else if obj["type"] == "response.reasoning_summary_text.delta" {
								reasoning.WriteString(strOf(obj["delta"]))
							}
						case "messages":
							if obj["type"] == "content_block_delta" {
								delta := obj["delta"].(map[string]any)
								content.WriteString(strOf(delta["text"]))
								reasoning.WriteString(strOf(delta["thinking"]))
							}
						}
					}
				}
				if content.String() != expected || reasoning.String() != expected {
					t.Fatalf("content=%q reasoning=%q want=%q", content.String(), reasoning.String(), expected)
				}
			})
		}
	}
}
