package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

// promptSettings 是配置文件内的可选提示词。零值完全保留旧行为（透传客户端 system + 内置兜底）。
//
// 生效语义只由 fallback 是否为空决定，没有额外的模式开关：
//   - fallback 非空：客户端自带 system 时用 fallback 覆盖；客户端没给时以 fallback 保底。
//   - fallback 为空（或全空白）：客户端自带 system 原样透传；客户端没给时用内置兜底。
//
// force 与上述语义正交：非空时始终追加到首条 system 末尾（后置）。
// 日志只记录来源、生效语义、位置、形态与长度；提示词正文仅在 debug.enabled 与
// debug.logSystemPrompt 同时开启时才写入 JSON 调试日志，普通运行日志永不记录正文。
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

// systemPromptFallbackActive 报告是否启用「配置提示词」：fallback 非空白即启用（覆盖 + 保底）。
func systemPromptFallbackActive(settings promptSettings) bool {
	return strings.TrimSpace(settings.fallback) != ""
}

// systemPromptModeLabel 派生本次生效模式，取值只有三种：
//   - 覆盖：fallback 非空。首条 system 一定是这段配置文本，客户端原文被丢弃（客户端本来没给 system 也一样算覆盖）；
//   - 透传：fallback 为空且客户端自带 system，原样发给上游；
//   - 保底：fallback 为空且客户端没给 system，发内置兜底文本。
//
// 文本的具体来源另由 fallback_source 字段细分（配置覆盖 / 配置保底 / 内置保底 / 客户端透传）。
func systemPromptModeLabel(settings promptSettings, injected bool) string {
	if systemPromptFallbackActive(settings) {
		return "覆盖"
	}
	if injected {
		return "保底"
	}
	return "透传"
}

func configuredFallbackSystemPrompt() string {
	fallback := configuredSystemPrompts().fallback
	if strings.TrimSpace(fallback) == "" {
		return defaultSystemPrompt
	}
	return fallback
}

// systemPromptBodyLoggingEnabled 报告是否允许把提示词正文写入 JSON 调试日志。
// 需要 debug.enabled 与 debug.logSystemPrompt 同时开启；普通运行日志永远不记录正文。
func systemPromptBodyLoggingEnabled() bool {
	return cfg.DebugEnabled && cfg.DebugLogSystemPrompt
}

// resolveUpstreamSystemText 返回本次请求真正会发给上游的首条 system 文本及其生效模式、文本来源。
// 供 token 估算这类只读场景复用同一套规则，避免在别处再写一遍 fallback/force 判断而与之漂移。
// 返回值的模式与来源取值与 [系统提示词规则] 日志、system_prompt_policy_applied 事件保持一致。
func resolveUpstreamSystemText(clientSystem string) (string, string, string) {
	settings := configuredSystemPrompts()
	clientEmpty := strings.TrimSpace(clientSystem) == ""
	text, source := clientSystem, "客户端透传"
	switch {
	case systemPromptFallbackActive(settings):
		text, source = settings.fallback, "配置覆盖"
		if clientEmpty {
			source = "配置保底"
		}
	case clientEmpty:
		text, source = defaultSystemPrompt, "内置保底"
	}
	// force 与覆盖/透传正交：始终追加到末尾（与 prepareSystemPromptForUpstream 的字符串形态一致）。
	if force := settings.force; strings.TrimSpace(force) != "" {
		if text == "" {
			text = force
		} else {
			text = text + "\n\n" + force
		}
	}
	return text, systemPromptModeLabel(settings, clientEmpty), source
}

// systemContentText 提取 system 内容的纯文本：字符串原样返回；
// 内容块数组拼接各 text 块（换行分隔）。第二个返回值是不可直接取正文时的形态说明。
func systemContentText(content any) (string, string) {
	switch v := content.(type) {
	case string:
		return v, "字符串"
	case []any:
		parts := make([]string, 0, len(v))
		for _, block := range v {
			item, ok := block.(map[string]any)
			if !ok {
				continue
			}
			// 只取 text 块；缺省 type 的块按 text 处理，其余形态（图片等）不计入正文。
			if kind, _ := item["type"].(string); kind != "" && !strings.EqualFold(kind, "text") {
				continue
			}
			if text, ok := item["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n"), "内容块数组"
	case nil:
		return "", "空内容"
	default:
		return "", "未支持的内容类型"
	}
}

// applySystemPromptOverride 在 fallback 非空时，用 fallback 整体替换首条 system 的内容，
// 客户端原文被丢弃（追加/保留客户端原文是 force 的职责）。
//
// 返回是否真的替换了客户端提供的 system，以及替换前的客户端原 system 纯文本（供调试对比）。
// 字符串形态替换为字符串；内容块数组保留数组形态，用一个 text 块承载 fallback 正文。
func applySystemPromptOverride(obj map[string]any, fallback string) (bool, string) {
	messages, _ := obj["messages"].([]any)
	if len(messages) == 0 {
		return false, ""
	}
	first, ok := messages[0].(map[string]any)
	if !ok || roleOfMessage(first) != "system" {
		return false, ""
	}
	clientBody, _ := systemContentText(first["content"])
	switch first["content"].(type) {
	case []any:
		first["content"] = []any{map[string]any{"type": "text", "text": fallback}}
	default:
		first["content"] = fallback
	}
	return true, clientBody
}

// prepareSystemPromptForUpstream 在 Chat、Responses、Messages 三个入口统一执行。
//
// 处理顺序：
//  1. 保证首条为 system（保底注入始终保留，上游 code 11128 硬校验依赖它）；
//  2. fallback 非空白：首条 system 内容整体替换为 fallback；
//  3. force 非空白：把配置文本追加到首条 system 内容末尾（后置），使其成为整段提示词中
//     位置最靠后的指令、紧贴其后的用户消息；
//
// 客户端没给 system 时，首条 system 由第 1 步注入，内容已经是 fallback（非空）或内置兜底（空），
// 此时不做第 2 步：既不需要替换，也不能把它记成"覆盖了客户端 system"。
// 内容块数组保留原来的内容块顺序，强制文本作为最后一个内容块追加在数组末尾。
func prepareSystemPromptForUpstream(obj map[string]any, r *http.Request, requestID uint64, traceID string) {
	injected := ensureLeadingSystemMessage(obj)
	settings := configuredSystemPrompts()

	overridden := false
	clientSystemBody := ""
	fallbackActive := systemPromptFallbackActive(settings)
	if !injected && fallbackActive {
		overridden, clientSystemBody = applySystemPromptOverride(obj, settings.fallback)
	}

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
	fallbackSource := "客户端透传"
	switch {
	case overridden:
		fallbackSource = "配置覆盖"
	case injected:
		fallbackSource = "内置保底"
		if fallbackActive {
			fallbackSource = "配置保底"
		}
	}
	// 结果描述与文本来源同源派生，避免两处各自判断造成日志语义漂移。
	effect := "保留客户端原有system"
	switch fallbackSource {
	case "配置覆盖":
		effect = "覆盖客户端system为配置fallback"
	case "配置保底":
		effect = "客户端未提供system，已注入配置fallback"
	case "内置保底":
		effect = "客户端未提供system，已使用内置兜底提示词"
	}
	if traceID == "" {
		traceID = debugTraceID(r)
	}
	if traceID == "" {
		traceID = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[系统提示词规则] traceId=%s requestId=%d 阶段=上游请求序列化前 生效模式=%s 文本来源=%s 强制全局已应用=%t 强制位置=后置(紧贴用户消息) 强制内容类型=%s 强制字符数=%d 结果=%s且未记录提示词正文",
		traceID, requestID, systemPromptModeLabel(settings, injected), fallbackSource, forced, kind, utf8.RuneCountInString(force), effect)

	fields := map[string]any{
		"fallback_source": fallbackSource, "forced_applied": forced,
		"forced_position": "append_after_client_system", "forced_content_kind": kind,
		"forced_chars":     utf8.RuneCountInString(force),
		"prompt_mode":      systemPromptModeLabel(settings, injected),
		"override_applied": overridden,
		"business_impact":  "按配置在请求出站前处理system；fallback 非空即覆盖客户端 system，为空则原样透传；正文仅在显式开启 debug.logSystemPrompt 时记录",
	}
	// 长度与指纹始终记录：不含正文，既能对比「这一版提示词到底发出去了吗」，也不会让日志体积随人设长度膨胀。
	// final_system_kind 一并记录，避免"不支持的内容类型"被读成"没有 system"（那时字符数为 0）。
	finalBody, finalKind := systemContentTextOf(obj)
	fields["final_system_kind"] = finalKind
	fields["final_system_chars"] = utf8.RuneCountInString(finalBody)
	fields["final_system_sha256_prefix"] = bodyHashPrefix([]byte(finalBody))
	if overridden {
		fields["client_system_chars"] = utf8.RuneCountInString(clientSystemBody)
		fields["client_system_sha256_prefix"] = bodyHashPrefix([]byte(clientSystemBody))
	}
	// 需求：调试日志记录提示词正文，默认关闭，必须 debug.enabled 与 debug.logSystemPrompt 同时开启。
	if systemPromptBodyLoggingEnabled() {
		fields["final_system_body"] = finalBody
		if overridden {
			fields["client_system_body"] = clientSystemBody
		}
	}
	debugEvent(r, "debug", "system_prompt_policy_applied", fields)
}

// systemContentTextOf 读取处理完成后首条消息（system）的纯文本与内容形态，供调试日志记录。
// 内容形态取值与 systemContentText 的第二个返回值一致：字符串 / 内容块数组 / 空内容 / 未支持的内容类型。
func systemContentTextOf(obj map[string]any) (string, string) {
	messages, _ := obj["messages"].([]any)
	if len(messages) == 0 {
		return "", "空内容"
	}
	first, ok := messages[0].(map[string]any)
	if !ok {
		return "", "空内容"
	}
	text, kind := systemContentText(first["content"])
	return text, kind
}
