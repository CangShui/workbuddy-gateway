package main

import (
	"context"
	"log"
	"net/http"
)

type identityRequestContextKey struct{}

type identityRequestState struct {
	settings           identitySettings
	originalsProcessed bool
}

func identityStateForRequest(r *http.Request) *identityRequestState {
	if state, ok := r.Context().Value(identityRequestContextKey{}).(*identityRequestState); ok {
		return state
	}
	state := &identityRequestState{settings: identitySnapshot()}
	*r = *r.WithContext(context.WithValue(r.Context(), identityRequestContextKey{}, state))
	return state
}

// Wildcards see original client blocks only once, before protocol concatenation.
// This also prevents a later suffix in another block being consumed on a second pass.
func applyOriginalIdentityReplacements(obj map[string]any, r *http.Request, requestID uint64) {
	state := identityStateForRequest(r)
	applyIdentityRulePass(obj, r, requestID, !state.originalsProcessed)
	state.originalsProcessed = true
}

// Config-injected fallback/force is a new independent block, not client text.
func applyIdentityReplacements(obj map[string]any, r *http.Request, requestID uint64) {
	applyIdentityRulePass(obj, r, requestID, true)
}

// One config snapshot per request, shared by all roles, blocks and retries.
// Neither the matcher nor this traversal knows any client-specific phrase.
func applyIdentityRulePass(obj map[string]any, r *http.Request, requestID uint64, allowWildcard bool) {
	settings := identityStateForRequest(r).settings
	if !settings.enabled {
		logIdentityReplacementPass(r, requestID, false, 0, 0, 0, "identity_disabled")
		return
	}
	messages, _ := obj["messages"].([]any)
	changed, literal, wildcard := 0, 0, 0
	normalize := func(text string) string {
		result, l, w := settings.normalize(text, allowWildcard)
		literal, wildcard = literal+l, wildcard+w
		if result != text {
			changed++
		}
		return result
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch roleOfMessage(message) {
		case "system", "developer", "assistant":
		default:
			continue
		}
		switch content := message["content"].(type) {
		case string:
			message["content"] = normalize(content)
		case []any:
			for _, rawBlock := range content {
				block, ok := rawBlock.(map[string]any)
				if !ok || block["type"] != "text" {
					continue
				}
				if text, ok := block["text"].(string); ok {
					block["text"] = normalize(text)
				}
			}
		}
	}
	result := "applied"
	if changed == 0 {
		result = "no_change"
	}
	logIdentityReplacementPass(r, requestID, true, changed, literal, wildcard, result)
}

func logIdentityReplacementPass(r *http.Request, requestID uint64, enabled bool, changed, literal, wildcard int, result string) {
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[配置文字替换] traceId=%s requestId=%d 启用=%t 修改文本块=%d 字面命中=%d 首位通配命中=%d 结果=%s 说明=applied为已替换，no_change为原样保留，identity_disabled为配置关闭；仅处理system/developer/assistant，未记录规则或提示词正文",
		trace, requestID, enabled, changed, literal, wildcard, result)
	event := "identity_replacements_applied"
	if changed == 0 {
		event = "identity_replacements_skipped"
	}
	debugEvent(r, "info", event, map[string]any{
		"modified_text_blocks": changed, "literal_matches": literal,
		"wildcard_matches": wildcard, "prompt_body_logged": false, "result": result,
	})
}

// Role normalization is protocol structure handling, not a text replacement.
func normalizeDeveloperRoles(obj map[string]any, r *http.Request, requestID uint64) int {
	messages, _ := obj["messages"].([]any)
	changed := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if ok && roleOfMessage(message) == "developer" {
			message["role"] = "system"
			changed++
		}
	}
	if changed > 0 {
		trace := debugTraceID(r)
		if trace == "" {
			trace = r.Header.Get("X-Trace-ID")
		}
		log.Printf("[消息角色兼容] traceId=%s requestId=%d 转换条数=%d 结果=developer归一为system 说明=仅修改角色字段，不修改文字内容",
			trace, requestID, changed)
		debugEvent(r, "debug", "developer_roles_normalized", map[string]any{"messages": changed})
	}
	return changed
}
