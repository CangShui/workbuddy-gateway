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

func TestDSHPromptScopeAndMetadataOnlyLogs(t *testing.T) {
	path, records := fullRequestTestLog(t)
	prefix := testDSHPrefix + ". private-prompt-sentinel"
	gui := testDSHGUI + " private-gui-sentinel"
	newPrefix := testIdentityTargetPrefix + ". private-prompt-sentinel"
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
	applyIdentityReplacements(obj, r, 1)
	if !reflect.DeepEqual(messages, want) {
		t.Fatal("wrong role/block scope, lost instructions, or modified reasoning/tool arguments")
	}
	before, _ := json.Marshal(obj)
	applyIdentityReplacements(obj, r, 1)
	after, _ := json.Marshal(obj)
	if string(before) != string(after) {
		t.Fatal("second compatibility pass changed content")
	}
	all := records()
	if len(all) != 2 || all[0]["event"] != "identity_replacements_applied" ||
		all[1]["event"] != "identity_replacements_skipped" {
		t.Fatal("missing applied/unchanged metadata events")
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
