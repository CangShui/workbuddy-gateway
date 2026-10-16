package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// WorkBuddy 5.8.4 的 buildConversationHeaders 使用 session.id、当前轮次 ID，
// 以及每次模型调用新生成的 UUID（message ID 去掉连字符）。不要用普通 TraceID 代替会话。
const (
	conversationIDHeader        = "X-Conversation-ID"
	conversationRequestIDHeader = "X-Conversation-Request-ID"
	conversationMessageIDHeader = "X-Conversation-Message-ID"
)

type conversationContextKey struct{}

type conversationHeaders struct {
	ID        string
	RequestID string
	MessageID string
	Source    string
}

// 非 UUID 的客户端会话键规范化为 UUIDv5，不直接发送或记录任意客户端文本。
// 命名空间固定、与标准 DNS/URL 命名空间分离；API Key 仅参与摘要，绝不落日志。
var conversationNamespace = uuid.MustParse("7518352d-f297-5ede-9583-a32f3413b74d")

func usableConversationSeed(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func normalizedConversationID(value, kind, caller string) string {
	if id, err := uuid.Parse(value); err == nil && id != uuid.Nil {
		// 保留官方主路径所用的 32 位无连字符格式；其他 UUID 表示规范为标准格式。
		if len(value) == 32 {
			return strings.ToLower(value)
		}
		return id.String()
	}
	return uuid.NewSHA1(conversationNamespace, []byte(caller+"\x00"+kind+"\x00"+value)).String()
}

func conversationHeadersFor(r *http.Request) *conversationHeaders {
	if r == nil {
		return nil
	}
	value, _ := r.Context().Value(conversationContextKey{}).(*conversationHeaders)
	return value
}

// prepareConversationContext 必须在协议转换前读取原请求的会话元数据。
// 转换后仍使用同一个 request context，三个协议、流式/聚合、换号/重试共享同一组头。
// 无客户端会话键时每个 HTTP 调用独立生成，不根据提示词/模型名推测或合并会话。
func prepareConversationContext(r *http.Request, body map[string]any) *conversationHeaders {
	if existing := conversationHeadersFor(r); existing != nil {
		return existing
	}
	h := &conversationHeaders{
		ID: uuid.NewString(), RequestID: uuid.NewString(),
		MessageID: strings.ReplaceAll(uuid.NewString(), "-", ""), Source: "独立生成",
	}
	if r == nil {
		return h
	}
	caller := apiKeyFingerprint(r.Header.Get("Authorization"))
	if caller == "" {
		caller = apiKeyFingerprint(r.Header.Get("x-api-key"))
	}
	metadata, _ := body["metadata"].(map[string]any)
	bodyValue := func(object map[string]any, names ...string) string {
		for _, name := range names {
			if value, ok := object[name].(string); ok {
				if seed := usableConversationSeed(value); seed != "" {
					return seed
				}
			}
		}
		return ""
	}
	seed := usableConversationSeed(r.Header.Get(conversationIDHeader))
	if seed != "" {
		h.Source = "客户端会话头"
	} else if seed = usableConversationSeed(r.Header.Get("X-Session-ID")); seed != "" {
		h.Source = "客户端Session头"
	} else if seed = bodyValue(body, "conversation_id", "conversationId", "session_id", "sessionId"); seed != "" {
		h.Source = "请求会话字段"
	} else if seed = bodyValue(metadata, "conversation_id", "conversationId", "session_id", "sessionId"); seed != "" {
		h.Source = "请求metadata会话字段"
	}
	if seed != "" {
		h.ID = normalizedConversationID(seed, "conversation", caller)
	}
	for _, field := range []struct {
		header string
		names  []string
		target *string
		kind   string
	}{
		{conversationRequestIDHeader, []string{"conversation_request_id", "conversationRequestId"}, &h.RequestID, "request"},
		{conversationMessageIDHeader, []string{"conversation_message_id", "conversationMessageId"}, &h.MessageID, "message"},
	} {
		value := usableConversationSeed(r.Header.Get(field.header))
		if value == "" {
			value = bodyValue(body, field.names...)
		}
		if value == "" {
			value = bodyValue(metadata, field.names...)
		}
		if value != "" {
			*field.target = normalizedConversationID(value, field.kind, caller+"\x00"+h.ID)
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), conversationContextKey{}, h))
	trace := debugTraceID(r)
	if trace == "" {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[会话头准备] traceId=%s 来源=%s 会话ID已就绪=true 轮次ID已就绪=true 消息ID已就绪=true 说明=同一逻辑请求的重试和换号沿用同一组标识；不记录会话键、提示词或令牌",
		trace, h.Source)
	return h
}

// backendHeaders 的统一兜底也覆盖 CLI probe 和后台价格探测。
// 只有模型推理调用经过这里；登录、续期、额度查询不注入会话头。
func setConversationHeaders(req *http.Request) {
	h := conversationHeadersFor(req)
	if h == nil {
		h = prepareConversationContext(req, nil)
	}
	req.Header.Set(conversationIDHeader, h.ID)
	req.Header.Set(conversationRequestIDHeader, h.RequestID)
	req.Header.Set(conversationMessageIDHeader, h.MessageID)
}
