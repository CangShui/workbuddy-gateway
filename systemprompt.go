package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

// promptSettings 是配置文件内的可选提示词。零值完全保留旧行为。
// 日志仅记录来源和长度，绝不记录配置或客户端的提示词正文。
type promptSettings struct {
	fallback string
	force    string
}

var (
	promptSettingsMu sync.RWMutex
	currentPrompts   promptSettings
)

func setSystemPromptConfig(fallback, force string) {
	promptSettingsMu.Lock()
	currentPrompts = promptSettings{fallback: fallback, force: force}
	promptSettingsMu.Unlock()
}

func configuredSystemPrompts() promptSettings {
	promptSettingsMu.RLock()
	settings := currentPrompts
	promptSettingsMu.RUnlock()
	return settings
}

func configuredFallbackSystemPrompt() string {
	fallback := configuredSystemPrompts().fallback
	if strings.TrimSpace(fallback) == "" {
		return defaultSystemPrompt
	}
	return fallback
}

// prepareSystemPromptForUpstream 在 Chat、Responses、Messages 三个入口统一执行。
// 先保证首条为 system，再把实验性的全局强制文本追加到该 system 内容末尾（后置），
// 使其成为整段提示词中位置最靠后的指令、紧贴其后的用户消息；
// 不删除、不覆盖客户端原有 system。content 数组保留原来的内容块顺序，
// 强制文本作为最后一个内容块追加在数组末尾。
func prepareSystemPromptForUpstream(obj map[string]any, r *http.Request, requestID uint64, traceID string) {
	injected := ensureLeadingSystemMessage(obj)
	settings := configuredSystemPrompts()
	force := settings.force
	forced := strings.TrimSpace(force) != ""
	kind := "未修改"
	messages, _ := obj["messages"].([]any)
	if forced && len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok {
			old := first["content"]
			switch content := old.(type) {
			case string:
				kind = "字符串"
				if content == "" {
					first["content"] = force
				} else {
					first["content"] = content + "\n\n" + force
				}
			case []any:
				kind = "内容块数组"
				blocks := make([]any, 0, len(content)+1)
				blocks = append(blocks, content...)
				blocks = append(blocks, map[string]any{"type": "text", "text": force})
				first["content"] = blocks
			case nil:
				kind = "空内容"
				first["content"] = force
			default:
				// 非法 system 内容仍由上游参数校验；不静默丢弃客户端数据。
				forced = false
				kind = "未支持的内容类型，已跳过强制后置"
			}
		}
	}
	fallbackSource := "未使用（客户端提供system）"
	if injected {
		fallbackSource = "内置保底"
		if strings.TrimSpace(settings.fallback) != "" {
			fallbackSource = "配置保底"
		}
	}
	if traceID == "" {
		traceID = debugTraceID(r)
	}
	if traceID == "" {
		traceID = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[系统提示词规则] traceId=%s requestId=%d 阶段=上游请求序列化前 保底来源=%s 强制全局已应用=%t 强制位置=后置(紧贴用户消息) 强制内容类型=%s 强制字符数=%d 结果=保留客户端原有system且未记录提示词正文",
		traceID, requestID, fallbackSource, forced, kind, utf8.RuneCountInString(force))
	debugEvent(r, "debug", "system_prompt_policy_applied", map[string]any{
		"fallback_source": fallbackSource, "forced_applied": forced,
		"forced_position": "append_after_client_system", "forced_content_kind": kind,
		"forced_chars":    utf8.RuneCountInString(force),
		"business_impact": "按配置在请求出站前处理system；强制文本后置于system末尾且不记录提示词正文",
	})
}
