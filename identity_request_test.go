package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIdentityIsOnlyConfigurableReplacementInOutboundRequests(t *testing.T) {
	const original = "Codex; workspace root; are paths the user; legacy phrase; Grok Build; Grok released by xAI"
	for _, tc := range []struct {
		name, config, want string
	}{
		{"defaults", `{}`, "WorkBuddy; workspace base; are paths that the user; legacy phrase; WorkBuddy; WorkBuddy"},
		{"disabled", `{"identity":{"enabled":false}}`, original},
		{
			"custom", `{"identity":{"replacements":{"workspace root":"custom base","legacy phrase":"identity result"}}}`,
			"Codex; custom base; are paths the user; identity result; Grok Build; Grok released by xAI",
		},
		{"retired_enabled_cannot_override_disabled", `{"identity":{"enabled":false},"promptCompatibility":{"enabled":true,"replacements":{"workspace root":"legacy result","legacy phrase":"legacy result"}}}`, original},
		{
			"retired_enabled_cannot_override_custom", `{"identity":{"replacements":{"workspace root":"custom base"}},"promptCompatibility":{"enabled":true,"replacements":{"workspace root":"legacy result","custom base":"legacy result","legacy phrase":"legacy result"}}}`,
			"Codex; custom base; are paths the user; legacy phrase; Grok Build; Grok released by xAI",
		},
		{
			"retired_enabled_does_not_add_rules", `{"promptCompatibility":{"enabled":true,"replacements":{"legacy phrase":"legacy result"}}}`,
			"WorkBuddy; workspace base; are paths that the user; legacy phrase; WorkBuddy; WorkBuddy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedGatewayConfig(t)
			t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
			c, err := parseRuntimeConfig([]byte(tc.config))
			if err != nil {
				t.Fatal(err)
			}
			if err := applyRuntimeConfig(c, false, "identity-only-test"); err != nil {
				t.Fatal(err)
			}
			obj := map[string]any{
				"model": "synthetic-model", "max_tokens": 16,
				"messages": []any{
					map[string]any{"role": "developer", "content": original},
					map[string]any{"role": "user", "content": original},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "text", "text": original},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": original}},
					}, "reasoning_content": original, "tool_calls": []any{
						map[string]any{"id": "call_identity_test", "type": "function", "function": map[string]any{
							"name": "test_tool", "arguments": `{"text":"` + original + `"}`,
						}},
					}},
					map[string]any{"role": "tool", "tool_call_id": "call_identity_test", "content": original},
				},
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{
					"name": "test_tool", "description": original, "parameters": map[string]any{
						"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string", "description": original}},
					},
				}}},
			}
			protected, _ := json.Marshal(obj["tools"])
			request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			request.Header.Set("X-Trace-ID", "identity-only-test")
			prepareUpstreamChatRequest(obj, request, 1, "synthetic-model")
			messages := obj["messages"].([]any)
			if got := messages[0].(map[string]any)["content"]; got != tc.want {
				t.Fatalf("system text=%q want %q", got, tc.want)
			}
			assistant := messages[2].(map[string]any)
			blocks := assistant["content"].([]any)
			if got := blocks[0].(map[string]any)["text"]; got != tc.want {
				t.Fatalf("assistant text=%q want %q", got, tc.want)
			}
			if got := blocks[1].(map[string]any)["image_url"].(map[string]any)["url"]; got != original {
				t.Fatal("image block was rewritten")
			}
			if messages[1].(map[string]any)["content"] != original || messages[3].(map[string]any)["content"] != original {
				t.Fatal("user text or tool result was rewritten")
			}
			if assistant["reasoning_content"] != original {
				t.Fatal("reasoning text was rewritten")
			}
			call := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if call["arguments"] != `{"text":"`+original+`"}` {
				t.Fatal("tool arguments were rewritten")
			}
			after, _ := json.Marshal(obj["tools"])
			if !bytes.Equal(protected, after) {
				t.Fatal("tool definitions were rewritten")
			}
		})
	}
}

func TestRetiredCompatibilityConfigDoesNotBlockStartupOrMigration(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := map[string]any{"enabled": true, "replacements": map[string]any{"legacy phrase": "legacy result"}}
	raw, _ := json.Marshal(map[string]any{"promptCompatibility": legacy, "systemPrompt": map[string]any{"force": "keep"}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeObject(t, migrated)
	if !reflect.DeepEqual(doc["promptCompatibility"], legacy) {
		t.Fatal("startup must not delete or silently move old custom rules")
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatalf("old field must not prevent startup: %v", err)
	}
	if text, _ := normalizeAssistantIdentityText("legacy phrase"); text != "legacy phrase" {
		t.Fatal("old rules must not be silently merged into identity")
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(migrated, again) {
		t.Fatal("complete migrated config must not be rewritten repeatedly")
	}
	if _, err := parseRuntimeConfig([]byte(`{"promptCompatibility":{},"unknownSetting":true}`)); err == nil {
		t.Fatal("retired-field compatibility must not disable validation of other unknown fields")
	}
}

func TestNewAndUpgradedConfigsDoNotAddRetiredCompatibility(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "upgrade"}[existing], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if existing {
				if err := os.WriteFile(path, []byte(`{"identity":{"target":"Keep"},"systemPrompt":{"force":"keep"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ensureRuntimeConfigFile(path); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			doc := decodeObject(t, raw)
			if _, exists := doc["promptCompatibility"]; exists {
				t.Fatal("removed field must not be generated")
			}
			identity := doc["identity"].(map[string]any)
			replacements := identity["replacements"].(map[string]any)
			for _, term := range []string{"Grok Build", "Grok", "Grok released by xAI"} {
				if replacements[term] != identity["target"] {
					t.Fatalf("identity rule %q=%v target=%v", term, replacements[term], identity["target"])
				}
			}
			if existing && (identity["target"] != "Keep" || doc["systemPrompt"].(map[string]any)["force"] != "keep") {
				t.Fatal("existing settings were overwritten")
			}
		})
	}
}

func TestRetiredCompatibilityWarningAndIdentityHotReload(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	oldHash, oldHasHash := runtimeAppliedHash, runtimeHasAppliedHash
	t.Cleanup(func() { runtimeAppliedHash, runtimeHasAppliedHash = oldHash, oldHasHash })
	var output bytes.Buffer
	writer := log.Writer()
	log.SetOutput(io.MultiWriter(writer, &output))
	t.Cleanup(func() { log.SetOutput(writer) })
	const privateRule = "synthetic-private-phrase-never-log"
	watcher := runtimeConfigWatcher{}
	for i, tc := range []struct {
		config, want string
		warn         bool
	}{
		{`{"identity":{"enabled":false},"promptCompatibility":{"enabled":true,"replacements":{"` + privateRule + `":"custom"}}}`, "workspace root", true},
		{`{"identity":{"replacements":{"workspace root":"custom base"}},"promptCompatibility":null}`, "custom base", true},
		{`{"identity":{}}`, "workspace base", false},
	} {
		output.Reset()
		if err := os.WriteFile(runtimeConfigFile, []byte(tc.config), 0o600); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatal(err)
			}
		} else if !watcher.reload() || watcher.lastError != nil {
			t.Fatalf("valid update must hot reload: %v", watcher.lastError)
		}
		// Startup repeats this after initializing file logging; hot reload uses
		// the same function with the request trace from applyRuntimeConfig.
		logIdentityRuntimeConfig("identity-reload-test")
		if text, _ := normalizeAssistantIdentityText("workspace root"); text != tc.want {
			t.Fatalf("reloaded text=%q want %q", text, tc.want)
		}
		hasWarning := strings.Contains(output.String(), "[废弃配置] traceId=identity-reload-test")
		if hasWarning != tc.warn {
			t.Fatalf("retired-field warning=%t want %t", hasWarning, tc.warn)
		}
		if strings.Contains(output.String(), privateRule) {
			t.Fatal("retired custom rule leaked into normal logs")
		}
	}
	output.Reset()
	if err := os.WriteFile(runtimeConfigFile, []byte(`{"identity":{"enabled":false},"unknownSetting":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if watcher.reload() || watcher.lastError == nil {
		t.Fatal("unknown configuration must still reject hot reload")
	}
	if text, _ := normalizeAssistantIdentityText("workspace root"); text != "workspace base" {
		t.Fatal("rejected reload must preserve the last working identity settings")
	}
	if !strings.Contains(output.String(), "结果=拒绝") {
		t.Fatal("rejected reload must have an audit entry")
	}
}

func TestGrokBuildModelLabelIdentity(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	const original = "You are Gemini 2.5 Pro released by xAI. Keep the task instructions."
	for _, tc := range []struct {
		name, config, input, want string
	}{
		{"default", `{}`, original, "You are WorkBuddy. Keep the task instructions."},
		{"versioned_label", `{}`, "You are GPT-5.2 released by xAI. Keep tools.", "You are WorkBuddy. Keep tools."},
		{"custom_target", `{"identity":{"target":"Assistant"}}`, original, "You are Assistant. Keep the task instructions."},
		{"disabled", `{"identity":{"enabled":false}}`, original, original},
		{"custom_rules_only", `{"identity":{"replacements":{"keep":"retain"}}}`, original, original},
		{"custom_identity_rule", `{"identity":{"replacements":{"Grok released by xAI":"Local Assistant"}}}`, original, "You are Local Assistant. Keep the task instructions."},
		{"explicit_noop", `{"identity":{"replacements":{"Grok released by xAI":"Grok released by xAI"}}}`, original, original},
		{"exact_label_override", `{"identity":{"replacements":{"Grok released by xAI":"WorkBuddy","Gemini 2.5 Pro released by xAI":"Local Assistant"}}}`, original, "You are Local Assistant. Keep the task instructions."},
		{"body_mention", `{}`, "Example: " + original, "Example: " + original},
		{"later_sentence", `{}`, "You are helpful. A tool was released by xAI.", "You are helpful. A tool was released by xAI."},
		{"later_line", `{}`, "You are helpful.\nGemini released by xAI.", "You are helpful.\nGemini released by xAI."},
		{"domain_suffix", `{}`, "You are using tools released by xAI.example.", "You are using tools released by xAI.example."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseRuntimeConfig([]byte(tc.config))
			if err != nil {
				t.Fatal(err)
			}
			setIdentityConfig(c.Identity)
			got, changed := normalizeAssistantIdentityText(tc.input)
			if got != tc.want || changed != (tc.input != tc.want) {
				t.Fatalf("text=%q changed=%t want=%q", got, changed, tc.want)
			}
			if again, changed := normalizeAssistantIdentityText(got); again != got || changed {
				t.Fatalf("repeated normalization changed text: %q", again)
			}
		})
	}
}

func TestGrokBuildThroughHTTP(t *testing.T) {
	const primary = "You are Gemini 2.5 Pro released by xAI. Keep tools."
	const protected = primary + " Grok Build."
	for _, profile := range []string{"legacy", "official"} {
		for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", profile, route, stream), func(t *testing.T) {
					isolatePolicies(t)
					resetStatsState(t)
					withRequestProfile(t, profile)
					withSystemPrompts(t, "", "")
					setIdentityConfig(runtimeIdentityConfig{})
					t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
					captured := make(chan map[string]any, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var reader io.Reader = r.Body
						if r.Header.Get("Content-Encoding") == "gzip" {
							compressed, err := gzip.NewReader(r.Body)
							if err != nil {
								t.Error(err)
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							defer compressed.Close()
							reader = compressed
						}
						var request map[string]any
						if err := json.NewDecoder(reader).Decode(&request); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						captured <- request
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, conversationFixtureSSE(false))
					}))
					defer upstream.Close()
					oldBase, oldClient := profileCN.Base, cfg.HttpClient
					profileCN.Base = upstream.URL
					cfg.HttpClient = &http.Client{Timeout: 5 * time.Second}
					t.Cleanup(func() { profileCN.Base, cfg.HttpClient = oldBase, oldClient })
					accountMu.Lock()
					accounts = []*Account{{Path: "synthetic-grok.json", Auth: &StoredAuth{
						Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix()},
					}}}
					accountMu.Unlock()
					system := []any{
						map[string]any{"type": "text", "text": "Keep rules."},
						map[string]any{"type": "text", "text": primary},
					}
					conversation := []any{
						msg("user", "Start task."),
						map[string]any{"role": "assistant", "content": []any{
							map[string]any{"type": "text", "text": "Keep rules."},
							map[string]any{"type": "text", "text": primary},
						}},
						msg("user", protected),
					}
					body := map[string]any{"model": "gemini-test", "stream": stream, "max_tokens": 64}
					switch route {
					case "/v1/responses":
						body["input"] = append([]any{map[string]any{"role": "developer", "content": system}}, conversation...)
						body["store"] = false
					case "/v1/messages":
						assistant := conversation[1].(map[string]any)
						assistant["content"] = append(assistant["content"].([]any), map[string]any{
							"type": "tool_use", "id": "call_grok", "name": "echo", "input": map[string]any{"text": protected},
						})
						conversation[2] = map[string]any{"role": "user", "content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_grok", "content": []any{map[string]any{"type": "text", "text": protected}}},
							map[string]any{"type": "text", "text": protected},
						}}
						body["system"], body["messages"] = system, conversation
					default:
						body["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, conversation...)
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					gateway := httptest.NewServer(newAPIHandler())
					defer gateway.Close()
					client := &http.Client{Timeout: 10 * time.Second}
					response, err := client.Post(gateway.URL+route, "application/json", bytes.NewReader(raw))
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					responseBody, err := io.ReadAll(response.Body)
					if err != nil || response.StatusCode != http.StatusOK {
						t.Fatalf("gateway HTTP %d: %s error=%v", response.StatusCode, responseBody, err)
					}
					select {
					case request := <-captured:
						if request["model"] != body["model"] {
							t.Fatalf("model changed: %v", request["model"])
						}
						messages := request["messages"].([]any)
						for _, index := range []int{0, 2} {
							text := dshTestText(messages[index].(map[string]any)["content"])
							if strings.ReplaceAll(text, "\n", "") != "Keep rules.You are WorkBuddy. Keep tools." {
								t.Fatalf("identity message %d=%q", index, text)
							}
						}
						if got := dshTestText(messages[len(messages)-1].(map[string]any)["content"]); got != protected {
							t.Fatalf("user text changed: %q", got)
						}
						if route == "/v1/messages" {
							tool := messages[3].(map[string]any)
							if tool["role"] != "tool" || tool["tool_call_id"] != "call_grok" || tool["content"] != protected {
								t.Fatalf("tool result changed: %v", tool)
							}
							call := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
							var args map[string]any
							if err := json.Unmarshal([]byte(call["arguments"].(string)), &args); err != nil || !reflect.DeepEqual(args, map[string]any{"text": protected}) {
								t.Fatalf("tool arguments changed: %v error=%v", args, err)
							}
						}
					case <-time.After(2 * time.Second):
						t.Fatal("no upstream HTTP request")
					}
				})
			}
		}
	}
}
