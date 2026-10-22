package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCanReplayStream(t *testing.T) {
	rstErr := errors.New(`read tcp 192.0.2.10:59360->198.51.100.20:443: read: connection reset by peer`)
	// Windows 上同一次 RST 的地址与端口完全相同，仅文案不同；必须同样允许重放。
	windowsRstErr := errors.New(`read tcp 192.0.2.10:59360->198.51.100.20:443: wsarecv: An existing connection was forcibly closed by the remote host.`)
	cases := []struct {
		name           string
		err            error
		sawData        bool
		budgetLeft     bool
		clientCanceled bool
		want           bool
	}{
		{"RST before data", rstErr, false, true, false, true},
		{"Windows wsasend RST before data", windowsRstErr, false, true, false, true},
		{"Windows wsasend RST after data", windowsRstErr, true, true, false, false},
		{"already sent data", rstErr, true, true, false, false},
		{"budget exhausted", rstErr, false, false, false, false},
		{"client canceled", context.Canceled, false, true, true, false},
		{"upstream idle canceled", context.Canceled, false, true, false, true},
		{"deadline", context.DeadlineExceeded, false, true, false, true},
		{"non-network scanner error", bufio.ErrTooLong, false, true, false, false},
		{"no error", nil, false, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canReplayStream(tc.err, tc.sawData, tc.budgetLeft, tc.clientCanceled); got != tc.want {
				t.Fatalf("canReplayStream()=%t want %t", got, tc.want)
			}
		})
	}
}

type lockedResponseWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushes int
}

func newLockedResponseWriter() *lockedResponseWriter {
	return &lockedResponseWriter{header: make(http.Header)}
}

func (w *lockedResponseWriter) Header() http.Header { return w.header }

func (w *lockedResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *lockedResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *lockedResponseWriter) Flush() {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
}

func (w *lockedResponseWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func waitForTestCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached before timeout")
}

func TestStreamHeartbeatWritesCommentBeforeDataAndStops(t *testing.T) {
	writer := newLockedResponseWriter()
	guard := &streamWriteGuard{w: writer, flusher: writer}
	heartbeat := startStreamHeartbeat(guard, 5*time.Millisecond)
	waitForTestCondition(t, time.Second, func() bool {
		return strings.Contains(writer.String(), ": keepalive")
	})
	guard.writeData(`{"choices":[]}`)
	count := strings.Count(writer.String(), ": keepalive")
	time.Sleep(20 * time.Millisecond)
	if got := strings.Count(writer.String(), ": keepalive"); got != count {
		t.Fatalf("heartbeat continued after data: before=%d after=%d", count, got)
	}
	heartbeat.Stop()
	if !strings.Contains(writer.String(), "data: {\"choices\":[]}") {
		t.Fatalf("data frame missing: %q", writer.String())
	}
}

func TestStreamHeartbeatDisabled(t *testing.T) {
	writer := newLockedResponseWriter()
	guard := &streamWriteGuard{w: writer, flusher: writer}
	heartbeat := startStreamHeartbeat(guard, 0)
	heartbeat.Stop()
	time.Sleep(10 * time.Millisecond)
	if strings.Contains(writer.String(), "keepalive") {
		t.Fatalf("disabled heartbeat wrote data: %q", writer.String())
	}
}

func TestRuntimeStreamResilienceDefaultsAndHotReload(t *testing.T) {
	isolatePolicies(t)
	disabled := false
	c := runtimeFileConfig{}
	c.Upstream.StreamKeepaliveSeconds = 7
	c.Upstream.EnableFirstByteRetry = &disabled
	if err := applyRuntimeConfig(c, true, "stream-config-test"); err != nil {
		t.Fatal(err)
	}
	if got := currentStreamKeepalive(); got != 7*time.Second {
		t.Fatalf("keepalive=%v want 7s", got)
	}
	if currentFirstByteRetry() {
		t.Fatal("explicit false did not disable first-byte replay")
	}
	c = runtimeFileConfig{}
	if err := applyRuntimeConfig(c, true, "stream-config-defaults"); err != nil {
		t.Fatal(err)
	}
	if got := currentStreamKeepalive(); got != streamKeepaliveDefault {
		t.Fatalf("default keepalive=%v", got)
	}
	if !currentFirstByteRetry() {
		t.Fatal("omitted first-byte replay did not default to enabled")
	}
}

type readErrorBody struct {
	err    error
	closed atomic.Bool
}

func (b *readErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b *readErrorBody) Close() error {
	b.closed.Store(true)
	return nil
}

type trackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func withStreamTestAccount(t *testing.T) {
	t.Helper()
	isolatePolicies(t)
	resetStatsState(t)
	oldKeepalive := streamKeepalive
	oldReplay := enableFirstByteRetry
	upstreamConfigMu.Lock()
	streamKeepalive = 0
	enableFirstByteRetry = true
	upstreamConfigMu.Unlock()
	t.Cleanup(func() {
		upstreamConfigMu.Lock()
		streamKeepalive = oldKeepalive
		enableFirstByteRetry = oldReplay
		upstreamConfigMu.Unlock()
	})
	oldAccounts := accounts
	accountMu.Lock()
	accounts = []*Account{{
		Path:    "stream-resilience-synthetic.json",
		Edition: "cn",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth: StoredTokens{
				AccessToken: "synthetic-access",
				ExpiresAt:   time.Now().Add(time.Hour).Unix(),
			},
		},
	}}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
}

func streamTestRequest() *http.Request {
	body := `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
}

func TestStreamResilientReplaysOnlyBeforeFirstByte(t *testing.T) {
	withStreamTestAccount(t)
	firstBody := &readErrorBody{err: errors.New("read: connection reset by peer")}
	secondBody := &trackingBody{Reader: strings.NewReader(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")}
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		body := io.ReadCloser(firstBody)
		if call > 1 {
			body = secondBody
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })

	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, streamTestRequest())

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2", got)
	}
	if !firstBody.closed.Load() || !secondBody.closed.Load() {
		t.Fatalf("response bodies not closed: first=%t second=%t", firstBody.closed.Load(), secondBody.closed.Load())
	}
	output := recorder.Body.String()
	if !strings.Contains(output, `"content":"ok"`) || !strings.HasSuffix(output, "data: [DONE]\n\n") {
		t.Fatalf("replayed stream incomplete: %q", output)
	}
	if strings.Contains(output, "stream_interrupted") {
		t.Fatalf("successful replay emitted interruption: %q", output)
	}
}

func TestStreamResilientNeverReplaysAfterData(t *testing.T) {
	withStreamTestAccount(t)
	firstBody := &trackingBody{Reader: &dataThenErrorReader{
		data: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"),
		err:  errors.New("read: connection reset by peer"),
	}}
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: firstBody, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })

	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, streamTestRequest())
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1 after partial data", got)
	}
	output := recorder.Body.String()
	if !strings.Contains(output, "partial") || !strings.Contains(output, `"code":"stream_interrupted"`) || strings.Contains(output, "[DONE]") {
		t.Fatalf("partial stream must not be completed or replayed: %q", output)
	}
}

func TestStreamResilientDoesNotReplayPermanentError(t *testing.T) {
	withStreamTestAccount(t)
	firstBody := &readErrorBody{err: bufio.ErrTooLong}
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: firstBody, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })

	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, streamTestRequest())
	if got := calls.Load(); got != 1 {
		t.Fatalf("permanent error triggered %d upstream calls", got)
	}
	if !strings.Contains(recorder.Body.String(), `"code":"stream_interrupted"`) {
		t.Fatalf("permanent stream error was hidden: %q", recorder.Body.String())
	}
}

type blockingStreamBody struct {
	release chan struct{}
	payload []byte
	done    bool
	closed  atomic.Bool
}

func (b *blockingStreamBody) Read(p []byte) (int, error) {
	if !b.done {
		<-b.release
		b.done = true
		return copy(p, b.payload), nil
	}
	return 0, io.EOF
}

func (b *blockingStreamBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestStreamChatWritesHeartbeatWhileWaitingForFirstByte(t *testing.T) {
	withStreamTestAccount(t)
	upstreamConfigMu.Lock()
	streamKeepalive = 5 * time.Millisecond
	upstreamConfigMu.Unlock()
	body := &blockingStreamBody{
		release: make(chan struct{}),
		payload: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"),
	}
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })

	writer := newLockedResponseWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		newAPIHandler().ServeHTTP(writer, streamTestRequest())
	}()
	waitForTestCondition(t, time.Second, func() bool {
		return strings.Contains(writer.String(), ": keepalive")
	})
	close(body.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish")
	}
	output := writer.String()
	heartbeatAt := strings.Index(output, ": keepalive")
	dataAt := strings.Index(output, `data: {"choices"`)
	if heartbeatAt < 0 || dataAt < 0 || heartbeatAt > dataAt {
		t.Fatalf("heartbeat must precede first data: %q", output)
	}
	if !body.closed.Load() || !strings.HasSuffix(output, "data: [DONE]\n\n") {
		t.Fatalf("stream cleanup or completion missing: closed=%t output=%q", body.closed.Load(), output)
	}
}

func TestReplayFailureBecomesSSEErrorWithoutSecondHeader(t *testing.T) {
	isolatePolicies(t)
	accountMu.Lock()
	oldAccounts := accounts
	accounts = nil
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	writer := httptest.NewRecorder()
	guard := &streamWriteGuard{w: writer, flusher: writer}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, _, _, ok := replayUpstreamChat(writer, r, 1, "deepseek-v4.1-flash", []byte(`{}`), time.Now(), guard)
	if ok {
		t.Fatal("replay unexpectedly succeeded without accounts")
	}
	if writer.Code != http.StatusOK {
		t.Fatalf("SSE error changed HTTP status to %d", writer.Code)
	}
	if !strings.Contains(writer.Body.String(), "data: ") || !strings.Contains(writer.Body.String(), `"type":"no_auth"`) {
		t.Fatalf("replay failure was not emitted as SSE error: %q", writer.Body.String())
	}
}

type dataThenErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, r.err
}
