package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRuntimeConfig(t *testing.T) {
	old := cfg.DebugEnabled
	t.Cleanup(func() { cfg.DebugEnabled = old })

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	cfg.DebugEnabled = true
	if err := loadRuntimeConfig(missing); err != nil {
		t.Fatal(err)
	}
	if cfg.DebugEnabled {
		t.Fatal("missing config must leave debug disabled")
	}

	enabled := filepath.Join(dir, "config.json")
	if err := os.WriteFile(enabled, []byte(`{"debug":{"enabled":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(enabled); err != nil {
		t.Fatal(err)
	}
	if !cfg.DebugEnabled {
		t.Fatal("config must enable debug logging")
	}

	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"debug":{"enabled":true},"unknown":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(invalid); err == nil {
		t.Fatal("unknown config fields must be rejected")
	}
}

func TestInitDebugLoggingHonorsConfig(t *testing.T) {
	oldEnabled := cfg.DebugEnabled
	oldSink := debugSink
	t.Cleanup(func() {
		cfg.DebugEnabled = oldEnabled
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})

	chdirTemp(t)
	cfg.DebugEnabled = false
	closeDebug, err := initDebugLogging()
	if err != nil {
		t.Fatal(err)
	}
	closeDebug()
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("disabled debug logging created %s", logDir)
	}

	cfg.DebugEnabled = true
	closeDebug, err = initDebugLogging()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	ensureDebugRequestContext(req, "file-trace", time.Now())
	debugEvent(req, "info", "request_received", nil)
	closeDebug()

	path := filepath.Join(logDir, "debug-"+time.Now().Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatalf("debug file is not JSONL: %v: %s", err, data)
	}
	if record["trace_id"] != "file-trace" || record["event"] != "request_received" {
		t.Fatalf("unexpected debug file record: %#v", record)
	}
}

func TestDebugJSONFieldsAndRedaction(t *testing.T) {
	var output bytes.Buffer
	sink := &debugJSONSink{writer: &output}
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = sink
	debugSinkMu.Unlock()
	t.Cleanup(func() {
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})

	body := []byte(`{"model":"hy4-preview","stream":true,"input":"DO_NOT_LOG_THIS_BODY"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "gateway.test"
	req.Header.Set("Authorization", "Bearer super-secret-api-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CPA/1.0")
	req.Header.Set("X-Trace-ID", "trace-from-client")
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	ensureDebugRequestContext(req, "trace-from-client", time.Now())
	debugSetModelAndStream(req, "hy4-preview", true)
	debugSetAccount(req, "/tmp/workbuddy1.json")
	debugEvent(req, "info", "request_received", map[string]any{"status_code": 200})

	line := strings.TrimSpace(output.String())
	if strings.Contains(line, "super-secret-api-key") || strings.Contains(line, "DO_NOT_LOG_THIS_BODY") {
		t.Fatalf("debug log leaked secret or request body: %s", line)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("debug line is not JSON: %v\n%s", err, line)
	}
	for _, key := range []string{
		"timestamp", "level", "event", "trace_id", "request_id", "parent_request_id",
		"service", "instance", "version", "route", "method", "model", "acc", "stream",
		"elapsed_ms", "client_ip", "remote_addr", "user_agent", "host", "scheme",
		"content_type", "content_length", "transfer_encoding", "x_forwarded_for", "x_real_ip",
		"trace_header_received", "deadline_present", "deadline_remaining_ms", "protocol", "tls",
		"authorization_present", "api_key_fingerprint",
	} {
		if _, ok := record[key]; !ok {
			t.Errorf("missing required debug field %q", key)
		}
	}
	if record["trace_id"] != "trace-from-client" || record["model"] != "hy4-preview" || record["acc"] != "workbuddy1.json" {
		t.Fatalf("unexpected correlation fields: %#v", record)
	}
	if record["api_key_fingerprint"] == "" || record["api_key_fingerprint"] == "super-secret-api-key" {
		t.Fatalf("invalid API key fingerprint: %#v", record["api_key_fingerprint"])
	}
}

func TestDebugBodyEventsDoNotLogBody(t *testing.T) {
	var output bytes.Buffer
	sink := &debugJSONSink{writer: &output}
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = sink
	debugSinkMu.Unlock()
	t.Cleanup(func() {
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})

	body := []byte(`{"model":"m","secret":"BODY_SECRET"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	ensureDebugRequestContext(req, "body-trace", time.Now())
	started := debugBodyReadStarted(req)
	debugSetModelAndStream(req, "m", false)
	debugBodyReadCompleted(req, body, time.Since(started), 125*time.Microsecond, true, nil)

	if strings.Contains(output.String(), "BODY_SECRET") {
		t.Fatalf("debug body event leaked request body: %s", output.String())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two body events, got %d: %s", len(lines), output.String())
	}
	var completed map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &completed); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"declared_body_bytes", "actual_body_bytes", "body_read_ms", "body_sha256_prefix",
		"json_valid", "json_decode_ms", "body_limit_bytes", "body_limit_exceeded",
		"read_error_type", "read_error",
	} {
		if _, ok := completed[key]; !ok {
			t.Errorf("missing body debug field %q", key)
		}
	}
	if completed["event"] != "request_body_read_completed" || completed["body_sha256_prefix"] == "" {
		t.Fatalf("unexpected completed body event: %#v", completed)
	}
}

func TestDebugMiddlewareLogsRejectedRequestWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	sink := &debugJSONSink{writer: &output}
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = sink
	debugSinkMu.Unlock()
	oldAPIKey := cfg.APIKey
	cfg.APIKey = "expected-api-key"
	t.Cleanup(func() {
		cfg.APIKey = oldAPIKey
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})

	handlerReached := false
	handler := requestAuditMiddleware(corsMiddleware(authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerReached = true
	}))))
	body := `{"model":"m","secret":"REJECTED_BODY_SECRET"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-api-key")
	req.Header.Set("X-Trace-ID", "rejected-trace")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if handlerReached {
		t.Fatal("rejected request entered the business handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusUnauthorized)
	}
	logs := output.String()
	if strings.Contains(logs, "wrong-api-key") || strings.Contains(logs, "REJECTED_BODY_SECRET") {
		t.Fatalf("rejected debug log leaked a secret: %s", logs)
	}
	for _, event := range []string{"request_received", "cors_check_passed", "authentication_rejected", "response_returned"} {
		if !strings.Contains(logs, `"event":"`+event+`"`) {
			t.Errorf("missing event %s in %s", event, logs)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSON line: %v: %s", err, line)
		}
		if record["trace_id"] != "rejected-trace" {
			t.Fatalf("trace ID changed across rejected chain: %#v", record)
		}
	}
}
