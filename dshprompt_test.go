package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testDSHPrefix = "You are an AI agent powered by DeepSeek Harness"
const testDSHGUI = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI."

func testDSHNeutralGUI() string {
	return strings.Replace(testDSHGUI, "the DeepSeek Harness Web GUI", "a Web GUI", 1)
}

func TestDSHToolArgumentsAndResultsThroughHTTP(t *testing.T) {
	private := testDSHPrefix + ". " + testDSHGUI
	args, err := json.Marshal(map[string]string{"text": private})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				isolatePolicies(t)
				resetStatsState(t)
				messages := []any{
					map[string]any{"role": "system", "content": testDSHGUI},
					map[string]any{"role": "user", "content": "Save text."},
					map[string]any{"role": "assistant", "content": testDSHPrefix, "tool_calls": []any{
						map[string]any{"id": "call-test", "type": "function", "function": map[string]any{"name": "save", "arguments": string(args)}},
					}},
					map[string]any{"role": "tool", "tool_call_id": "call-test", "content": private},
					map[string]any{"role": "user", "content": private},
				}
				body := map[string]any{"model": "deepseek-v4.1-flash", "stream": stream, "max_tokens": 64, "messages": messages}
				if path == "/v1/responses" {
					body["input"] = []any{
						messages[0], messages[1], map[string]any{"role": "assistant", "content": testDSHPrefix},
						map[string]any{"type": "function_call", "call_id": "call-test", "name": "save", "arguments": string(args)},
						map[string]any{"type": "function_call_output", "call_id": "call-test", "output": private}, messages[4],
					}
					body["store"] = false
					delete(body, "messages")
				} else if path == "/v1/messages" {
					body["system"] = testDSHGUI
					body["messages"] = []any{
						messages[1],
						map[string]any{"role": "assistant", "content": []any{
							map[string]any{"type": "text", "text": testDSHPrefix},
							map[string]any{"type": "tool_use", "id": "call-test", "name": "save", "input": map[string]string{"text": private}},
						}},
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-test", "content": private}}},
						messages[4],
					}
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				got := captureUpstreamMessages(t, path, string(raw))
				foundCall, foundResult := false, false
				for _, item := range got {
					message := item.(map[string]any)
					if message["role"] == "tool" {
						foundResult = message["tool_call_id"] == "call-test" && message["content"] == private
					}
					calls, _ := message["tool_calls"].([]any)
					for _, rawCall := range calls {
						call := rawCall.(map[string]any)
						function := call["function"].(map[string]any)
						foundCall = call["id"] == "call-test" && function["name"] == "save" && function["arguments"] == string(args)
					}
				}
				if !foundCall || !foundResult || got[len(got)-1].(map[string]any)["content"] != private {
					t.Fatal("tool arguments, result linkage, or user text changed")
				}
			})
		}
	}
}

func TestDSHUpstreamRejectionIsNotRetried(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				isolatePolicies(t)
				resetStatsState(t)
				accountMu.Lock()
				accounts = []*Account{{Path: "synthetic.json", Edition: "cn", Auth: &StoredAuth{
					Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()},
				}}}
				accountMu.Unlock()
				oldClient := cfg.HttpClient
				t.Cleanup(func() { cfg.HttpClient = oldClient })
				calls := 0
				cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 400, Header: make(http.Header), Request: req,
						Body: io.NopCloser(strings.NewReader(`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`))}, nil
				})}
				messages := []any{map[string]any{"role": "system", "content": testDSHGUI}, map[string]any{"role": "user", "content": "hi"}}
				body := map[string]any{"model": "deepseek-v4.1-flash", "stream": stream, "messages": messages}
				if path == "/v1/responses" {
					body["input"], body["store"] = messages, false
					delete(body, "messages")
				} else if path == "/v1/messages" {
					body["system"], body["messages"] = testDSHGUI, messages[1:]
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
				newAPIHandler().ServeHTTP(w, r)
				if w.Code != 400 || !strings.Contains(w.Body.String(), "11128") || calls != 1 {
					t.Fatalf("rejection was hidden or retried: HTTP %d calls=%d", w.Code, calls)
				}
			})
		}
	}
}

func dshTestText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	var texts []string
	parts, _ := content.([]any)
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if part["type"] == "text" {
			text, _ := part["text"].(string)
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func TestConfiguredDSHPromptsThroughHTTP(t *testing.T) {
	identity := testDSHPrefix + ". config-identity-sentinel"
	neutralIdentity := blockedSystemPromptPrefixReplacement + ". config-identity-sentinel"
	gui := testDSHGUI + " Keep config-tail-sentinel."
	neutralGUI := testDSHNeutralGUI() + " Keep config-tail-sentinel."
	user := identity + "\n\n" + gui + " private-user-sentinel"
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, fallback, force string
				clientSystem          any
				want                  string
			}{
				{"fallback", identity + "\n\n" + gui, "", nil, neutralIdentity + "\n\n" + neutralGUI},
				{"force_string", "", gui, "Client instruction.", "Client instruction.\n\n" + neutralGUI},
				{"force_block", "", identity + "\n\n" + gui, []any{map[string]any{"type": "text", "text": "Client instruction."}}, "Client instruction.\n" + neutralIdentity + "\n\n" + neutralGUI},
				{"fallback_and_force", identity, gui, nil, neutralIdentity + "\n\n" + neutralGUI},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, tc.name), func(t *testing.T) {
					isolatePolicies(t)
					resetStatsState(t)
					withSystemPrompts(t, tc.fallback, tc.force)
					messages := []any{map[string]any{"role": "user", "content": user}}
					if tc.clientSystem != nil {
						messages = append([]any{map[string]any{"role": "system", "content": tc.clientSystem}}, messages...)
					}
					body := map[string]any{"model": "deepseek-v4.1-flash", "stream": stream, "max_tokens": 64, "messages": messages}
					want := tc.want
					if path == "/v1/responses" {
						body["input"], body["store"] = messages, false
						delete(body, "messages")
					} else if path == "/v1/messages" {
						body["messages"] = []any{map[string]any{"role": "user", "content": user}}
						if tc.clientSystem != nil {
							body["system"] = tc.clientSystem
						}
						if tc.name == "force_block" {
							// Messages joins system blocks before appending force; a
							// mid-string identity stays outside the prefix rule.
							want = "Client instruction.\n\n" + identity + "\n\n" + neutralGUI
						}
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					got := captureUpstreamMessages(t, path, string(raw))
					if len(got) != 2 || dshTestText(got[0].(map[string]any)["content"]) != want || got[1].(map[string]any)["content"] != user {
						t.Fatal("configured prompt was not normalized or user text changed")
					}
				})
			}
		}
	}
}

func TestDSHHistoryThroughHTTP(t *testing.T) {
	identity := testDSHPrefix + ". Preserve history-tail-sentinel."
	neutralIdentity := blockedSystemPromptPrefixReplacement + ". Preserve history-tail-sentinel."
	gui := "Previous context: " + testDSHGUI + " Keep custom GUI instructions."
	neutralGUI := "Previous context: " + testDSHNeutralGUI() + " Keep custom GUI instructions."
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, blocks := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/blocks=%t", path, stream, blocks), func(t *testing.T) {
					isolatePolicies(t)
					resetStatsState(t)
					var system, assistant any = "Answer briefly.", identity
					wantSystem, wantAssistant := "Answer briefly.", neutralIdentity
					if blocks {
						system = []any{map[string]any{"type": "text", "text": "Answer briefly."}, map[string]any{"type": "text", "text": identity}}
						assistant = []any{map[string]any{"type": "text", "text": "Earlier part."}, map[string]any{"type": "text", "text": identity}}
						wantSystem += "\n" + neutralIdentity
						wantAssistant = "Earlier part.\n" + neutralIdentity
					}
					messages := []any{
						map[string]any{"role": "system", "content": system},
						map[string]any{"role": "user", "content": "Earlier question."},
						map[string]any{"role": "assistant", "content": assistant},
						map[string]any{"role": "user", "content": "Continue."},
						map[string]any{"role": "assistant", "content": gui},
						map[string]any{"role": "user", "content": identity + "\n" + gui},
					}
					body := map[string]any{"model": "deepseek-v4.1-flash", "stream": stream, "max_tokens": 64, "messages": messages}
					if path == "/v1/responses" {
						body["input"], body["store"] = messages, false
						delete(body, "messages")
					} else if path == "/v1/messages" {
						body["system"], body["messages"] = system, messages[1:]
						if blocks {
							wantAssistant = "Earlier part." + neutralIdentity
						}
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					got := captureUpstreamMessages(t, path, string(raw))
					want := []string{wantSystem, "Earlier question.", wantAssistant, "Continue.", neutralGUI, identity + "\n" + gui}
					roles := []string{"system", "user", "assistant", "user", "assistant", "user"}
					if len(got) != len(want) {
						t.Fatal("history message count changed")
					}
					for i, rawMessage := range got {
						message := rawMessage.(map[string]any)
						if message["role"] != roles[i] || dshTestText(message["content"]) != want[i] {
							t.Fatalf("history message %d role or text mismatch", i)
						}
					}
					// Text and GUI compatibility must remain idempotent.
					before, _ := json.Marshal(got)
					// A second HTTP request replays the already-normalized history.
					if path != "/v1/messages" {
						body["messages"] = got
						if path == "/v1/responses" {
							body["input"] = got
							delete(body, "messages")
						}
						second, _ := json.Marshal(body)
						again := captureUpstreamMessages(t, path, string(second))
						after, _ := json.Marshal(again)
						if !reflect.DeepEqual(before, after) {
							t.Fatal("normalized history changed on replay")
						}
					}
				})
			}
		}
	}
}
