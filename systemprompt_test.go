package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withSystemPrompts(t *testing.T, fallback, force string) {
	t.Helper()
	old := configuredSystemPrompts()
	setSystemPromptConfig(fallback, force)
	t.Cleanup(func() { setSystemPromptConfig(old.fallback, old.force) })
}

func TestSystemPromptConfigurationAndMissingConfig(t *testing.T) {
	old := configuredSystemPrompts()
	oldDebug := cfg.DebugEnabled
	t.Cleanup(func() {
		setSystemPromptConfig(old.fallback, old.force)
		cfg.DebugEnabled = oldDebug
	})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"systemPrompt":{"fallback":"新保底","force":"全局实验规则"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := configuredSystemPrompts(); got.fallback != "新保底" || got.force != "全局实验规则" {
		t.Fatalf("配置没有生效: %#v", got)
	}
	if err := loadRuntimeConfig(path + ".missing"); err != nil {
		t.Fatal(err)
	}
	if got := configuredSystemPrompts(); got.fallback != "" || got.force != "" {
		t.Fatalf("配置文件缺失时应重置为空: %#v", got)
	}
}

func TestFallbackPromptOnlyChangesMissingSystem(t *testing.T) {
	withSystemPrompts(t, "自定义保底", "")
	userOnly := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(userOnly, httptest.NewRequest("POST", "/v1/chat/completions", nil), 1, "test-trace")
	msgs := userOnly["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "自定义保底" {
		t.Fatalf("没有用配置的保底提示词: %#v", msgs)
	}
	original := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "客户端规则"}, map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(original, httptest.NewRequest("POST", "/v1/chat/completions", nil), 2, "test-trace")
	if got := original["messages"].([]any)[0].(map[string]any)["content"]; got != "客户端规则" {
		t.Fatalf("已有 system 不应被配置保底覆盖: %#v", got)
	}
}

func TestForcedGlobalPromptPreservesExistingSystem(t *testing.T) {
	withSystemPrompts(t, "", "全局实验规则")
	obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "客户端规则"}, map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 3, "test-trace")
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "全局实验规则\n\n客户端规则" {
		t.Fatalf("全局前缀没有置于客户端 system 前或篡改了原文: %#v", msgs)
	}
	obj = map[string]any{"messages": []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "原始内容块"}}}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/responses", nil), 4, "test-trace")
	parts := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "全局实验规则" || parts[1].(map[string]any)["text"] != "原始内容块" {
		t.Fatalf("全局前缀不应丢弃数组中的原 system: %#v", parts)
	}
}

func TestEmptyPromptConfigPreservesLegacyBehavior(t *testing.T) {
	withSystemPrompts(t, "  ", "\n\t")
	obj := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 5, "test-trace")
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != defaultSystemPrompt {
		t.Fatalf("空配置必须使用原保底文本: %#v", got)
	}
}

func TestSystemPromptConfigurationReachesChatAndResponses(t *testing.T) {
	withSystemPrompts(t, "定制保底", "统一规则")
	chat := captureUpstreamMessages(t, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if got := chat[0].(map[string]any)["content"]; got != "统一规则\n\n定制保底" {
		t.Fatalf("Chat 缺 system 时应保留保底并加入全局规则: %#v", got)
	}
	responses := captureUpstreamMessages(t, "/v1/responses", `{"model":"m","stream":true,"instructions":"客户端已有system","input":"hi"}`)
	if got := responses[0].(map[string]any)["content"]; got != "统一规则\n\n客户端已有system" {
		t.Fatalf("Responses 应保留 instructions 并加入全局规则: %#v", got)
	}
	if strings.Contains(chat[1].(map[string]any)["content"].(string), "统一规则") {
		t.Fatal("全局规则不能重复写入用户消息")
	}
}

func TestSystemPromptConfigurationReachesMessages(t *testing.T) {
	withSystemPrompts(t, "定制保底", "统一规则")
	for _, tc := range []struct {
		name, body, want string
	}{
		{"fallback", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "统一规则\n\n定制保底"},
		{"client_system", `{"model":"m","system":"客户端规则","messages":[{"role":"user","content":"hi"}]}`, "统一规则\n\n客户端规则"},
		{"system_blocks", `{"model":"m","system":[{"type":"text","text":"规则一"},{"type":"text","text":"规则二"}],"messages":[{"role":"user","content":"hi"}]}`, "统一规则\n\n规则一\n规则二"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := captureUpstreamMessages(t, "/v1/messages", tc.body)
			if got := messages[0].(map[string]any)["content"]; got != tc.want {
				t.Fatalf("Messages 提示词未统一应用: got=%#v want=%q", got, tc.want)
			}
			if got := messages[1].(map[string]any)["content"]; got != "hi" {
				t.Fatalf("不应修改 user 内容: %#v", got)
			}
		})
	}
}

// TestForcedPromptIsPrependedBeforeClientSystem 固定「强制提示词在前、客户端原文在后」这一契约。
//
// 这条顺序直接决定冲突时的实际效果：网关只发送一条 system 消息，内容形如
// 「<强制文本>\n\n<客户端自己的系统提示词>」。客户端自己的身份类指令排在后面，
// 且通常长得多、具体得多，因此当两者冲突时，客户端的指令往往更有影响力。
// 这不是优先级字段，而是同一段文本里的先后与详略差异。
func TestForcedPromptIsPrependedBeforeClientSystem(t *testing.T) {
	withSystemPrompts(t, "", "【强制前缀】")
	body := `{"model":"m","stream":true,"messages":[{"role":"system","content":"【客户端自己的系统提示词】"},{"role":"user","content":"hi"}]}`
	messages := captureUpstreamMessages(t, "/v1/chat/completions", body)
	content, ok := messages[0].(map[string]any)["content"].(string)
	if !ok {
		t.Fatalf("system 内容应为字符串: %#v", messages[0])
	}
	forcedAt := strings.Index(content, "【强制前缀】")
	clientAt := strings.Index(content, "【客户端自己的系统提示词】")
	if forcedAt != 0 {
		t.Fatalf("强制文本应位于 system 内容最前: %q", content)
	}
	if clientAt <= forcedAt {
		t.Fatalf("客户端原文应排在强制文本之后: %q", content)
	}
	// 两者之间用空行分隔，保证仍是一条合法 system 消息。
	if !strings.Contains(content, "【强制前缀】\n\n【客户端自己的系统提示词】") {
		t.Fatalf("拼接格式变化: %q", content)
	}
}
