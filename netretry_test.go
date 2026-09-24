package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 瞬时网络错误必须被识别（可重试），非瞬时错误不得误判。
func TestIsTransientNetworkError(t *testing.T) {
	transient := []error{
		io.EOF,
		errors.New(`Post "https://x": write tcp 1.2.3.4:1->5.6.7.8:443: write: connection reset by peer`),
		errors.New("write: broken pipe"),
		errors.New("use of closed network connection"),
		errors.New("dial tcp 1.2.3.4:443: connect: connection refused"),
		errors.New(`http2: server sent GOAWAY and closed the connection`),
		errors.New("unexpected EOF"),
	}
	for _, err := range transient {
		if !isTransientNetworkError(err) {
			t.Errorf("应判定为瞬时错误: %v", err)
		}
	}

	permanent := []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		errors.New("invalid character 'x' looking for beginning of value"),
		errors.New("json: cannot unmarshal number"),
	}
	for _, err := range permanent {
		if isTransientNetworkError(err) {
			t.Errorf("不应判定为瞬时错误: %v", err)
		}
	}
}

type retryTransportFunc func(*http.Request) (*http.Response, error)

func (f retryTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 连接建立阶段失败，请求头未写出，才可用新连接重试。
func TestUpstreamRetriesTransientFailureAndSucceeds(t *testing.T) {
	var attempts int32
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	oldBackoff := upstreamRetryBackoff
	defer func() {
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
		upstreamRetryBackoff = oldBackoff
	}()
	upstreamTransientRetries = 2
	upstreamRetryBackoff = time.Millisecond
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			return nil, io.EOF // 建连失败，请求头与请求体都未发送
		}
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != `{"model":"m"}` {
			t.Errorf("request body on attempt %d: %q, %v", n, body, err)
		}
		if !req.Close {
			t.Error("retry must request a new connection")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	resp, cancel, err := doUpstreamRequest(req, acc, &profileCN, []byte(`{"model":"m"}`), 1, "trace-test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected one safe retry, got %d attempts", got)
	}
}

// 上游已读到请求头后断开，可能已经处理了 POST，不能重放。
func TestUpstreamDoesNotRetryEOFOnceHeadersSent(t *testing.T) {
	var attempts int32
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	defer func() { cfg.HttpClient, upstreamTransientRetries = oldClient, oldRetries }()
	upstreamTransientRetries = 2
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
			trace.WroteHeaders()
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return nil, io.EOF
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	_, _, err := doUpstreamRequest(req, acc, &profileCN, []byte(`{"model":"m"}`), 1, "trace-test")
	if !errors.Is(err, io.EOF) || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("already sent POST must not retry: attempts=%d err=%v", attempts, err)
	}
}

// 即使只返回裸 EOF，若请求体已写完而服务端尚未发响应，也不能重复生成。
func TestUpstreamDoesNotRetryEOFOnceBodySent(t *testing.T) {
	var attempts int32
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	defer func() { cfg.HttpClient, upstreamTransientRetries = oldClient, oldRetries }()
	upstreamTransientRetries = 2
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return nil, io.EOF
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	_, _, err := doUpstreamRequest(req, acc, &profileCN, []byte(`{"model":"m"}`), 1, "trace-test")
	if !errors.Is(err, io.EOF) || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("fully sent POST must not retry: attempts=%d err=%v", attempts, err)
	}
}

// 真 TCP RST 发生在服务端收到请求之后时，同样不得自动重放。
func TestUpstreamDoesNotRetryAfterServerReceivesRequest(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			// 模拟对端 RST：劫持底层连接后直接关闭
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0) // 立即 RST
					}
					_ = conn.Close()
					return
				}
			}
			panic("hijack unsupported")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	oldBackoff := upstreamRetryBackoff
	profileCN.Base, profileCN.Origin = srv.URL, srv.URL
	cfg.HttpClient = &http.Client{}
	upstreamTransientRetries = 2
	upstreamRetryBackoff = 10 * time.Millisecond
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
		upstreamRetryBackoff = oldBackoff
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "retry.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
	}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	if got := atomic.LoadInt32(&attempts); got != 1 || rec.Code != http.StatusBadGateway {
		t.Fatalf("request reached server, must not retry: attempts=%d status=%d", got, rec.Code)
	}
}

// 真 HTTP 服务器收到完整 POST 后直接关连接时，客户端看到 EOF 仍不能重放。
func TestUpstreamDoesNotRetryRealEOF(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read request body: %v", err)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	oldClient, oldRetries := cfg.HttpClient, upstreamTransientRetries
	defer func() { cfg.HttpClient, upstreamTransientRetries = oldClient, oldRetries }()
	cfg.HttpClient = srv.Client()
	upstreamTransientRetries = 2
	prof := profileCN
	prof.Base = srv.URL
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "retry.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	_, _, err := doUpstreamRequest(req, acc, &prof, []byte(`{"model":"m"}`), 1, "trace-test")
	if !errors.Is(err, io.EOF) || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("upstream received POST, must not retry: attempts=%d err=%v", attempts, err)
	}
}

// 重试次数用尽仍失败时，应如实返回 502，且不得伪造成功。
func TestUpstreamRetriesExhaustedReturns502(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				_ = conn.Close()
				return
			}
		}
		panic("hijack unsupported")
	}))
	defer srv.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	oldBackoff := upstreamRetryBackoff
	profileCN.Base, profileCN.Origin = srv.URL, srv.URL
	cfg.HttpClient = &http.Client{}
	upstreamTransientRetries = 2
	upstreamRetryBackoff = 10 * time.Millisecond
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
		upstreamRetryBackoff = oldBackoff
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "fail.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
	}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("重试用尽应返回 502, got %d", rec.Code)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("RST after request headers must not retry, attempts=%d", got)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatal("失败时不得伪造 [DONE]")
	}
}

// 非瞬时错误（如上游返回 400）不得触发重试，避免放大问题。
func TestUpstreamDoesNotRetryPermanentError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":11128,"msg":"bad"}`)
	}))
	defer srv.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	oldRetries := upstreamTransientRetries
	profileCN.Base, profileCN.Origin = srv.URL, srv.URL
	cfg.HttpClient = &http.Client{}
	upstreamTransientRetries = 2
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		upstreamTransientRetries = oldRetries
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "p.json",
		Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
	}}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("HTTP 400 不应重试，实际请求次数=%d", got)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应透传上游 400, got %d", rec.Code)
	}
}

// config.json 应能覆盖重试次数。
func TestRuntimeConfigOverridesTransientRetries(t *testing.T) {
	old := upstreamTransientRetries
	defer func() { upstreamTransientRetries = old }()

	dir := t.TempDir()
	path := dir + "/config.json"
	if err := writeFile(path, `{"upstream":{"transientRetries":5}}`); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamTransientRetries != 5 {
		t.Fatalf("重试次数覆盖失败: %d", upstreamTransientRetries)
	}

	// 显式设为 0 应禁用重试（而不是回退默认值）
	if err := writeFile(path, `{"upstream":{"transientRetries":0}}`); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamTransientRetries != 0 {
		t.Fatalf("显式 0 应禁用重试, got %d", upstreamTransientRetries)
	}

	// 省略该字段应回到默认值
	if err := writeFile(path, `{"debug":{"enabled":false}}`); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamTransientRetries != upstreamTransientRetriesDefault {
		t.Fatalf("省略配置应回默认值: %d", upstreamTransientRetries)
	}
}

// 辅助：写文件
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}
