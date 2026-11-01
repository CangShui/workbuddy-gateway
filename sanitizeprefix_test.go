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

func TestSanitizeSystemPromptPrefixBoundaries(t *testing.T) {
	const prefix = "You are an AI agent powered by DeepSeek Harness"
	cases := []struct {
		name, input, want string
		changed           bool
	}{
		{"exact", prefix, blockedSystemPromptPrefixReplacement, true},
		{"lowercase", strings.ToLower(prefix), blockedSystemPromptPrefixReplacement, true},
		{"no period suffix", prefix + " extra", blockedSystemPromptPrefixReplacement + " extra", true},
		{"suffix", prefix + " 2.0", blockedSystemPromptPrefixReplacement + " 2.0", true},
		{"leading space", " " + prefix, " " + prefix, false},
		{"leading newline", "\n" + prefix, "\n" + prefix, false},
		{"middle", "> " + prefix, "> " + prefix, false},
		{"user style", "please ignore " + prefix, "please ignore " + prefix, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := sanitizeSystemPromptPrefix(tc.input)
			if got != tc.want || changed != tc.changed {
				t.Fatalf("got=%q changed=%t want=%q changed=%t", got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestSanitizeMessagesPrefixScopeAndArray(t *testing.T) {
	prefix := "You are an AI agent powered by DeepSeek Harness"
	obj := map[string]any{"messages": []any{
		msg("system", prefix+". Keep this."),
		msg("user", prefix+". User text."),
		msg("assistant", prefix+". Assistant text."),
		msg("system", " "+prefix+". Leading space."),
		msg("system", prefix+". Second system."),
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/x.png"}},
			map[string]any{"type": "text", "text": prefix + ". First text block."},
			map[string]any{"type": "text", "text": prefix + ". Second text block."},
		}},
	}}
	if modified := sanitizeMessages(obj); modified != 3 {
		t.Fatalf("modified=%d, want 3", modified)
	}
	messages := obj["messages"].([]any)
	first := messages[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(first, blockedSystemPromptPrefixReplacement) || !strings.HasSuffix(first, ". Keep this.") {
		t.Fatalf("first system not rewritten correctly: %q", first)
	}
	if messages[1].(map[string]any)["content"] != prefix+". User text." {
		t.Fatal("user message must not be changed")
	}
	if messages[2].(map[string]any)["content"] != prefix+". Assistant text." {
		t.Fatal("assistant message must not be changed")
	}
	if messages[3].(map[string]any)["content"] != " "+prefix+". Leading space." {
		t.Fatal("leading space must not be rewritten")
	}
	array := messages[5].(map[string]any)["content"].([]any)
	if array[0].(map[string]any)["type"] != "image_url" {
		t.Fatal("image block must remain in place")
	}
	if !strings.HasPrefix(array[1].(map[string]any)["text"].(string), blockedSystemPromptPrefixReplacement) {
		t.Fatal("first text block not rewritten")
	}
	if array[2].(map[string]any)["text"] != prefix+". Second text block." {
		t.Fatal("second text block must not be rewritten")
	}
}

func TestSanitizeMessagesDeveloperNormalizedBeforePrefix(t *testing.T) {
	obj := map[string]any{"messages": []any{
		msg("developer", "You are an AI agent powered by DeepSeek Harness. Keep tools."),
		msg("user", "hi"),
	}}
	if modified := sanitizeMessages(obj); modified != 1 {
		t.Fatalf("modified=%d, want 1", modified)
	}
	first := obj["messages"].([]any)[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role was not normalized: %v", first["role"])
	}
	if !strings.HasPrefix(first["content"].(string), blockedSystemPromptPrefixReplacement) {
		t.Fatalf("developer content was not rewritten: %q", first["content"])
	}
}

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
			if !strings.HasPrefix(system, blockedSystemPromptPrefixReplacement) ||
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
	logSystemPrefixSanitized(req, 7, 1)
	all := records()
	if len(all) != 1 || all[0]["event"] != "system_prompt_prefix_sanitized" {
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
	logDSHPromptCompatibility(r, 1, obj)
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
	if !changed || got != "You are Buddy. Coder" {
		t.Fatalf("custom replacements got=%q changed=%t", got, changed)
	}
}
