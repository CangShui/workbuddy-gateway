package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolateSecrets(t *testing.T) {
	t.Helper()
	old := map[string]struct{}{}
	sensitiveSecrets.Range(func(key, value any) bool {
		old[key.(string)] = struct{}{}
		sensitiveSecrets.Delete(key)
		return true
	})
	t.Cleanup(func() {
		sensitiveSecrets.Range(func(key, _ any) bool {
			sensitiveSecrets.Delete(key)
			return true
		})
		for key := range old {
			sensitiveSecrets.Store(key, struct{}{})
		}
	})
}

type contextManifestBody struct {
	ctx              context.Context
	reader           io.Reader
	closed           bool
	cancelledAtClose bool
}

func (b *contextManifestBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.reader.Read(p)
}

func (b *contextManifestBody) Close() error {
	b.closed = true
	b.cancelledAtClose = b.ctx.Err() != nil
	return nil
}

func TestMinimalNPMCancelAfterBodyClosed(t *testing.T) {
	oldClient, oldBases := cfg.HttpClient, npmBases
	t.Cleanup(func() { cfg.HttpClient, npmBases = oldClient, oldBases })
	npmBases = []string{"http://npm.test/package"}
	var body *contextManifestBody
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		body = &contextManifestBody{ctx: r.Context(), reader: strings.NewReader(`{"version":" 9.8.7 "}`)}
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}
	version, err := fetchNPMCatalogVersion()
	if err != nil || version != "9.8.7" {
		t.Fatalf("version=%q err=%v", version, err)
	}
	if !body.closed || body.cancelledAtClose || body.ctx.Err() != context.Canceled {
		t.Fatalf("closed=%v cancelledAtClose=%v context=%v", body.closed, body.cancelledAtClose, body.ctx.Err())
	}
}

type failingManifestReader struct{}

func (failingManifestReader) Read(p []byte) (int, error) {
	return copy(p, `{"version":"incomplete"}`), io.ErrUnexpectedEOF
}

func TestMinimalNPMFallbackReleasesContexts(t *testing.T) {
	for _, first := range []string{"network", "status", "invalid_json", "read"} {
		t.Run(first, func(t *testing.T) {
			oldClient, oldBases := cfg.HttpClient, npmBases
			t.Cleanup(func() { cfg.HttpClient, npmBases = oldClient, oldBases })
			npmBases = []string{"http://npm-first.test/package", "http://npm-second.test/package"}
			var contexts []context.Context
			var bodies []*contextManifestBody
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(r *http.Request) (*http.Response, error) {
				contexts = append(contexts, r.Context())
				status := http.StatusOK
				var reader io.Reader = strings.NewReader(`{"version":"9.8.7"}`)
				if len(contexts) == 1 {
					switch first {
					case "network":
						return nil, errors.New("synthetic network failure")
					case "status":
						status = http.StatusServiceUnavailable
					case "invalid_json":
						reader = strings.NewReader("{")
					case "read":
						reader = failingManifestReader{}
					}
				} else if contexts[0].Err() != context.Canceled {
					t.Error("first context not released before fallback")
				}
				body := &contextManifestBody{ctx: r.Context(), reader: reader}
				bodies = append(bodies, body)
				return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
			})}
			version, err := fetchNPMCatalogVersion()
			if err != nil || version != "9.8.7" || len(contexts) != 2 {
				t.Fatalf("version=%q err=%v attempts=%d", version, err, len(contexts))
			}
			for _, ctx := range contexts {
				if ctx.Err() != context.Canceled {
					t.Error("request context not cancelled")
				}
			}
			for _, body := range bodies {
				if !body.closed || body.cancelledAtClose {
					t.Error("body was not closed before cancellation")
				}
			}
		})
	}
}

func TestMinimalAuthKeepsExtractionCompatibility(t *testing.T) {
	oldKey := cfg.APIKey
	t.Cleanup(func() { cfg.APIKey = oldKey })
	cfg.APIKey = "compatibility-secret"
	for _, tc := range []struct {
		name, path, authorization, xAPIKey string
		want                               int
	}{
		{"bearer", "/v1/responses", "Bearer compatibility-secret", "", 204},
		{"raw header", "/v1/responses", "compatibility-secret", "", 204},
		{"x-api-key", "/v1/messages", "", cfg.APIKey, 204},
		{"empty bearer fallback", "/v1/messages", "Bearer ", cfg.APIKey, 204},
		{"wrong authorization takes precedence", "/v1/messages", "Bearer wrong", cfg.APIKey, 401},
		{"lowercase bearer unchanged", "/v1/responses", "bearer compatibility-secret", cfg.APIKey, 401},
		{"missing", "/v1/responses", "", "", 401},
		{"different same length", "/v1/responses", "compatibility-secreu", "", 401},
		{"health exempt", "/health", "", "", 204},
		{"ping exempt", "/ping", "", "", 204},
		{"root exempt", "/", "", "", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			handler := requestAuditMiddleware(corsMiddleware(authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusNoContent)
			}))))
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("Authorization", tc.authorization)
			req.Header.Set("x-api-key", tc.xAPIKey)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.want || reached != (tc.want == 204) {
				t.Fatalf("status=%d reached=%v", w.Code, reached)
			}
		})
	}
	cfg.APIKey = ""
	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if w.Code != 204 {
		t.Fatal("optional authentication changed")
	}
}

func TestMinimalBannerHidesAPIKey(t *testing.T) {
	oldKey := cfg.APIKey
	t.Cleanup(func() { cfg.APIKey = oldKey })
	cfg.APIKey = "synthetic-banner-secret"
	var output bytes.Buffer
	printAPIAuthBanner(&output)
	if strings.Contains(output.String(), cfg.APIKey) || !strings.Contains(output.String(), "已启用 (密钥已隐藏)") {
		t.Fatalf("unsafe banner: %q", output.String())
	}
}

func TestMinimalVerify403IsNotAlwaysInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		usable bool
		err    bool
	}{
		{"usable", 200, `{"code":0,"data":{}}`, true, false},
		{"unauthorized", 401, `{"msg":"denied"}`, false, false},
		{"permission", 403, `{"code":40301,"msg":"permission denied"}`, false, true},
		{"waf", 403, "request denied by risk control", false, true},
		{"explicit invalid token", 403, `{"msg":"invalid token"}`, false, false},
		{"explicit expired login", 403, `{"msg":"登录已过期"}`, false, false},
		{"upstream unavailable", 503, `{"msg":"maintenance"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credTestServer(t, "", "", tc.status, tc.body)
			auth := StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic-verify-access"}}
			usable, err := verifyAccountUsable(context.Background(), auth)
			if usable != tc.usable || (err != nil) != tc.err {
				t.Fatalf("usable=%v err=%v", usable, err)
			}
			keep, _ := credentialStillUsableForDelete(&auth)
			if keep != (tc.usable || tc.err) {
				t.Fatalf("delete safeguard keep=%v", keep)
			}
		})
	}
	if !isAuthFailure(http.StatusForbidden, "permission denied") {
		t.Fatal("ordinary upstream 403 stop-account classification must stay unchanged")
	}
}

func TestMinimalSaveAuthCommitsMemoryAfterWrite(t *testing.T) {
	oldPath, oldAuth := cfg.AuthFile, currAuth
	t.Cleanup(func() {
		cfg.AuthFile = oldPath
		authLock.Lock()
		currAuth = oldAuth
		authLock.Unlock()
	})
	previous := &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic-previous-access"}}
	next := &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic-next-access"}}
	authLock.Lock()
	currAuth = previous
	authLock.Unlock()
	cfg.AuthFile = t.TempDir() // 写入目录应确定失败，无需修改真实文件或权限。
	if err := saveAuth(next); err == nil {
		t.Fatal("write to directory should fail")
	}
	if currAuth != previous {
		t.Fatal("failed save switched current memory credentials")
	}
	cfg.AuthFile = filepath.Join(t.TempDir(), "synthetic-auth.json")
	if err := saveAuth(next); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.AuthFile)
	if err != nil {
		t.Fatal(err)
	}
	var stored StoredAuth
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if currAuth != next || stored.Auth.AccessToken != next.Auth.AccessToken {
		t.Fatal("successful save did not update both disk and memory")
	}
}

func TestMinimalLogRedactionPreservesDiagnostics(t *testing.T) {
	isolateSecrets(t)
	registerSecrets("synthetic-known-access", "synthetic-known-access-overlap", "short", `escaped"secret`)
	for _, raw := range []string{
		`HTTP=403 code=12153 reason=permission denied traceId=trace-keep access=synthetic-known-access-overlap`,
		`HTTP=403 code=12153 reason=permission denied traceId=trace-keep {"accessToken":"unknown-access","refresh_token":"unknown-refresh","apiKey":"unknown-key"}`,
		`HTTP=403 code=12153 reason=permission denied traceId=trace-keep Authorization: Bearer unknown-bearer`,
		`HTTP=403 code=12153 reason=permission denied traceId=trace-keep x-api-key: 'unknown-key', token=short`,
		`HTTP=403 code=12153 reason=permission denied traceId=trace-keep {"accessToken":"escaped\"secret"}`,
		`HTTP=403 code=12153 reason=permission denied traceId=trace-keep api_key=synthetic-known-access`,
	} {
		got := redactSensitiveText(raw)
		if again := redactSensitiveText(got); again != got {
			t.Fatalf("redaction is not idempotent: %q -> %q", got, again)
		}
		for _, secret := range []string{"synthetic-known-access", "unknown-access", "unknown-refresh", "unknown-key", "unknown-bearer", "token=short", `escaped\"secret`} {
			if strings.Contains(got, secret) {
				t.Fatalf("secret leaked: %q", got)
			}
		}
		for _, detail := range []string{"HTTP=403", "code=12153", "reason=permission denied", "traceId=trace-keep"} {
			if !strings.Contains(got, detail) {
				t.Fatalf("diagnostic lost: %q", got)
			}
		}
	}
	rawErr := errors.New("code=12153 reason=invalid token; access_token=unknown-access traceId=trace-keep")
	before := rawErr.Error()
	if got := safeDebugError(rawErr); strings.Contains(got, "unknown-access") || !strings.Contains(got, "invalid token") {
		t.Fatalf("unsafe or unhelpful debug error: %q", got)
	}
	if rawErr.Error() != before || !isAuthFailure(0, rawErr.Error()) {
		t.Fatal("log formatting changed business error classification")
	}
}

type shortLogWriter struct{}

func (shortLogWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestMinimalRedactingWriterLengthAndErrors(t *testing.T) {
	isolateSecrets(t)
	registerSecrets("synthetic-writer-secret")
	var output bytes.Buffer
	raw := []byte("code=12153 token=synthetic-writer-secret traceId=trace-keep")
	n, err := (&redactingLogWriter{writer: &output}).Write(raw)
	if n != len(raw) || err != nil || strings.Contains(output.String(), "synthetic-writer-secret") {
		t.Fatalf("n=%d err=%v output=%q", n, err, output.String())
	}
	if _, err := (&redactingLogWriter{writer: shortLogWriter{}}).Write(raw); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write err=%v", err)
	}
}

func TestMinimalDebugRedactsCopyAndKeepsJSON(t *testing.T) {
	isolateSecrets(t)
	registerSecrets(`synthetic"debug-secret`)
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
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ensureDebugRequestContext(req, "trace-keep", time.Now())
	fields := map[string]any{
		"status_code": 403, "error_code": 12153,
		"reason": `permission denied; details=synthetic"debug-secret`,
		"nested": map[string]any{"access_token": "unregistered-secret"},
	}
	debugEvent(req, "warn", "upstream_response_rejected", fields)
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "unregistered-secret") || strings.Contains(output.String(), "debug-secret") {
		t.Fatalf("debug secret leak: %s", output.String())
	}
	if record["trace_id"] != "trace-keep" || record["error_code"] != float64(12153) || !strings.Contains(record["reason"].(string), "permission denied") {
		t.Fatalf("diagnostics lost: %#v", record)
	}
	if fields["nested"].(map[string]any)["access_token"] != "unregistered-secret" || !strings.Contains(fields["reason"].(string), "debug-secret") {
		t.Fatal("redaction modified business fields")
	}
}

func TestMinimalRedactionConcurrentUse(t *testing.T) {
	isolateSecrets(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			registerSecrets("synthetic-concurrent-secret")
			got := redactSensitiveText("traceId=concurrent code=403 key=synthetic-concurrent-secret")
			if strings.Contains(got, "synthetic-concurrent-secret") {
				t.Errorf("secret leaked: %q", got)
			}
		})
	}
	wg.Wait()
}

func TestMinimalFileAuditStillCoversRejectedAndSuccessfulRequests(t *testing.T) {
	isolateSecrets(t)
	chdirTemp(t)
	oldWriter, oldKey := log.Writer(), cfg.APIKey
	t.Cleanup(func() { log.SetOutput(oldWriter); cfg.APIKey = oldKey })
	cfg.APIKey = "synthetic-file-audit-key"
	closeLog := initFileLogging("minimal-fix-test")
	defer closeLog()
	handler := requestAuditMiddleware(corsMiddleware(authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[业务入口] traceId=%s 原因=synthetic business success", debugTraceID(r))
		log.Printf("[外部接口] traceId=%s HTTP=403 code=12153 原因=permission denied Authorization: Bearer unknown-audit-token", debugTraceID(r))
		w.WriteHeader(http.StatusNoContent)
	}))))
	for _, allowed := range []bool{false, true} {
		trace := "rejected-trace"
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		if allowed {
			trace = "accepted-trace"
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
		req.Header.Set("X-Trace-ID", trace)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Header().Get("X-Trace-ID") != trace {
			t.Fatal("response lost trace ID")
		}
	}
	path := filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, stage := range []string{
		"[请求到达] traceId=rejected-trace", "[请求被拦截] traceId=rejected-trace", "[响应返回] traceId=rejected-trace",
		"[请求到达] traceId=accepted-trace", "[中间件-鉴权] traceId=accepted-trace", "[业务入口] traceId=accepted-trace",
		"[外部接口] traceId=accepted-trace HTTP=403 code=12153 原因=permission denied", "[响应返回] traceId=accepted-trace",
	} {
		if !strings.Contains(text, stage) {
			t.Fatalf("missing audit stage %q: %s", stage, text)
		}
	}
	if strings.Contains(text, cfg.APIKey) || strings.Contains(text, "unknown-audit-token") {
		t.Fatal("file audit leaked secrets")
	}
}
