package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDSHGUIContextBoundaries(t *testing.T) {
	neutral := testDSHNeutralGUI()
	for _, tc := range []struct {
		name, input, want string
		changes           int
	}{
		{"opening", testDSHGUI, neutral, 1},
		{"custom_tail", "Before. " + testDSHGUI + " Keep after.", "Before. " + neutral + " Keep after.", 1},
		{"unknown_opening", strings.Replace(testDSHGUI, "they mean", "they refer to", 1), strings.Replace(testDSHGUI, "they mean", "they refer to", 1), 0},
		{"single_sentence", dshGUIContextSentence, dshGUIContextSentence, 0},
		{"multiple", testDSHGUI + "\n" + testDSHGUI, neutral + "\n" + neutral, 2},
		{"crlf", "Before.\r\n" + testDSHGUI + "\r\nAfter.", "Before.\r\n" + neutral + "\r\nAfter.", 1},
		{"inline", "``" + testDSHGUI + "``", "``" + testDSHGUI + "``", 0},
		{"indented", "    " + testDSHGUI, "    " + testDSHGUI, 0},
		{"tabbed", "\t" + testDSHGUI, "\t" + testDSHGUI, 0},
		{"fenced", "```text\n" + testDSHGUI + "\n```", "```text\n" + testDSHGUI + "\n```", 0},
		{"tilde_fence", "~~~text\n" + testDSHGUI + "\n~~~", "~~~text\n" + testDSHGUI + "\n~~~", 0},
		{"after_fence", "```text\nexample\n```\n" + testDSHGUI, "```text\nexample\n```\n" + neutral, 1},
		{"single_tick_multiline", "Example `first line\n" + testDSHGUI + "\nlast line`.", "Example `first line\n" + testDSHGUI + "\nlast line`.", 0},
		{"double_tick_with_single_tick", "Example ``first line\n` " + testDSHGUI + "\nlast line``.", "Example ``first line\n` " + testDSHGUI + "\nlast line``.", 0},
		{"multiline_crlf", "Example `first\r\n" + testDSHGUI + "\r\nlast`.", "Example `first\r\n" + testDSHGUI + "\r\nlast`.", 0},
		{"normalize_after_close", "Example `first\n" + testDSHGUI + "\nlast` " + testDSHGUI, "Example `first\n" + testDSHGUI + "\nlast` " + neutral, 1},
		{"fence_like_line_inside_span", "Example ````first\n```\n" + testDSHGUI + "\nlast```` " + testDSHGUI, "Example ````first\n```\n" + testDSHGUI + "\nlast```` " + neutral, 1},
		{"close_on_indented_line", "Example `first\n" + testDSHGUI + "\n    last`\n" + testDSHGUI, "Example `first\n" + testDSHGUI + "\n    last`\n" + neutral, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := normalizeDSHGUIContextText(tc.input)
			if got != tc.want || n != tc.changes {
				t.Fatalf("incorrect GUI/code boundary: changes=%d want=%d", n, tc.changes)
			}
			if again, n := normalizeDSHGUIContextText(got); again != got || n != 0 {
				t.Fatal("normalization is not idempotent")
			}
		})
	}
}

func TestDSHPromptScopeAndMetadataOnlyLogs(t *testing.T) {
	path, records := fullRequestTestLog(t)
	prefix := testDSHPrefix + ". private-prompt-sentinel"
	gui := testDSHGUI + " private-gui-sentinel"
	newPrefix := blockedSystemPromptPrefixReplacement + ". private-prompt-sentinel"
	newGUI := testDSHNeutralGUI() + " private-gui-sentinel"
	makeMessage := func(role, identity, context string) map[string]any {
		return map[string]any{
			"role": role, "content": []any{
				map[string]any{"type": "image_url", "text": prefix, "image_url": map[string]any{"url": "private-image-sentinel"}},
				map[string]any{"type": "text", "text": "Ordinary instructions."},
				map[string]any{"type": "text", "text": identity},
				map[string]any{"type": "text", "text": context},
				nil,
			},
			"reasoning_content": prefix + gui,
			"tool_calls":        []any{map[string]any{"id": "call-test", "type": "function", "function": map[string]any{"name": "save", "arguments": prefix + gui}}},
		}
	}
	var messages, want []any
	for _, role := range []string{"system", "assistant", "user", "tool"} {
		messages = append(messages, makeMessage(role, prefix, gui))
		if role == "system" || role == "assistant" {
			want = append(want, makeMessage(role, newPrefix, newGUI))
		} else {
			want = append(want, makeMessage(role, prefix, gui))
		}
	}
	obj := map[string]any{"messages": messages}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(r, "dsh-compat-test", time.Now())
	sanitizeMessages(obj)
	logDSHPromptCompatibility(r, 1, obj)
	if !reflect.DeepEqual(messages, want) {
		t.Fatal("wrong role/block scope, lost instructions, or modified reasoning/tool arguments")
	}
	before, _ := json.Marshal(obj)
	logDSHPromptCompatibility(r, 1, obj)
	after, _ := json.Marshal(obj)
	if string(before) != string(after) {
		t.Fatal("second compatibility pass changed content")
	}
	all := records()
	if len(all) != 1 || all[0]["event"] != "dsh_prompt_compatibility" {
		t.Fatal("missing metadata event or repeated normalization")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{prefix, gui, "private-prompt-sentinel", "private-gui-sentinel", "private-image-sentinel"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("compatibility diagnostics leaked private content")
		}
	}
}
