package main

import (
	"reflect"
	"testing"
)

func TestRepairReasoningHistoryPreservesCopiesAndMarksMissing(t *testing.T) {
	obj := map[string]any{
		"model":            "DeepSeek-v4.1-flash",
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "a", "reasoning_content": "original"},
			map[string]any{"role": "assistant", "content": "b", "reasoning": "legacy"},
			map[string]any{"role": "assistant", "content": "c"},
			map[string]any{"role": "assistant", "content": "d", "reasoning_content": nil},
			map[string]any{"role": "tool", "content": "result"},
		},
	}
	report := repairReasoningHistory(obj)
	if !report.Applied || !report.ThinkingEnabled || !report.HasTrace || report.AssistantMessages != 4 ||
		report.PreservedContent != 1 || report.CopiedFromReasoning != 1 || report.EmptyContentAdded != 2 ||
		report.InvalidContentReplaced != 1 || report.ReasoningMirrored != 1 || report.ReasoningPlaceholders != 2 {
		t.Fatalf("unexpected repair report: %+v", report)
	}
	msgs := obj["messages"].([]any)
	want := [][2]string{{"original", "original"}, {"legacy", "legacy"}, {"", " "}, {"", " "}}
	for i, pair := range want {
		msg := msgs[i+1].(map[string]any)
		if msg["reasoning_content"] != pair[0] || msg["reasoning"] != pair[1] {
			t.Fatalf("assistant[%d] = %#v, want rc=%q reasoning=%q", i, msg, pair[0], pair[1])
		}
	}
	if _, exists := msgs[0].(map[string]any)["reasoning_content"]; exists {
		t.Fatal("user message unexpectedly modified")
	}
	if _, exists := msgs[len(msgs)-1].(map[string]any)["reasoning"]; exists {
		t.Fatal("tool result unexpectedly modified")
	}
	// 相同输入重复经过共享出站管线，不应改变任何已补过的字段。
	before := make([]map[string]any, 4)
	for i := range before {
		msg := msgs[i+1].(map[string]any)
		before[i] = map[string]any{"reasoning_content": msg["reasoning_content"], "reasoning": msg["reasoning"]}
	}
	repairReasoningHistory(obj)
	for i := range before {
		msg := msgs[i+1].(map[string]any)
		if msg["reasoning_content"] != before[i]["reasoning_content"] || msg["reasoning"] != before[i]["reasoning"] {
			t.Fatalf("repeat changed assistant[%d]: %#v", i, msg)
		}
	}
}

func TestRepairReasoningHistoryThinkingGate(t *testing.T) {
	cases := []struct {
		name  string
		body  map[string]any
		apply bool
	}{
		{"non-deepseek untouched", map[string]any{"model": "hy4-preview", "reasoning_effort": "high", "messages": []any{map[string]any{"role": "assistant", "content": "ok"}}}, false},
		{"deepseek without explicit thinking or trace untouched", map[string]any{"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "assistant", "content": "ok"}}}, false},
		{"disabled without trace untouched", map[string]any{"model": "deepseek-v4.1-flash", "thinking": map[string]any{"type": "disabled"}, "reasoning_effort": "high", "messages": []any{map[string]any{"role": "assistant", "content": "ok"}}}, false},
		{"disabled with trace still replays history", map[string]any{"model": "deepseek-v4.1-flash", "thinking": map[string]any{"type": "disabled"}, "messages": []any{map[string]any{"role": "assistant", "reasoning": "earlier", "content": "ok"}, map[string]any{"role": "assistant", "content": "more"}}}, true},
		{"nested reasoning effort enables replay", map[string]any{"model": "deepseek-v4.1-flash", "reasoning": map[string]any{"effort": "high"}, "messages": []any{map[string]any{"role": "assistant", "content": "ok"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := repairReasoningHistory(tc.body)
			if report.Applied != tc.apply {
				t.Fatalf("report=%+v want apply=%t", report, tc.apply)
			}
			msgs := tc.body["messages"].([]any)
			for _, raw := range msgs {
				msg := raw.(map[string]any)
				if !tc.apply {
					if _, exists := msg["reasoning_content"]; exists {
						t.Fatalf("skipped request was modified: %#v", msg)
					}
					continue
				}
				if _, ok := msg["reasoning_content"].(string); !ok {
					t.Fatalf("assistant missing string reasoning_content: %#v", msg)
				}
				if reasoning, ok := msg["reasoning"].(string); !ok || reasoning == "" {
					t.Fatalf("assistant missing nonempty reasoning marker: %#v", msg)
				}
			}
		})
	}
}

func TestChatEndpointRepairsDeepSeekReasoningHistory(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","stream":true,"reasoning_effort":"high","messages":[
		{"role":"system","content":"s"},
		{"role":"user","content":"run"},
		{"role":"assistant","content":"start","reasoning_content":"real trace","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_a","content":"ok"},
		{"role":"assistant","content":"next","reasoning":"legacy trace","tool_calls":[{"id":"call_b","type":"function","function":{"name":"grep","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_b","content":"ok"},
		{"role":"assistant","content":"done"}
	]}`
	messages := captureUpstreamMessages(t, "/v1/chat/completions", body)
	if len(messages) != 7 || !reflect.DeepEqual(rolesOf(map[string]any{"messages": messages}), []string{"system", "user", "assistant", "tool", "assistant", "tool", "assistant"}) {
		t.Fatalf("chat history topology changed: %#v", messages)
	}
	first := messages[2].(map[string]any)
	if first["reasoning_content"] != "real trace" || first["reasoning"] != "real trace" {
		t.Fatalf("original Chat reasoning was not preserved and mirrored: %#v", first)
	}
	second := messages[4].(map[string]any)
	if second["reasoning_content"] != "legacy trace" || second["reasoning"] != "legacy trace" {
		t.Fatalf("legacy Chat reasoning was not copied: %#v", second)
	}
	last := messages[6].(map[string]any)
	if last["reasoning_content"] != "" || last["reasoning"] != " " {
		t.Fatalf("missing Chat reasoning fields were not backfilled: %#v", last)
	}
}
