package main

import (
	"log"
	"net/http"
	"strings"
)

// 已知身份前缀只在偏移 0 匹配并等义改写；前导空白和正文中段不动。
// 除既有 system 规则外，共享步骤也覆盖 assistant 历史；user/tool 不参与。
var blockedSystemPromptPrefixes = []string{
	"You are an AI agent powered by DeepSeek Harness",
}

const blockedSystemPromptPrefixReplacement = "You are an AI agent powered by a large language model"

// sanitizeSystemPromptPrefix 返回改写后的文本以及是否命中。匹配忽略大小写，
// 但只允许从字符串第一个字节开始；前缀后的正文逐字保留。
func sanitizeSystemPromptPrefix(text string) (string, bool) {
	for _, prefix := range blockedSystemPromptPrefixes {
		if len(text) < len(prefix) || !strings.EqualFold(text[:len(prefix)], prefix) {
			continue
		}
		return blockedSystemPromptPrefixReplacement + text[len(prefix):], true
	}
	return text, false
}

// sanitizeSystemMessageContent 处理 string 与内容块数组；数组只处理第一个
// 文本块，其余文本、图片和工具内容保持原序与原文不变。
func sanitizeSystemMessageContent(content any) (any, bool) {
	switch value := content.(type) {
	case string:
		return sanitizeSystemPromptPrefix(value)
	case []any:
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if part["type"] != "text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			if rewritten, changed := sanitizeSystemPromptPrefix(text); changed {
				part["text"] = rewritten
				return value, true
			}
			return value, false
		}
	}
	return content, false
}

// normalizeDSHIdentityPrefixes 扫描 system/assistant 的每个标准文本块。
// 旧 sanitizeMessages 只负责原始 system 首块；本步骤同时覆盖合并前的前缀和
// 配置 fallback/force 注入后的内容。user/tool、图片、reasoning 和工具参数不动。
func normalizeDSHIdentityPrefixes(obj map[string]any, role string) int {
	messages, _ := obj["messages"].([]any)
	modified := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || roleOfMessage(message) != role {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if rewritten, changed := sanitizeSystemPromptPrefix(content); changed {
				message["content"] = rewritten
				modified++
			}
		case []any:
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if part["type"] != "text" {
					continue
				}
				if text, ok := part["text"].(string); ok {
					if rewritten, changed := sanitizeSystemPromptPrefix(text); changed {
						part["text"] = rewritten
						modified++
					}
				}
			}
		}
	}
	return modified
}

// logDSHPromptCompatibility 只记录固定规则和命中数量，不记录提示词正文。
func logDSHPromptCompatibility(r *http.Request, requestID uint64, obj map[string]any) {
	system := normalizeDSHIdentityPrefixes(obj, "system")
	assistant := normalizeDSHIdentityPrefixes(obj, "assistant")
	gui := normalizeDSHGUIContexts(obj)
	if system+assistant+gui == 0 {
		return
	}
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[DSH提示词兼容] traceId=%s requestId=%d system前缀=%d assistant前缀=%d GUI环境句=%d 结果=已改写为中性描述，未记录提示词正文",
		trace, requestID, system, assistant, gui)
	debugEvent(r, "info", "dsh_prompt_compatibility", map[string]any{
		"system_prefix_parts": system, "assistant_prefix_parts": assistant,
		"gui_context_matches": gui, "prompt_body_logged": false,
	})
}

// logSystemPrefixSanitized 只记录命中数量与规则，不记录提示词正文。
func logSystemPrefixSanitized(r *http.Request, requestID uint64, modified int) {
	if modified <= 0 {
		return
	}
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[提示词前缀净化] traceId=%s requestId=%d 规则=system开头DeepSeekHarness身份 修改消息数=%d 结果=已改写为中性前缀 说明=仅偏移0，正文其余部分逐字保留，不记录提示词正文",
		trace, requestID, modified)
	debugEvent(r, "info", "system_prompt_prefix_sanitized", map[string]any{
		"rule":               "deepseek_harness_system_prefix",
		"modified_messages":  modified,
		"result":             "rewritten",
		"business_impact":    "避免上游11128渠道校验；未改动非开头正文",
		"prompt_body_logged": false,
	})
}
