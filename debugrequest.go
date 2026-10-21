package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
)

// 完整请求日志是显式开关，普通 debug 仍不记录正文。所有采集只读副本。
var debugFullRequests atomic.Bool
var fullRequestTokenPattern = regexp.MustCompile(`(?:github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{16,})`)
var fullRequestPrivateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

func fullRequestText(text string) string {
	text = fullRequestTokenPattern.ReplaceAllString(text, "[REDACTED]")
	text = fullRequestPrivateKeyPattern.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
	return redactSensitiveText(text)
}

var debugRequestDigestKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil
	}
	return key
}()

func fullRequestLoggingEnabled(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost || !debugFullRequests.Load() || !debugLoggingEnabled() {
		return false
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/chat/completions", "/v1/responses", "/responses", "/v1/messages", "/messages":
		return true
	}
	return false // 管理配置、登录、凭据接口不记录正文。
}

func fullRequestSecretField(key string) bool {
	k := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
	return secretLogField(key) || strings.Contains(k, "authorization") ||
		strings.Contains(k, "apikey") || strings.Contains(k, "password") ||
		strings.Contains(k, "secret") || strings.Contains(k, "cookie") ||
		strings.HasSuffix(k, "token") || strings.HasSuffix(k, "signature") ||
		k == "key" || k == "sig" || k == "credential" || k == "credentials"
}

func fullRequestURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return fullRequestText(raw)
	}
	if u.User != nil {
		u.User = url.User("[REDACTED]")
	}
	if u.RawQuery != "" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			u.RawQuery = "query=[REDACTED]" // malformed query must not leak credentials
		} else {
			for k, values := range q {
				if fullRequestSecretField(k) {
					q[k] = []string{"[REDACTED]"}
				} else {
					for i := range values {
						values[i] = fullRequestText(values[i])
					}
				}
			}
			u.RawQuery = q.Encode()
		}
	}
	return fullRequestText(u.String())
}

func fullRequestRedact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if fullRequestSecretField(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = fullRequestRedact(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = fullRequestRedact(value)
		}
		return out
	case string:
		if strings.HasPrefix(x, "https://") || strings.HasPrefix(x, "http://") {
			return fullRequestURL(x)
		}
		return fullRequestText(x)
	default:
		return v
	}
}

func fullRequestDigest(v any) string {
	if len(debugRequestDigestKey) == 0 {
		return "unavailable"
	}
	b, _ := json.Marshal(v)
	h := hmac.New(sha256.New, debugRequestDigestKey)
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func fullRequestHeaders(h http.Header) (map[string]any, map[string]string) {
	out, digests := map[string]any{}, map[string]string{}
	for name, values := range h {
		if fullRequestSecretField(name) {
			out[name] = "[REDACTED]"
			digests["header."+name] = fullRequestDigest(values)
			continue
		}
		copied := make([]any, len(values))
		for i, v := range values {
			copied[i] = fullRequestRedact(v)
		}
		out[name] = copied
	}
	return out, digests
}

func fullRequestBody(raw []byte) map[string]any {
	fields := map[string]any{"body_bytes": len(raw), "body_digest": fullRequestDigest(string(raw)),
		"body_truncated": false}
	// UseNumber preserves integers beyond float64 precision. Decode only a log copy,
	// retaining all messages/tools/media; malformed/partial JSON is explicitly omitted.
	var obj any
	if !json.Valid(raw) {
		fields["body_capture"] = "invalid_or_partial_json_omitted"
		return fields
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		fields["body_capture"] = "decode_failed_omitted"
		return fields
	}
	fields["body_capture"] = "complete_redacted_json"
	fields["body"] = fullRequestRedact(obj)
	return fields
}

func fullRequestEnvelope(req *http.Request) map[string]any {
	headers, digests := fullRequestHeaders(req.Header)
	return map[string]any{
		"method": req.Method, "url": fullRequestURL(req.URL.String()), "host": req.Host,
		"headers": headers, "credential_digests": digests,
		"content_length": req.ContentLength, "transfer_encoding": append([]string(nil), req.TransferEncoding...),
		"close": req.Close,
	}
}

func debugFullIncomingHeaders(r *http.Request) {
	if !fullRequestLoggingEnabled(r) {
		return
	}
	debugEvent(r, "info", "full_request_inbound_headers", map[string]any{
		"request": fullRequestEnvelope(r), "body_capture": "not_read_yet",
		"message": "入站请求头已记录；正文仅在通过鉴权并读取后另行记录，未为采集额外读取请求流",
	})
}

func debugFullIncomingBody(r *http.Request, raw []byte, valid, partial bool) {
	if !fullRequestLoggingEnabled(r) {
		return
	}
	fields := fullRequestBody(raw)
	fields["request"], fields["json_valid"], fields["body_read_partial"] = fullRequestEnvelope(r), valid, partial
	debugEvent(r, "info", "full_request_inbound", fields)
}

func debugFullUpstreamRequest(r, req *http.Request, raw []byte, attempt int) {
	if !fullRequestLoggingEnabled(r) {
		return
	}
	fields := fullRequestBody(raw)
	fields["request"], fields["attempt"], fields["upstream_request_id"] = fullRequestEnvelope(req), attempt, req.Header.Get("X-Request-ID")
	fields["content_encoding"] = req.Header.Get("Content-Encoding")
	fields["wire_body_bytes"] = req.ContentLength
	fields["body_representation"] = "decoded_json_before_content_encoding"
	fields["message"] = "协议转换和全部修复之后、HTTP发送之前的完整请求；不是TLS原始抓包；不改变上游请求"
	debugEvent(r, "info", "full_request_upstream", fields)
}

func debugFullUpstreamResponse(r, req *http.Request, resp *http.Response, attempt int, headersWritten, bodyWritten bool, err error) {
	if !fullRequestLoggingEnabled(r) {
		return
	}
	fields := map[string]any{"attempt": attempt, "upstream_request_id": req.Header.Get("X-Request-ID"),
		"headers_written": headersWritten, "body_written": bodyWritten, "network_error": err != nil}
	if resp != nil {
		headers, _ := fullRequestHeaders(resp.Header)
		fields["status_code"], fields["response_headers"] = resp.StatusCode, headers
	}
	if err != nil {
		fields["error_type"] = debugErrorType(err)
	}
	debugEvent(r, "info", "full_request_upstream_result", fields)
}

func debugFullUpstreamError(r *http.Request, resp *http.Response, raw []byte) {
	if !fullRequestLoggingEnabled(r) {
		return
	}
	fields := fullRequestBody(raw)
	fields["status_code"] = resp.StatusCode
	if resp.Request != nil {
		fields["upstream_request_id"] = resp.Request.Header.Get("X-Request-ID")
	}
	fields["message"] = "上游错误JSON已记录；成功响应流不被采集或预读"
	debugEvent(r, "warn", "full_request_upstream_error", fields)
}
