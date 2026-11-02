package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrefixSanitizedOnAllProtocolEntrypoints(t *testing.T) {
	cases := []struct {
		name, path, body string
	}{
		{"chat", "/v1/chat/completions", `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,"messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness. Keep this system."},{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"deepseek-v4.1-flash","stream":false,"max_output_tokens":64,"input":[{"role":"developer","content":"You are an AI agent powered by DeepSeek Harness. Keep this system."},{"role":"user","content":"hi"}]}`},
		{"messages", "/v1/messages", `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":64,"system":"You are an AI agent powered by DeepSeek Harness. Keep this system.","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePolicies(t)
			resetStatsState(t)
			accountMu.Lock()
			accounts = []*Account{{Path: "synthetic.json", Edition: "cn", Auth: &StoredAuth{
				Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			}}}
			accountMu.Unlock()
			oldClient := cfg.HttpClient
			t.Cleanup(func() { cfg.HttpClient = oldClient })
			var captured map[string]any
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(raw, &captured); err != nil {
					t.Fatal(err)
				}
				return &http.Response{
					StatusCode: 200, Header: make(http.Header), Request: req,
					Body: io.NopCloser(strings.NewReader(conversationFixtureSSE(false))),
				}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			messages, ok := captured["messages"].([]any)
			if !ok || len(messages) == 0 {
				t.Fatalf("no upstream messages captured: %#v", captured)
			}
			var system string
			for _, raw := range messages {
				m := raw.(map[string]any)
				if m["role"] == "system" {
					system, _ = m["content"].(string)
					break
				}
			}
			if !strings.HasPrefix(system, testIdentityTargetPrefix) ||
				!strings.Contains(system, "Keep this system.") {
				t.Fatalf("upstream system not sanitized: %q", system)
			}
		})
	}
}

func TestPrefixSanitizeLogIsMetadataOnly(t *testing.T) {
	path, records := fullRequestTestLog(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ensureDebugRequestContext(req, "prefix-log-test", time.Now())
	applyIdentityReplacements(map[string]any{"messages": []any{msg("system", testDSHPrefix+". Keep private sentinel.")}}, req, 7)
	all := records()
	if len(all) != 1 || all[0]["event"] != "identity_replacements_applied" {
		t.Fatalf("unexpected log: %#v", all)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "DeepSeek Harness. Keep") {
		t.Fatal("log leaked prompt body")
	}
}

func TestAssistantIdentityBrandReplacement(t *testing.T) {
	cases := []struct{ input, want string }{
		{"您是 Codex，一个在 Codex CLI 中运行的编码代理。", "您是 WorkBuddy，一个在 WorkBuddy CLI 中运行的编码代理。"},
		{"You are an AI agent powered by DeepSeek Harness. Keep rules.", "You are WorkBuddy. Keep rules."},
		{"You are Claude Code, Anthropic's official CLI tool for Claude.", "You are WorkBuddy, Anthropic's official CLI tool for WorkBuddy."},
		{"You are ZCode. Keep rules.", "You are WorkBuddy. Keep rules."},
		{"You are Grok released by xAI. Keep rules.", "You are WorkBuddy. Keep rules."},
		{"You are a coding agent running in the Grok Build CLI, a terminal-based coding assistant.", "You are a coding agent running in the WorkBuddy CLI, a terminal-based coding assistant."},
		{"You are a Grok Build subagent. Keep delegated task.", "You are a WorkBuddy subagent. Keep delegated task."},
	}
	for _, tc := range cases {
		got, changed := normalizeAssistantIdentityText(tc.input)
		if !changed || got != tc.want {
			t.Fatalf("input=%q got=%q changed=%t want=%q", tc.input, got, changed, tc.want)
		}
	}
}

func TestAssistantIdentityDoesNotTouchUserOrToolText(t *testing.T) {
	obj := map[string]any{"messages": []any{
		msg("system", "You are Codex. Keep."),
		msg("assistant", "You are Claude Code. Keep."),
		msg("user", "You are Codex. User text."),
		msg("tool", "You are DeepSeek Harness. Tool text."),
	}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	applyIdentityReplacements(obj, r, 1)
	messages := obj["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "You are WorkBuddy. Keep." ||
		messages[1].(map[string]any)["content"] != "You are WorkBuddy. Keep." {
		t.Fatalf("assistant identity not normalized: %#v", messages[:2])
	}
	if messages[2].(map[string]any)["content"] != "You are Codex. User text." ||
		messages[3].(map[string]any)["content"] != "You are DeepSeek Harness. Tool text." {
		t.Fatal("user or tool text was modified")
	}
}

func TestResponsesReasoningSummaryDetailedIsPreserved(t *testing.T) {
	chat, _, err := responsesToChatRequestDetailed(decodeObject(t, []byte(`{
		"input":"hi","reasoning":{"effort":"max","summary":"detailed"}
	}`)), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_summary"] != "detailed" {
		t.Fatalf("summary lost during conversion: %#v", chat)
	}
	applyThinkingRules(chat, "deepseek-v4.1-flash")
	if chat["reasoning_summary"] != "detailed" {
		t.Fatalf("summary downgraded during outbound preparation: %#v", chat)
	}
}

func TestIdentityConfigTargetAndCustomReplacements(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Target: "WorkBuddy"})
	got, changed := normalizeAssistantIdentityText("You are an AI agent powered by DeepSeek Harness. Codex")
	if !changed || got != "You are WorkBuddy. WorkBuddy" {
		t.Fatalf("target override got=%q changed=%t", got, changed)
	}
	disabled := false
	setIdentityConfig(runtimeIdentityConfig{Enabled: &disabled, Target: "WorkBuddy"})
	got, changed = normalizeAssistantIdentityText("You are Codex. DeepSeek Harness")
	if changed || got != "You are Codex. DeepSeek Harness" {
		t.Fatalf("disabled identity still changed=%q", got)
	}
	setIdentityConfig(runtimeIdentityConfig{Target: "Assistant", Replacements: map[string]string{
		"Codex": "Coder", "DeepSeek Harness": "Buddy",
	}})
	got, changed = normalizeAssistantIdentityText("You are an AI agent powered by DeepSeek Harness. Codex")
	if !changed || got != "You are an AI agent powered by Buddy. Coder" {
		t.Fatalf("custom replacements got=%q changed=%t", got, changed)
	}
}
