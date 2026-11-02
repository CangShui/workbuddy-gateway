package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIdentityWildcardAnchoredAndBounded(t *testing.T) {
	const pattern = "You are(.*)released by xAI"
	const replacement = "You are WorkBuddy"
	for _, tc := range []struct{ name, input, want string }{
		{"provided_example", "You are Grok released by xAI. Keep the task.", "You are WorkBuddy. Keep the task."},
		{"custom_model", "You are Gemini 2.5 Pro released by xAI.\nKeep all tools.", "You are WorkBuddy.\nKeep all tools."},
		{"empty_dynamic", "You arereleased by xAI. Keep.", "You are WorkBuddy. Keep."},
		{"unicode", "You are 助手🚀 released by xAI. 保留指令。", "You are WorkBuddy. 保留指令。"},
		{"first_suffix_only", "You are A released by xAI. Text released by xAI. Keep.", "You are WorkBuddy. Text released by xAI. Keep."},
		{"middle", "Example: You are Grok released by xAI.", "Example: You are Grok released by xAI."},
		{"leading_space", " You are Grok released by xAI.", " You are Grok released by xAI."},
		{"leading_newline", "\nYou are Grok released by xAI.", "\nYou are Grok released by xAI."},
		{"case_sensitive", "you are Grok released by xAI.", "you are Grok released by xAI."},
		{"no_suffix", "You are an assistant. Keep all instructions.", "You are an assistant. Keep all instructions."},
		{"no_cross_lf", "You are Grok\nreleased by xAI. Keep.", "You are Grok\nreleased by xAI. Keep."},
		{"no_cross_cr", "You are Grok\rreleased by xAI. Keep.", "You are Grok\rreleased by xAI. Keep."},
		{"limit_ascii", "You are" + strings.Repeat("x", 256) + "released by xAI.tail", replacement + ".tail"},
		{"over_limit_ascii", "You are" + strings.Repeat("x", 257) + "released by xAI.tail", "You are" + strings.Repeat("x", 257) + "released by xAI.tail"},
		{"limit_unicode", "You are" + strings.Repeat("🚀", 256) + "released by xAI.tail", replacement + ".tail"},
		{"over_limit_unicode", "You are" + strings.Repeat("🚀", 257) + "released by xAI.tail", "You are" + strings.Repeat("🚀", 257) + "released by xAI.tail"},
		{"huge_unclosed_input", "You are" + strings.Repeat("x", 200000), "You are" + strings.Repeat("x", 200000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule, ok := compileIdentityReplacement(pattern, replacement)
			if !ok {
				t.Fatal("valid bounded wildcard was rejected")
			}
			got, _ := rule.replace(tc.input)
			if got != tc.want {
				t.Fatalf("output length=%d, want %d; bounded or anchored matching failed", len(got), len(tc.want))
			}
		})
	}
}

func TestIdentityWildcardRejectsUnsafePatternsWithoutFallback(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	for _, pattern := range []string{
		"", " ", "(.*)", "(.*)tail", "head(.*)", " (.*)tail",
		"head(.*) ", "head(.*)middle(.*)tail", "head\n(.*)tail", "head(.*)\rtail",
	} {
		t.Run(pattern, func(t *testing.T) {
			if _, ok := compileIdentityReplacement(pattern, "bad"); ok {
				t.Fatal("unsafe or empty pattern was accepted")
			}
			setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{pattern: "bad"}})
			const text = "You are Codex. Main branch (you will usually use this for PRs)"
			if got, changed := normalizeAssistantIdentityText(text); got != text || changed {
				t.Fatal("invalid rules must not enable default rewrites")
			}
		})
	}
}

func TestIdentityWildcardIsNotARegularExpression(t *testing.T) {
	rule, ok := compileIdentityReplacement("You are [A]+(.*)released by xAI$", "$1\\1")
	if !ok {
		t.Fatal("valid literal boundaries rejected")
	}
	for _, tc := range []struct{ input, want string }{
		{"You are [A]+ model released by xAI$. Rest.", "$1\\1. Rest."},
		{"You are AAA model released by xAI. Rest.", "You are AAA model released by xAI. Rest."},
	} {
		if got, _ := rule.replace(tc.input); got != tc.want {
			t.Fatal("regex metacharacters or replacement backreferences were interpreted")
		}
	}
	literal, ok := compileIdentityReplacement("a.*b", "literal")
	if !ok || literal.Wildcard {
		t.Fatal("unsupported regex syntax must remain ordinary literal text")
	}
	if got, _ := literal.replace("a.*b and axxb"); got != "literal and axxb" {
		t.Fatal("non-token regex syntax unexpectedly matched")
	}
}

func TestIdentityRulePriorityAndSingleWildcardMatch(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{
		"You are(.*)released by xAI":        "You are Generic",
		"You are Gemini released by xAI":    "You are Exact",
		"You are Gemini(.*)released by xAI": "You are Specific",
	}})
	if got, _ := normalizeAssistantIdentityText("You are Gemini released by xAI. Keep."); got != "You are Exact. Keep." {
		t.Fatalf("more specific literal should win: %q", got)
	}
	if got, _ := normalizeAssistantIdentityText("You are Gemini released by xAI. Keep code released by xAI."); got != "You are Exact. Keep code released by xAI." {
		t.Fatalf("wildcard consumed a later suffix after a literal replacement: %q", got)
	}
	if got, _ := normalizeAssistantIdentityText("You are Gemini 2.5 released by xAI. Keep."); got != "You are Specific. Keep." {
		t.Fatalf("more fixed text should win: %q", got)
	}
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{
		"A long(.*)tail": "A short tail",
		"A(.*)tail":      "second rule must not consume the new text",
	}})
	if got, _ := normalizeAssistantIdentityText("A long body tail. Keep."); got != "A short tail. Keep." {
		t.Fatalf("multiple wildcard rules rewrote the same block: %q", got)
	}
}

func TestEveryFormerHardcodedRuleRequiresConfigAndHonorsDisable(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	text := testDSHPrefix + ".\n" + testDSHGUI +
		"\nYou are Claude Code, Anthropic's official CLI for Claude." +
		"\nMain branch (you will usually use this for PRs)"
	disabled := false
	for _, tc := range []struct {
		name string
		c    runtimeIdentityConfig
	}{
		{"disabled_all_defaults", runtimeIdentityConfig{Enabled: &disabled}},
		{"no_matching_config_rules", runtimeIdentityConfig{Replacements: map[string]string{"unrelated": "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setIdentityConfig(tc.c)
			obj := map[string]any{"model": "deepseek-v4.1-flash", "max_tokens": 16,
				"messages": []any{msg("developer", text), msg("assistant", text), msg("user", text)}}
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r.Header.Set("X-Trace-ID", "config-only-rewrite-test")
			prepareUpstreamChatRequest(obj, r, 1, "deepseek-v4.1-flash")
			messages := obj["messages"].([]any)
			for _, raw := range messages {
				if raw.(map[string]any)["content"] != text {
					t.Fatal("a text rewrite bypassed the configured rules or disabled switch")
				}
			}
			if messages[0].(map[string]any)["role"] != "system" {
				t.Fatal("disabling text replacements must not disable protocol role normalization")
			}
		})
	}
}

func TestDefaultTemplateRulesNeverModifyUserOrToolMessages(t *testing.T) {
	isolatedGatewayConfig(t)
	setIdentityConfig(runtimeIdentityConfig{})
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	original := "You are Claude Code, Anthropic's official CLI for Claude.\nMain branch (you will usually use this for PRs)"
	expected, _ := normalizeAssistantIdentityText(original)
	if expected == original {
		t.Fatal("default migrated templates must still apply when configured")
	}
	obj := map[string]any{"messages": []any{msg("system", original), msg("assistant", original), msg("user", original), msg("tool", original)}}
	applyIdentityReplacements(obj, httptest.NewRequest("POST", "/", nil), 1)
	for i, raw := range obj["messages"].([]any) {
		want := original
		if i < 2 {
			want = expected
		}
		if raw.(map[string]any)["content"] != want {
			t.Fatalf("role scope incorrect at message %d", i)
		}
	}
}

func TestIdentityWildcardConfigHotReloadAndInvalidRuleAudit(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	oldHash, oldHasHash := runtimeAppliedHash, runtimeHasAppliedHash
	t.Cleanup(func() { runtimeAppliedHash, runtimeHasAppliedHash = oldHash, oldHasHash })
	writer := log.Writer()
	var output bytes.Buffer
	log.SetOutput(io.MultiWriter(writer, &output))
	t.Cleanup(func() { log.SetOutput(writer) })
	input := "You are Model released by xAI. Keep."
	watcher := runtimeConfigWatcher{}
	for i, tc := range []struct{ config, want string }{
		{`{"identity":{"replacements":{"You are(.*)released by xAI":"You are WorkBuddy","private-rule-sentinel(.*)":"private-value-sentinel"}}}`, "You are WorkBuddy. Keep."},
		{`{"identity":{"enabled":false,"replacements":{"You are(.*)released by xAI":"You are Changed"}}}`, input},
		{`{"identity":{"replacements":{"You are(.*)released by xAI":"You are Changed"}}}`, "You are Changed. Keep."},
		{`{"identity":{"replacements":{"You are(.*)":"invalid-without-suffix"}}}`, input},
	} {
		output.Reset()
		if err := os.WriteFile(runtimeConfigFile, []byte(tc.config), 0600); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatal(err)
			}
		} else if !watcher.reload() || watcher.lastError != nil {
			t.Fatalf("hot reload failed: %v", watcher.lastError)
		}
		if got, _ := normalizeAssistantIdentityText(input); got != tc.want {
			t.Fatalf("hot reload output=%q want=%q", got, tc.want)
		}
		if i == 0 || i == 3 {
			if !strings.Contains(output.String(), "[替换规则告警]") {
				t.Fatal("invalid rule must have a metadata-only warning")
			}
		}
		if strings.Contains(output.String(), "private-rule-sentinel") || strings.Contains(output.String(), "private-value-sentinel") {
			t.Fatal("replacement configuration leaked into logs")
		}
	}
}

func TestIdentityDefaultsAndUpgradePersistAllMigratedRules(t *testing.T) {
	standard := defaultIdentityReplacements("WorkBuddy")
	if len(standard) != 15 {
		t.Fatalf("default rule count=%d want=15", len(standard))
	}
	if standard["You are(.*)released by xAI"] != "You are WorkBuddy" {
		t.Fatal("requested wildcard must be persisted among default configuration rules")
	}
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var example runtimeFileConfig
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(example.Identity.Replacements, standard) {
		t.Fatal("example and fresh-install defaults differ")
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"identity":{"enabled":false,"target":"Assistant","replacements":{"DeepSeek Harness":"Buddy","You are(.*)released by xAI":"You are Custom","Main branch (you will usually use this for PRs)":"My branch"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(p); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var c runtimeFileConfig
	if err := json.Unmarshal(migrated, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Enabled == nil || *c.Identity.Enabled ||
		c.Identity.Replacements[defaultDSHIdentityText] != "You are Buddy" ||
		c.Identity.Replacements["You are(.*)released by xAI"] != "You are Custom" ||
		c.Identity.Replacements["Main branch (you will usually use this for PRs)"] != "My branch" {
		t.Fatal("upgrade overwrote an existing setting or failed to persist migrated defaults")
	}
	if err := ensureRuntimeConfigFile(p); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(migrated, again) {
		t.Fatal("upgrade must be idempotent")
	}
}

func TestIdentityWildcardDoesNotCrossOriginalBlocksAfterConcatenation(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{
		"You are(.*)released by xAI": "You are WorkBuddy",
	}})
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	first, second := "You are reviewing software", " released by xAI. Preserve this task."
	original := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "text", "text": first},
		map[string]any{"type": "text", "text": second},
	}}}}
	applyOriginalIdentityReplacements(original, r, 1)
	// Simulate Messages protocol concatenation. Neither original block matched;
	// conversion must not create a new eligible wildcard span.
	obj := map[string]any{"model": "deepseek-v4.1-flash", "max_tokens": 16,
		"messages": []any{msg("system", "Policy."), msg("assistant", first+second)}}
	prepareUpstreamChatRequest(obj, r, 1, "deepseek-v4.1-flash")
	if got := obj["messages"].([]any)[1].(map[string]any)["content"]; got != first+second {
		t.Fatalf("a wildcard crossed original block boundaries: %q", got)
	}
}

func TestIdentityWildcardProcessesInjectedPromptsSeparately(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{
		"You are(.*)released by xAI": "You are WorkBuddy",
	}})
	setSystemPromptConfig("You are A released by xAI. Keep A.", "You are B released by xAI. Keep B.")
	obj := map[string]any{"model": "deepseek-v4.1-flash", "max_tokens": 16, "messages": []any{msg("user", "hi")}}
	prepareUpstreamChatRequest(obj, httptest.NewRequest("POST", "/", nil), 1, "deepseek-v4.1-flash")
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != "You are WorkBuddy. Keep A.\n\nYou are WorkBuddy. Keep B." {
		t.Fatalf("fallback/force wildcard crossed independently injected blocks: %q", got)
	}
}

func TestIdentityRequestUsesOneConfigSnapshotAcrossPasses(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"You are(.*)released by xAI": "You are Old"}})
	r := httptest.NewRequest("POST", "/", nil)
	first := map[string]any{"messages": []any{msg("system", "You are A released by xAI.")}}
	applyOriginalIdentityReplacements(first, r, 1)
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"You are(.*)released by xAI": "You are New"}})
	later := map[string]any{"messages": []any{msg("system", "You are B released by xAI.")}}
	applyIdentityReplacements(later, r, 1)
	if got := later["messages"].([]any)[0].(map[string]any)["content"]; got != "You are Old." {
		t.Fatal("a hot update changed the config in the middle of an existing request")
	}
	next := map[string]any{"messages": []any{msg("system", "You are C released by xAI.")}}
	applyOriginalIdentityReplacements(next, httptest.NewRequest("POST", "/", nil), 2)
	if got := next["messages"].([]any)[0].(map[string]any)["content"]; got != "You are New." {
		t.Fatal("next request did not receive the new configuration")
	}
}

func TestUpgradeAutomaticallyCompletesAllNewReplacementDefaults(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	const wildcard = "You are(.*)released by xAI"
	const sample = "You are Gemini released by xAI. Preserve the rest."
	for _, tc := range []struct {
		name, original, want string
	}{
		{"no_identity", `{"logs":{"retentionDays":3},"systemPrompt":{"force":"keep"}}`, "You are WorkBuddy. Preserve the rest."},
		{"null_identity", `{"identity":null,"logs":{"retentionDays":3}}`, "You are WorkBuddy. Preserve the rest."},
		{"empty_replacements", `{"identity":{"enabled":true,"target":"","replacements":{}}}`, "You are WorkBuddy. Preserve the rest."},
		{"disabled_null_replacements", `{"identity":{"enabled":false,"target":"","replacements":null}}`, sample},
		{"prior_v142_identity", `{"identity":{"enabled":true,"target":"WorkBuddy","replacements":{"Codex":"Coder","workspace root":"custom base"}},"concurrent":{"MaxAcc":2,"MaxGlobal":7}}`, "You are WorkBuddy. Preserve the rest."},
		{"disabled_old_user", `{"identity":{"enabled":false,"target":"WorkBuddy","replacements":{"Codex":"Coder"}}}`, sample},
		{"custom_target", `{"identity":{"target":"Assistant","replacements":{"Codex":"Coder"}}}`, "You are Assistant. Preserve the rest."},
		{"custom_wildcard_preserved", `{"identity":{"replacements":{"You are(.*)released by xAI":"You are Local"}}}`, "You are Local. Preserve the rest."},
		{"wildcard_noop_preserved", `{"identity":{"replacements":{"You are(.*)released by xAI":"You are(.*)released by xAI"}}}`, sample},
		{"empty_destination_preserved", `{"identity":{"replacements":{"You are(.*)released by xAI":""}}}`, ". Preserve the rest."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			before := decodeObject(t, []byte(tc.original))
			if err := os.WriteFile(path, []byte(tc.original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ensureRuntimeConfigFile(path); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			after := decodeObject(t, raw)
			for key, value := range before {
				if key != "identity" && !reflect.DeepEqual(after[key], value) {
					t.Fatalf("unrelated config field %q changed", key)
				}
			}
			c, err := parseRuntimeConfig(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Identity.Replacements) != 15 {
				t.Fatalf("upgraded defaults=%d want=15", len(c.Identity.Replacements))
			}
			oldIdentity, _ := before["identity"].(map[string]any)
			if value, exists := oldIdentity["enabled"]; exists && (c.Identity.Enabled == nil || *c.Identity.Enabled != value.(bool)) {
				t.Fatal("existing enabled switch was overwritten")
			}
			target := c.Identity.Target
			if target == "" {
				target = "WorkBuddy"
			}
			expectedNew := map[string]string{
				defaultDSHIdentityText: "You are " + target,
				testDSHGUI:             testDSHNeutralGUI(),
				"You are Claude Code, Anthropic's official CLI for Claude.": "You are Claude Code, Anthropic's official CLI tool for Claude.",
				"Main branch (you will usually use this for PRs)":           "Default branch (you will usually use this for PRs)",
				wildcard: "You are " + target,
			}
			oldRules, _ := oldIdentity["replacements"].(map[string]any)
			for key, value := range expectedNew {
				if configured, exists := oldRules[key]; exists {
					value = configured.(string)
				}
				if c.Identity.Replacements[key] != value {
					t.Fatalf("new default %q missing or existing value overwritten", key)
				}
			}
			for key, value := range oldRules {
				if c.Identity.Replacements[key] != value.(string) {
					t.Fatalf("existing replacement %q was overwritten", key)
				}
			}
			setIdentityConfig(c.Identity)
			if got, _ := normalizeAssistantIdentityText(sample); got != tc.want {
				t.Fatalf("upgraded behavior=%q want %q", got, tc.want)
			}
			if err := ensureRuntimeConfigFile(path); err != nil {
				t.Fatal(err)
			}
			again, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, again) {
				t.Fatal("a complete upgraded configuration must not be rewritten on every startup")
			}
		})
	}
}

func TestDefaultWildcardIsConfigurationNotClientDetection(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	const original = "You are reviewing software released by xAI. Report compatibility issues."
	setIdentityConfig(runtimeIdentityConfig{})
	if got, _ := normalizeAssistantIdentityText(original); got != "You are WorkBuddy. Report compatibility issues." {
		t.Fatal("requested default pattern must apply to matching text regardless of client brand")
	}
	rules := defaultIdentityReplacements("WorkBuddy")
	rules["You are(.*)released by xAI"] = "You are(.*)released by xAI"
	setIdentityConfig(runtimeIdentityConfig{Replacements: rules})
	if got, changed := normalizeAssistantIdentityText(original); got != original || changed {
		t.Fatal("mapping the default wildcard to itself must preserve its actual matched text")
	}
}
