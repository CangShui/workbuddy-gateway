package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const runtimeConfigFile = "config.json"

type runtimeFileConfig struct {
	Debug struct {
		Enabled bool `json:"enabled"`
	} `json:"debug"`
	// Upstream 段可选，用于按网络状况调整上游超时（单位秒，<=0 或省略表示用默认值）。
	Upstream struct {
		// HeaderTimeoutSeconds：请求发送完成 → 收到响应头的等待上限。
		HeaderTimeoutSeconds int `json:"headerTimeoutSeconds"`
		// IdleTimeoutSeconds：流式响应体的空闲读超时（持续有数据则不超时）。
		IdleTimeoutSeconds int `json:"idleTimeoutSeconds"`
	} `json:"upstream"`
	// Models 段可选，用于按模型名做黑白名单控制（大小写不敏感）。
	Models struct {
		// Blocklist：被禁用的模型名列表，命中即拒绝。
		Blocklist []string `json:"blocklist"`
		// Allowlist：白名单；非空时只允许列表内的模型，其余一律拒绝。
		Allowlist []string `json:"allowlist"`
	} `json:"models"`
}

type debugRequestContextKey struct{}

type debugRequestContext struct {
	mu                  sync.RWMutex
	start               time.Time
	traceID             string
	requestID           uint64
	parentRequestID     string
	model               string
	account             string
	stream              bool
	traceHeaderReceived bool
	deadlinePresent     bool
	deadlineRemainingMS int64
}

type debugJSONSink struct {
	mu     sync.Mutex
	writer io.Writer
	closer io.Closer
}

var (
	debugSinkMu sync.RWMutex
	debugSink   *debugJSONSink
	instanceID  = hostInstanceID()
)

func loadRuntimeConfig(path string) error {
	cfg.DebugEnabled = false
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取调试配置 %s: %w", path, err)
	}
	defer f.Close()

	var fileCfg runtimeFileConfig
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fileCfg); err != nil {
		return fmt.Errorf("解析调试配置 %s: %w", path, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return fmt.Errorf("解析调试配置 %s: %w", path, err)
	}
	cfg.DebugEnabled = fileCfg.Debug.Enabled

	// 模型黑白名单：先清空再按配置重建，避免热加载时残留旧规则。
	setModelFilter(fileCfg.Models.Blocklist, fileCfg.Models.Allowlist)

	// 上游超时覆盖：仅当配置为正数时生效，否则保持内置默认值。
	upstreamHeaderTimeout = upstreamHeaderTimeoutDefault
	upstreamIdleTimeout = upstreamIdleTimeoutDefault
	if secs := fileCfg.Upstream.HeaderTimeoutSeconds; secs > 0 {
		upstreamHeaderTimeout = time.Duration(secs) * time.Second
	}
	if secs := fileCfg.Upstream.IdleTimeoutSeconds; secs > 0 {
		upstreamIdleTimeout = time.Duration(secs) * time.Second
	}
	return nil
}

// modelFilter 保存生效的模型黑白名单（已归一化为小写、去重）。
type modelFilter struct {
	blocklist map[string]bool
	allowlist map[string]bool
}

var (
	modelFilterMu sync.RWMutex
	currentFilter = modelFilter{}
)

// setModelFilter 用配置重建模型黑白名单。空列表表示该维度不限制。
func setModelFilter(blocklist, allowlist []string) {
	build := func(names []string) map[string]bool {
		if len(names) == 0 {
			return nil
		}
		out := make(map[string]bool, len(names))
		for _, name := range names {
			key := normalizeModelName(name)
			if key != "" {
				out[key] = true
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	modelFilterMu.Lock()
	currentFilter = modelFilter{blocklist: build(blocklist), allowlist: build(allowlist)}
	modelFilterMu.Unlock()
}

// modelDisabled 判断某模型是否被配置禁用，并返回中文原因。
// 规则：黑名单命中即禁用；白名单非空且未命中则禁用；两者都未配置则不限制。
func modelDisabled(model string) (bool, string) {
	key := normalizeModelName(model)
	if key == "" {
		return false, ""
	}
	modelFilterMu.RLock()
	filter := currentFilter
	modelFilterMu.RUnlock()

	if filter.blocklist != nil && filter.blocklist[key] {
		return true, fmt.Sprintf("模型 %s 已被网关禁用（命中黑名单），请联系管理员调整 config.json", model)
	}
	if filter.allowlist != nil && !filter.allowlist[key] {
		return true, fmt.Sprintf("模型 %s 已被网关禁用（不在白名单内），请联系管理员调整 config.json", model)
	}
	return false, ""
}

// modelFilterConfigured 报告当前是否配置了任何黑白名单（用于启动横幅展示）。
func modelFilterConfigured() bool {
	modelFilterMu.RLock()
	defer modelFilterMu.RUnlock()
	return currentFilter.blocklist != nil || currentFilter.allowlist != nil
}

// modelFilterSummary 返回生效的黑名单/白名单条目数（用于启动横幅展示）。
func modelFilterSummary() (blocked, allowed int) {
	modelFilterMu.RLock()
	defer modelFilterMu.RUnlock()
	return len(currentFilter.blocklist), len(currentFilter.allowlist)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("配置文件包含多个 JSON 值")
}

func initDebugLogging() (func(), error) {
	if !cfg.DebugEnabled {
		return func() {}, nil
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("创建调试日志目录 %s: %w", logDir, err)
	}
	path := filepath.Join(logDir, "debug-"+time.Now().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开调试日志 %s: %w", path, err)
	}
	sink := &debugJSONSink{writer: f, closer: f}
	debugSinkMu.Lock()
	debugSink = sink
	debugSinkMu.Unlock()
	return func() {
		debugSinkMu.Lock()
		if debugSink == sink {
			debugSink = nil
		}
		debugSinkMu.Unlock()
		_ = sink.closer.Close()
	}, nil
}

func debugLoggingEnabled() bool {
	debugSinkMu.RLock()
	enabled := debugSink != nil
	debugSinkMu.RUnlock()
	return enabled
}

func hostInstanceID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "unknown"
	}
	return host
}

func ensureDebugRequestContext(r *http.Request, traceID string, start time.Time) *debugRequestContext {
	if current := debugContextFromRequest(r); current != nil {
		return current
	}
	if start.IsZero() {
		start = time.Now()
	}
	received := strings.TrimSpace(r.Header.Get("X-Trace-ID")) != ""
	if traceID == "" {
		traceID = strings.TrimSpace(r.Header.Get("X-Trace-ID"))
	}
	if traceID == "" {
		traceID = newTraceID()
	}
	parentID := strings.TrimSpace(r.Header.Get("X-Parent-Request-ID"))
	if parentID == "" {
		parentID = strings.TrimSpace(r.Header.Get("X-Request-ID"))
	}
	deadline, hasDeadline := r.Context().Deadline()
	remaining := int64(0)
	if hasDeadline {
		remaining = time.Until(deadline).Milliseconds()
	}
	state := &debugRequestContext{
		start:               start,
		traceID:             traceID,
		parentRequestID:     parentID,
		traceHeaderReceived: received,
		deadlinePresent:     hasDeadline,
		deadlineRemainingMS: remaining,
	}
	*r = *r.WithContext(context.WithValue(r.Context(), debugRequestContextKey{}, state))
	return state
}

func newTraceID() string {
	return uuid.NewString()
}

func debugContextFromRequest(r *http.Request) *debugRequestContext {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(debugRequestContextKey{}).(*debugRequestContext)
	return state
}

func requestIDFor(r *http.Request) uint64 {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.requestID == 0 {
			state.requestID = atomic.AddUint64(&reqCounter, 1)
		}
		return state.requestID
	}
	return atomic.AddUint64(&reqCounter, 1)
}

func requestStartFor(r *http.Request) time.Time {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.start
	}
	return time.Now()
}

func debugSetModelAndStream(r *http.Request, model string, stream bool) {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.Lock()
		state.model = model
		state.stream = stream
		state.mu.Unlock()
	}
}

func debugSetAccount(r *http.Request, account string) {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.Lock()
		state.account = filepath.Base(account)
		state.mu.Unlock()
	}
}

func debugTraceID(r *http.Request) string {
	if state := debugContextFromRequest(r); state != nil {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.traceID
	}
	return ""
}

func debugEvent(r *http.Request, level, event string, fields map[string]any) {
	debugSinkMu.RLock()
	sink := debugSink
	debugSinkMu.RUnlock()
	if sink == nil || r == nil {
		return
	}

	state := ensureDebugRequestContext(r, "", time.Time{})
	requestIDFor(r)
	state.mu.RLock()
	start := state.start
	traceID := state.traceID
	requestID := state.requestID
	parentRequestID := state.parentRequestID
	model := state.model
	account := state.account
	stream := state.stream
	traceHeaderReceived := state.traceHeaderReceived
	deadlinePresent := state.deadlinePresent
	deadlineRemainingMS := state.deadlineRemainingMS
	state.mu.RUnlock()

	record := map[string]any{
		"timestamp":             time.Now().UTC().Format(time.RFC3339Nano),
		"level":                 level,
		"event":                 event,
		"trace_id":              traceID,
		"request_id":            requestID,
		"parent_request_id":     parentRequestID,
		"service":               "workbuddy-gateway",
		"instance":              instanceID,
		"version":               version,
		"route":                 r.URL.Path,
		"method":                r.Method,
		"model":                 model,
		"acc":                   account,
		"stream":                stream,
		"elapsed_ms":            time.Since(start).Milliseconds(),
		"client_ip":             requestClientIP(r),
		"remote_addr":           r.RemoteAddr,
		"user_agent":            r.UserAgent(),
		"host":                  r.Host,
		"scheme":                requestScheme(r),
		"content_type":          r.Header.Get("Content-Type"),
		"content_length":        r.ContentLength,
		"transfer_encoding":     strings.Join(r.TransferEncoding, ","),
		"x_forwarded_for":       r.Header.Get("X-Forwarded-For"),
		"x_real_ip":             r.Header.Get("X-Real-IP"),
		"trace_header_received": traceHeaderReceived,
		"deadline_present":      deadlinePresent,
		"deadline_remaining_ms": deadlineRemainingMS,
		"protocol":              r.Proto,
		"tls":                   r.TLS != nil,
		"authorization_present": strings.TrimSpace(r.Header.Get("Authorization")) != "",
		"api_key_fingerprint":   apiKeyFingerprint(r.Header.Get("Authorization")),
	}
	for key, value := range fields {
		record[key] = value
	}

	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	sink.mu.Lock()
	_, _ = sink.writer.Write(append(data, '\n'))
	sink.mu.Unlock()
}

func debugBodyReadStarted(r *http.Request) time.Time {
	if !debugLoggingEnabled() {
		return time.Time{}
	}
	started := time.Now()
	debugEvent(r, "debug", "request_body_read_started", debugBodyFields(r, nil, 0, 0, false, nil))
	return started
}

func debugElapsedSince(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

func debugBodyReadCompleted(r *http.Request, body []byte, readDuration, decodeDuration time.Duration, jsonValid bool, decodeErr error) {
	if !debugLoggingEnabled() {
		return
	}
	level := "debug"
	fields := debugBodyFields(r, body, readDuration, decodeDuration, jsonValid, nil)
	if decodeErr != nil {
		level = "warn"
		fields["json_error"] = safeDebugError(decodeErr)
	}
	debugEvent(r, level, "request_body_read_completed", fields)
}

func debugBodyReadFailed(r *http.Request, body []byte, readStarted time.Time, readErr error) {
	if !debugLoggingEnabled() {
		return
	}
	debugEvent(r, "error", "request_body_read_failed", debugBodyFields(r, body, time.Since(readStarted), 0, false, readErr))
}

func debugBodyFields(r *http.Request, body []byte, readDuration, decodeDuration time.Duration, jsonValid bool, readErr error) map[string]any {
	declared := r.ContentLength
	fields := map[string]any{
		"declared_body_bytes": declared,
		"actual_body_bytes":   len(body),
		"body_read_ms":        durationMilliseconds(readDuration),
		"body_sha256_prefix":  bodyHashPrefix(body),
		"json_valid":          jsonValid,
		"json_decode_ms":      durationMilliseconds(decodeDuration),
		"body_limit_bytes":    int64(0),
		"body_limit_exceeded": false,
		"read_error_type":     "",
		"read_error":          "",
	}
	if readErr != nil {
		fields["read_error_type"] = debugErrorType(readErr)
		fields["read_error"] = safeDebugError(readErr)
	}
	return fields
}

func durationMilliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func bodyHashPrefix(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:12]
}

func debugErrorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	default:
		t := reflect.TypeOf(err)
		if t == nil {
			return "unknown"
		}
		return t.String()
	}
}

func safeDebugError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " ")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func requestClientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
		return forwarded
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func apiKeyFingerprint(authorization string) string {
	parts := strings.Fields(authorization)
	secret := strings.TrimSpace(authorization)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		secret = parts[1]
	}
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:12]
}
