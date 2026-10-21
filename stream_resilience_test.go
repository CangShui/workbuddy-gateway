package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCanReplayStream 覆盖安全重放判定。
func TestCanReplayStream(t *testing.T) {
	rstErr := errors.New(`read tcp 192.168.100.71:59360->112.49.40.78:443: read: connection reset by peer`)
	base := &sseTestBody{}
	_ = base
	cases := []struct {
		name        string
		err         error
		sawData     bool
		budgetLeft  bool
		wantReplay  bool
	}{
		{"RST且未写任何数据且预算够 → 重放", rstErr, false, true, true},
		{"RST且已写过数据 → 不重放（防重复输出）", rstErr, true, true, false},
		{"RST且预算用尽 → 不重放", rstErr, false, false, false},
		{"客户端取消 → 不重放", context.Canceled, false, true, false},
		{"无错误 → 不重放", nil, false, true, false},
		{"EOF且未写数据 → 重放", io.EOF, false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canReplayStream(c.err, c.sawData, c.budgetLeft); got != c.wantReplay {
				t.Fatalf("canReplayStream(%v,%v,%v)=%v want %v", c.err, c.sawData, c.budgetLeft, got, c.wantReplay)
			}
		})
	}
}

type sseTestBody struct{}

// TestStreamHeartbeatWritesCommentBeforeData 验证：
// 在首个 data 行写出前，心跳间隔会向下游写 SSE 注释行；首个 data 行后停止。
func TestStreamHeartbeatWritesCommentBeforeData(t *testing.T) {
	oldKeepalive := streamKeepalive
	streamKeepalive = 15 * time.Millisecond
	defer func() { streamKeepalive = oldKeepalive }()

	rec := httptest.NewRecorder()
	flusher := &syncFlusher{ResponseRecorder: rec}
	guard := &streamWriteGuard{w: flusher, flusher: flusher}
	stop := startStreamHeartbeat(guard, streamKeepalive)
	defer stop()

	// 先等一个心跳周期：此时还没写 data，应出现注释行
	time.Sleep(40 * time.Millisecond)
	if !strings.Contains(rec.Body.String(), ": keepalive") {
		t.Fatalf("首字节前应下发心跳注释，实际 body=%q", rec.Body.String())
	}

	// 写首个 data 行后，再等两个心跳周期：不应再新增注释行
	guard.writeData(`{"choices":[]}`)
	nBefore := strings.Count(rec.Body.String(), ": keepalive")
	time.Sleep(45 * time.Millisecond)
	nAfter := strings.Count(rec.Body.String(), ": keepalive")
	if nAfter != nBefore {
		t.Fatalf("首个 data 后不应再写心跳注释：before=%d after=%d body=%q", nBefore, nAfter, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data: {"choices":[]}`) {
		t.Fatalf("data 行应透传，body=%q", rec.Body.String())
	}
}

// syncFlusher 组合 ResponseRecorder 与 Flusher（ResponseRecorder 已实现 Flusher）。
type syncFlusher struct {
	*httptest.ResponseRecorder
}

// TestStreamChatResponseResilientPassthrough 验证韧性版在正常流下与原版行为一致。
func TestStreamChatResponseResilientPassthrough(t *testing.T) {
	oldKeepalive := streamKeepalive
	streamKeepalive = 0 // 关闭心跳，聚焦透传
	defer func() { streamKeepalive = oldKeepalive }()

	pipeR, pipeW := io.Pipe()
	go func() {
		defer pipeW.Close()
		fmt.Fprint(pipeW, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		fmt.Fprint(pipeW, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(pipeW, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\ndata: [DONE]\n\n")
	}()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	start := time.Now()

	streamChatResponseResilient(rec, r, &http.Response{Body: pipeR}, "metric-model", 1,
		&Account{Path: "synthetic.json"}, &profileCN, start, []byte(`{}`))

	body := rec.Body.String()
	if !strings.Contains(body, `"content":"ok"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("正常流应透传完整内容，body=%q", body)
	}
	if strings.Contains(body, `stream_interrupted`) {
		t.Fatalf("正常流不应出现中断错误，body=%q", body)
	}
}

// TestStreamHeartbeatDisabled 验证心跳关闭时不下发注释行。
func TestStreamHeartbeatDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &syncFlusher{ResponseRecorder: rec}
	guard := &streamWriteGuard{w: flusher, flusher: flusher}
	stop := startStreamHeartbeat(guard, 0)
	defer stop()
	time.Sleep(10 * time.Millisecond)
	if strings.Contains(rec.Body.String(), "keepalive") {
		t.Fatalf("心跳关闭时不应有注释行，body=%q", rec.Body.String())
	}
}

var _ = sync.Mutex{}
