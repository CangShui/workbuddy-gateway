package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// 持续有数据的流，即使总时长超过空闲阈值，也不得被中断（旧实现会被 180s 总超时误杀）。
func TestIdleReadCloserKeepsActiveStream(t *testing.T) {
	old := upstreamIdleTimeout
	upstreamIdleTimeout = 200 * time.Millisecond
	defer func() { upstreamIdleTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		// 总时长约 1s，远超 200ms 的空闲阈值，但每次间隔 50ms 持续有数据
		for i := 0; i < 20; i++ {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			f.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = newIdleReadCloser(resp.Body, upstreamIdleTimeout, cancel)
	defer resp.Body.Close()

	start := time.Now()
	total := 0
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("active stream must not be interrupted, got %v after %v", err, time.Since(start))
		}
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("stream finished too early, watchdog likely cut it: %v", time.Since(start))
	}
	if total == 0 {
		t.Fatal("no data read")
	}
}

// 上游卡死（超过空闲阈值无任何数据）时必须中断，而不是永久挂起。
func TestIdleReadCloserInterruptsStalledStream(t *testing.T) {
	old := upstreamIdleTimeout
	upstreamIdleTimeout = 200 * time.Millisecond
	defer func() { upstreamIdleTimeout = old }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		f.Flush()
		<-release // 卡住不再发数据
	}))
	defer func() { close(release); srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = newIdleReadCloser(resp.Body, upstreamIdleTimeout, cancel)
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	// 先读到第一块
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read should succeed: %v", err)
	}
	start := time.Now()
	// 后续读取必须在空闲阈值附近返回错误，而不是一直阻塞
	for {
		_, err := resp.Body.Read(buf)
		if err != nil {
			elapsed := time.Since(start)
			if elapsed > 2*time.Second {
				t.Fatalf("stalled stream took too long to interrupt: %v", elapsed)
			}
			return
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("stalled stream was never interrupted")
		}
	}
}

// 上游对 3MB+ 大请求可能长时间才回响应头（实测 2.3MB 曾需 50.8s）。
// 这类请求不能被误杀成 502「timeout awaiting response headers」。
func TestLargeSlowUpstreamHeaderIsNotKilled(t *testing.T) {
	oldHeader := upstreamHeaderTimeout
	oldIdle := upstreamIdleTimeout
	upstreamHeaderTimeout = 3 * time.Second
	upstreamIdleTimeout = 3 * time.Second
	defer func() {
		upstreamHeaderTimeout = oldHeader
		upstreamIdleTimeout = oldIdle
	}()

	// 上游先思考 1.2s 才回响应头，然后正常流式输出
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		f.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Base, profileCN.Origin = upstream.URL, upstream.URL
	cfg.HttpClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: upstreamHeaderTimeout}}
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "slow-header.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
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

	if rec.Code != http.StatusOK {
		t.Fatalf("缓慢但正常的响应头不应被误杀, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("正常流应完整结束:\n%s", rec.Body.String())
	}
}

// config.json 的 upstream 段应能覆盖内置超时默认值。
func TestRuntimeConfigOverridesUpstreamTimeouts(t *testing.T) {
	oldHeader := upstreamHeaderTimeout
	oldIdle := upstreamIdleTimeout
	defer func() {
		upstreamHeaderTimeout = oldHeader
		upstreamIdleTimeout = oldIdle
	}()

	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"upstream":{"headerTimeoutSeconds":420,"idleTimeoutSeconds":75}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamHeaderTimeout != 420*time.Second {
		t.Fatalf("header timeout override failed: %v", upstreamHeaderTimeout)
	}
	if upstreamIdleTimeout != 75*time.Second {
		t.Fatalf("idle timeout override failed: %v", upstreamIdleTimeout)
	}

	// 省略 upstream 段时应回到默认值
	if err := os.WriteFile(path, []byte(`{"debug":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(path); err != nil {
		t.Fatal(err)
	}
	if upstreamHeaderTimeout != upstreamHeaderTimeoutDefault || upstreamIdleTimeout != upstreamIdleTimeoutDefault {
		t.Fatalf("omitted upstream section must restore defaults: header=%v idle=%v",
			upstreamHeaderTimeout, upstreamIdleTimeout)
	}
}
func TestInterruptedChatStreamDoesNotSendDone(t *testing.T) {
	oldTimeout := upstreamIdleTimeout
	upstreamIdleTimeout = 300 * time.Millisecond
	defer func() { upstreamIdleTimeout = oldTimeout }()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":""}]}`+"\n\n")
		f.Flush()
		<-release
	}))
	defer func() { close(release); upstream.Close() }()

	oldBase := profileCN.Base
	oldClient := cfg.HttpClient
	profileCN.Base = upstream.URL
	cfg.HttpClient = &http.Client{}
	defer func() {
		profileCN.Base = oldBase
		cfg.HttpClient = oldClient
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "idle-test.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
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

	out := rec.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("interrupted stream must not send [DONE]:\n%s", out)
	}
	if !strings.Contains(out, "upstream_stream_interrupted") {
		t.Fatalf("interrupted stream must report an explicit error event:\n%s", out)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("already-received partial content should be forwarded:\n%s", out)
	}
}

// useSingleUpstreamAccount 把国内站指到测试上游，并只留一个账号。
func useSingleUpstreamAccount(t *testing.T, upstreamURL string) {
	t.Helper()
	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Base, profileCN.Origin = upstreamURL, upstreamURL
	cfg.HttpClient = &http.Client{}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "eof-rule.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}
	accountMu.Unlock()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
}

// 干净 EOF，且只有空 finish_reason：不能补 [DONE]，必须明确告诉客户端这次不完整。
func TestChatCleanEOFWithoutFinishReasonIsNotSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":""}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("没有真实 finish_reason 时不得转发 [DONE]:\n%s", out)
	}
	if !strings.Contains(out, "stream_closed_without_finish") {
		t.Fatalf("必须明确报告流未完整结束:\n%s", out)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("已经收到的内容仍应转发:\n%s", out)
	}
}

// 有真实 finish_reason 时，即使上游没再发 [DONE]，也是正常结束，不能当成中断。
func TestChatFinishReasonWithoutDoneIsSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"length"}]}`+"\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if strings.Contains(out, "stream_closed_without_finish") || strings.Contains(out, "stream_interrupted") {
		t.Fatalf("已有 finish_reason=length，不应判失败:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) || !strings.Contains(out, "done") {
		t.Fatalf("终止分片应原样转发:\n%s", out)
	}
}

// Responses：干净结束且没有 finish_reason 时，只发小的 response.failed，
// 不得再把整段输出收成 output_item.done / response.completed。
func TestResponsesCleanEOFWithoutFinishReasonEmitsFailed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"loop"},"finish_reason":""}]}`+"\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleResponses)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_closed_without_finish") {
		t.Fatalf("应下发 response.failed:\n%s", out)
	}
	if strings.Contains(out, "response.completed") || strings.Contains(out, "response.output_item.done") || strings.Contains(out, "data: [DONE]") {
		t.Fatalf("不完整流不得伪装完成:\n%s", out)
	}
	if !strings.Contains(out, "loop") {
		t.Fatalf("已经发出的增量可以保留，但收尾不能再重放整段:\n%s", out)
	}
}

// 终止分片可以没有 delta。只要 finish_reason 非空，Responses 仍应正常 completed。
func TestResponsesFinishReasonEmitsCompleted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":""}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	useSingleUpstreamAccount(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	requestAuditMiddleware(http.HandlerFunc(handleResponses)).ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "response.completed") || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("有 finish_reason 的流应正常完成:\n%s", out)
	}
	if strings.Contains(out, "stream_closed_without_finish") {
		t.Fatalf("有 finish_reason 时不应判未完成:\n%s", out)
	}
}

func TestAggregateCompletionWithoutFinishReasonIsNotSuccess(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":""}]}`,
		`data: [DONE]`,
	}, "\n")
	_, err := aggregateCompletion(strings.NewReader(sse), "m")
	if !errors.Is(err, errStreamClosedWithoutFinish) {
		t.Fatalf("没有 finish_reason 的聚合应失败，实际: %v", err)
	}
}
