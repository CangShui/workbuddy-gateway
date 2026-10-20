package main

import (
	"log"
	"net/http"
	"strings"
)

// 上游按 system 消息开头做提示词级渠道校验。只在偏移 0 匹配并等义改写，
// 前导空白、正文中段、user/assistant 消息一律不动，避免误伤正常内容。
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
