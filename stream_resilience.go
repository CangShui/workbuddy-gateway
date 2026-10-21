package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// streamWriteGuard 串行化下游 ResponseWriter 的心跳和数据写入。
// net/http 不保证 ResponseWriter 并发安全，心跳 goroutine 与主流读取
// 必须共用同一把锁。
type streamWriteGuard struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	flusher  http.Flusher
	dataSeen bool
}

func (g *streamWriteGuard) writeData(payload string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, _ = fmt.Fprintf(g.w, "data: %s\n\n", payload)
	g.flusher.Flush()
	g.dataSeen = true
}

// writeUpstreamComment 转发上游已有的 SSE 注释，不把注释算作有效数据。
func (g *streamWriteGuard) writeUpstreamComment(line string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, _ = fmt.Fprintf(g.w, "%s\n\n", line)
	g.flusher.Flush()
}

func (g *streamWriteGuard) writeHeartbeat() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dataSeen {
		return
	}
	_, _ = fmt.Fprint(g.w, ": keepalive\n\n")
	g.flusher.Flush()
}

func (g *streamWriteGuard) hasData() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dataSeen
}

type streamHeartbeat struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// startStreamHeartbeat 在首个有效 data 前周期性下发 SSE 注释；Stop 会等待
// goroutine 退出，避免 handler 返回后仍写 ResponseWriter。
func startStreamHeartbeat(g *streamWriteGuard, interval time.Duration) *streamHeartbeat {
	if interval <= 0 {
		return nil
	}
	h := &streamHeartbeat{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-ticker.C:
				g.writeHeartbeat()
			}
		}
	}()
	return h
}

func (h *streamHeartbeat) Stop() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.stop) })
	<-h.done
}

// canReplayStream 只允许在客户端没有收到任何有效数据、预算未用尽、
// 且错误属于瞬时网络故障或上游空闲超时时重放。
// clientCanceled 由调用方根据 r.Context().Err() 判断；内部 idle reader 超时
// 也会表现为 context.Canceled，因此必须与客户端取消区分。
func canReplayStream(scanErr error, sawFirstData, budgetLeft, clientCanceled bool) bool {
	if scanErr == nil || sawFirstData || !budgetLeft || clientCanceled {
		return false
	}
	if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
		return true
	}
	return isTransientNetworkError(scanErr)
}

// replayResponseWriter 捕获重放时 upstreamChat 的错误响应，避免心跳已经
// 把 200 响应头发出后再次 WriteHeader，或把 JSON 错误混进 SSE 流。
type replayResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newReplayResponseWriter(source http.Header) *replayResponseWriter {
	return &replayResponseWriter{header: source.Clone()}
}

func (w *replayResponseWriter) Header() http.Header { return w.header }

func (w *replayResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *replayResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func replayUpstreamChat(w http.ResponseWriter, r *http.Request, reqID uint64, modelName string, upstreamBytes []byte, startTime time.Time, guard *streamWriteGuard) (*http.Response, *Account, *upstreamProfile, bool) {
	captured := newReplayResponseWriter(w.Header())
	resp, acc, prof, ok := upstreamChat(captured, r, reqID, modelName, upstreamBytes, startTime)
	if ok {
		return resp, acc, prof, true
	}
	payload := bytes.TrimSpace(captured.body.Bytes())
	if len(payload) == 0 || !json.Valid(payload) {
		status := captured.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		payload, _ = json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "首字节前重放失败，本次响应不完整",
				"type":    "upstream_stream_interrupted",
				"code":    "stream_replay_failed",
				"status":  status,
			},
		})
	}
	guard.writeData(string(payload))
	log.Printf("[异常] traceId=%s requestId=%d 发生阶段=首字节前重放 模型=%s 结果=重放未取得可用上游响应 业务影响=已通过SSE错误事件通知客户端，不伪造[DONE]",
		debugTraceID(r), reqID, modelName)
	debugEvent(r, "error", "stream_first_byte_replay_failed", map[string]any{
		"status_code":     http.StatusBadGateway,
		"business_impact": "客户端已开始接收SSE，无法再改写HTTP状态，改为下发流内错误",
	})
	return nil, nil, nil, false
}
