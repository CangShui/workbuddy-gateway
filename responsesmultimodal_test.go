package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestResponsesToolOutputKeepsTextAndPromotesImages(t *testing.T) {
	url := "data:image/png;base64," + strings.Repeat("A", 4096)
	output := []any{
		map[string]any{"type": "input_text", "text": "完整工具说明"},
		map[string]any{"type": "input_image", "image_url": url, "detail": "high"},
		map[string]any{"type": "input_text", "text": "图片之后的说明"},
	}
	before, _ := json.Marshal(output)
	text, images := convertResponsesToolOutput(output)
	if text != "完整工具说明\n图片之后的说明" || len(images) != 1 {
		t.Fatalf("text/vision conversion wrong: text=%q images=%d", text, len(images))
	}
	image := images[0].(map[string]any)["image_url"].(map[string]any)
	if image["url"] != url || image["detail"] != "high" {
		t.Fatal("image URL or detail changed")
	}
	if strings.Contains(text, "base64") || len(text) > 100 {
		t.Fatal("image payload leaked into text")
	}
	after, _ := json.Marshal(output)
	if string(before) != string(after) {
		t.Fatal("conversion mutated the original input")
	}
}

func TestResponsesToolOutputPreservesLegacyAndUnknownData(t *testing.T) {
	for _, raw := range []any{nil, "", "plain", map[string]any{"status": "ok"}, []any{"one", "two"}, []any{map[string]any{"answer": 42}}} {
		text, images := convertResponsesToolOutput(raw)
		if text != stringifyToolOutput(raw) || len(images) != 0 {
			t.Fatalf("legacy output changed: %#v", raw)
		}
	}
	output := []any{
		map[string]any{"type": "text", "text": "原文"},
		map[string]any{"type": "unknown", "value": 42},
		map[string]any{"type": "input_image", "image_url": ""},
	}
	text, images := convertResponsesToolOutput(output)
	if !strings.Contains(text, "原文") || !strings.Contains(text, `"value":42`) ||
		!strings.Contains(text, `"image_url":""`) || len(images) != 0 {
		t.Fatal("unknown or malformed blocks were silently dropped")
	}
}

func TestResponsesImageOnlyToolResultHasNonemptyText(t *testing.T) {
	for _, image := range []any{
		map[string]any{"type": "input_image", "image_url": "https://example.invalid/screen.png"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/screen.png", "detail": "low"}},
	} {
		messages := convertResponsesInputItem(map[string]any{
			"type": "function_call_output", "call_id": "call_a", "output": []any{image},
		})
		if len(messages) != 2 || messages[0].(map[string]any)["content"] != "[图片]" ||
			messages[0].(map[string]any)["tool_call_id"] != "call_a" ||
			messages[1].(map[string]any)["role"] != "user" {
			t.Fatalf("image-only output conversion wrong: %#v", messages)
		}
	}
}

func TestResponsesEndpointKeepsParallelImageResultsPaired(t *testing.T) {
	withSystemPrompts(t, "", "")
	body := `{"model":"deepseek-v4.1-flash","stream":true,"instructions":"完整操作手册",
	"input":[
		{"role":"user","content":"请比较截图"},
		{"type":"function_call","call_id":"call_a","name":"capture","arguments":"{}"},
		{"type":"function_call","call_id":"call_b","name":"capture","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_a","output":[
			{"type":"input_text","text":"第一张截图说明"},
			{"type":"input_image","image_url":"data:image/png;base64,AAA","detail":"high"}]},
		{"type":"function_call_output","call_id":"call_b","output":[
			{"type":"input_image","image_url":"data:image/png;base64,BBB"}]},
		{"role":"user","content":"保留后续用户指令"}]}`
	messages := captureUpstreamMessages(t, "/v1/responses", body)
	wantRoles := []string{"system", "user", "assistant", "tool", "tool", "user", "user", "user"}
	if !reflect.DeepEqual(rolesOf(map[string]any{"messages": messages}), wantRoles) {
		t.Fatalf("parallel results were interrupted by images: %#v", rolesOf(map[string]any{"messages": messages}))
	}
	if messages[0].(map[string]any)["content"] != "完整操作手册" ||
		messages[3].(map[string]any)["content"] != "第一张截图说明" ||
		messages[4].(map[string]any)["content"] != "[图片]" ||
		messages[7].(map[string]any)["content"] != "保留后续用户指令" {
		t.Fatal("manual, tool text, or later user content changed")
	}
	for i, wantURL := range []string{"data:image/png;base64,AAA", "data:image/png;base64,BBB"} {
		parts := messages[5+i].(map[string]any)["content"].([]any)
		if parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != wantURL {
			t.Fatal("image data changed or image order reversed")
		}
	}
}

func TestResponsesEndpointSingleToolImageKeepsOrder(t *testing.T) {
	withSystemPrompts(t, "", "")
	body := `{"model":"m","stream":true,"input":[
		{"role":"user","content":"检查图片"},
		{"type":"function_call","call_id":"call_a","name":"capture","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_a","output":[
			{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}]}`
	messages := captureUpstreamMessages(t, "/v1/responses", body)
	if !reflect.DeepEqual(rolesOf(map[string]any{"messages": messages}), []string{"system", "user", "assistant", "tool", "user"}) {
		t.Fatal("single tool output/image order wrong")
	}
}
