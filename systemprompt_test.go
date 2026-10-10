package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withSystemPrompts 设置 systemPrompt 配置（fallback / force），测试结束自动还原。
func withSystemPrompts(t *testing.T, fallback, force string) {
	t.Helper()
	old := configuredSystemPrompts()
	setSystemPromptConfig(fallback, force)
	t.Cleanup(func() { setSystemPromptConfig(old.fallback, old.force) })
}

func TestSystemPromptConfigurationAndMissingConfig(t *testing.T) {
	old := configuredSystemPrompts()
	oldDebug := cfg.DebugEnabled
	oldLogBody := cfg.DebugLogSystemPrompt
	t.Cleanup(func() {
		setSystemPromptConfig(old.fallback, old.force)
		cfg.DebugEnabled = oldDebug
		cfg.DebugLogSystemPrompt = oldLogBody
	})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"systemPrompt":{"fallback":"新保底","force":"全局实验规则"},"debug":{"enabled":true,"logSystemPrompt":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := configuredSystemPrompts(); got.fallback != "新保底" || got.force != "全局实验规则" {
		t.Fatalf("配置没有生效: %#v", got)
	}
	if !cfg.DebugEnabled || !cfg.DebugLogSystemPrompt {
		t.Fatalf("debug.logSystemPrompt 未随配置生效: enabled=%t logSystemPrompt=%t", cfg.DebugEnabled, cfg.DebugLogSystemPrompt)
	}
	// 语义完全由 fallback 决定：非空即覆盖，无需任何模式字段。
	if err := os.WriteFile(path, []byte(`{"systemPrompt":{"fallback":"","force":"全局实验规则"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := configuredSystemPrompts(); got.fallback != "" || got.force != "全局实验规则" {
		t.Fatalf("空 fallback 配置没有生效: %#v", got)
	}
	// systemPrompt 段的未知字段仍然在启动阶段直接报错，不静默忽略。
	if err := os.WriteFile(path, []byte(`{"systemPrompt":{"fallback":"x","overWrite":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err == nil {
		t.Fatal("systemPrompt 段的未知字段必须加载失败")
	}
	if err := loadRuntimeConfig(path + ".missing"); err != nil {
		t.Fatal(err)
	}
	if got := configuredSystemPrompts(); got.fallback != "" || got.force != "" {
		t.Fatalf("配置文件缺失时应重置为空: %#v", got)
	}
	if cfg.DebugLogSystemPrompt {
		t.Fatal("配置文件缺失时 debug.logSystemPrompt 必须重置为关闭")
	}
}

// TestFallbackNonEmptyCoversClientSystem 固定核心语义：fallback 非空即覆盖客户端 system。
func TestFallbackNonEmptyCoversClientSystem(t *testing.T) {
	withSystemPrompts(t, "自定义保底", "")
	obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "客户端规则"}, map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 2, "test-trace")
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != "自定义保底" {
		t.Fatalf("fallback 非空时必须覆盖客户端 system: %#v", got)
	}
}

// TestEmptyFallbackPassesThroughClientSystem 固定另一半语义：fallback 为空即原样透传。
func TestEmptyFallbackPassesThroughClientSystem(t *testing.T) {
	withSystemPrompts(t, "   ", "")
	obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "客户端规则"}, map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 2, "test-trace")
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != "客户端规则" {
		t.Fatalf("fallback 为空时必须透传客户端 system: %#v", got)
	}
	// 客户端没给 system 时用内置兜底。
	userOnly := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(userOnly, httptest.NewRequest("POST", "/v1/chat/completions", nil), 3, "test-trace")
	if got := userOnly["messages"].([]any)[0].(map[string]any)["content"]; got != defaultSystemPrompt {
		t.Fatalf("fallback 为空且客户端无 system 时应使用内置兜底: %#v", got)
	}
}

func TestForcedGlobalPromptPreservesExistingSystem(t *testing.T) {
	withSystemPrompts(t, "", "全局实验规则")
	obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "客户端规则"}, map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 3, "test-trace")
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "客户端规则\n\n全局实验规则" {
		t.Fatalf("全局强制文本没有追加到客户端 system 末尾或篡改了原文: %#v", msgs)
	}
	obj = map[string]any{"messages": []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "原始内容块"}}}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/responses", nil), 4, "test-trace")
	parts := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "原始内容块" || parts[1].(map[string]any)["text"] != "全局实验规则" {
		t.Fatalf("全局强制文本应作为最后一个内容块追加且不丢弃原 system: %#v", parts)
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
	if got := chat[0].(map[string]any)["content"]; got != "定制保底\n\n统一规则" {
		t.Fatalf("Chat 缺 system 时应保留保底并把全局规则追加到末尾: %#v", got)
	}
	responses := captureUpstreamMessages(t, "/v1/responses", `{"model":"m","stream":true,"instructions":"客户端已有system","input":"hi"}`)
	if got := responses[0].(map[string]any)["content"]; got != "定制保底\n\n统一规则" {
		t.Fatalf("Responses 应覆盖 instructions 并把全局规则追加到末尾: %#v", got)
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
		{"fallback", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "定制保底\n\n统一规则"},
		{"client_system", `{"model":"m","system":"客户端规则","messages":[{"role":"user","content":"hi"}]}`, "定制保底\n\n统一规则"},
		{"system_blocks", `{"model":"m","system":[{"type":"text","text":"规则一"},{"type":"text","text":"规则二"}],"messages":[{"role":"user","content":"hi"}]}`, "定制保底\n\n统一规则"},
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

// TestForcedPromptIsAppendedAfterClientSystem 固定「客户端原文在前、强制提示词在后」这一契约。
//
// 这条顺序直接决定冲突时的实际效果：网关只发送一条 system 消息，内容形如
// 「<客户端自己的系统提示词>\n\n<强制文本>」。强制文本位于整段提示词的最末尾，
// 紧贴其后的用户消息，是最"新近"的指令，因此人设类、风格类指令通常最有影响力。
// 这不是优先级字段，而是同一段文本里的先后顺序。fallback 为空时才会出现这种"原文 + 强制"的形态。
func TestForcedPromptIsAppendedAfterClientSystem(t *testing.T) {
	withSystemPrompts(t, "", "【强制后置】")
	body := `{"model":"m","stream":true,"messages":[{"role":"system","content":"【客户端自己的系统提示词】"},{"role":"user","content":"hi"}]}`
	messages := captureUpstreamMessages(t, "/v1/chat/completions", body)
	content, ok := messages[0].(map[string]any)["content"].(string)
	if !ok {
		t.Fatalf("system 内容应为字符串: %#v", messages[0])
	}
	forcedAt := strings.Index(content, "【强制后置】")
	clientAt := strings.Index(content, "【客户端自己的系统提示词】")
	if clientAt != 0 {
		t.Fatalf("客户端原文应位于 system 内容最前: %q", content)
	}
	if forcedAt <= clientAt {
		t.Fatalf("强制文本应排在客户端原文之后: %q", content)
	}
	// 强制文本必须收尾，即位于 system 内容的最后，紧贴其后的用户消息。
	if !strings.HasSuffix(content, "【强制后置】") {
		t.Fatalf("强制文本应位于 system 内容最末: %q", content)
	}
	// 两者之间用空行分隔，保证仍是一条合法 system 消息。
	if !strings.Contains(content, "【客户端自己的系统提示词】\n\n【强制后置】") {
		t.Fatalf("拼接格式变化: %q", content)
	}
}
