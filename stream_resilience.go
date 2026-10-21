package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 流式响应韧性加固
//
// 背景（本机实测，2026-10-20~21）：腾讯 copilot.tencent.com 边缘节点对
// 「响应头已发出、但长时间没有首个有效字节」的 SSE 连接有很低的静默容忍
// （6~132s 内的静默期即触发 connection reset by peer，RST 集中在部分边缘 IP）。
// 后果是 workbuddy 网关向上游请求 deepseek-v4.1-flash 时，流式响应在
// 「首个 token 之前」就被杀连接——实测 100% 的中断请求都发生在首字节前，
// 且请求体越大（≥2MB 上下文）中断率越高（2.44% vs <0.5MB 的 0.52%）。
//
// 本文件做两件事：
//  1. 流式心跳（StreamKeepaliveSeconds / streamKeepalive）：
//     在「已收到上游响应头、却迟迟没有首个有效数据」的空窗期，周期性向下游
//     下发 SSE 注释行 `: keepalive`（OpenAI 兼容客户端会忽略注释行），
//     保持下游连接活跃，避免客户端因长时间收不到任何字节而主动取消本请求。
//     首个 data 事件写出后自动停止，不污染流解析。
//  2. 首字节前安全重放（EnableFirstByteRetry / enableFirstByteRetry）：
//     若上游在「任何有效字节都未下发」之前就 RST/EOF，由于客户端还没收到
//     任何内容，重放整条请求不会产生重复输出——仅重复消耗一次上游额度；
//     换取边缘节点静默期误杀时请求仍能成功。默认开启，最多重放 1 次。
// -----------------------------------------------------------------------------

// streamWriteGuard 串行化对下游 http.ResponseWriter 的所有写（数据行 + 心跳行），
// 避免 Go http.ResponseWriter 被并发写产生竞态。每个流式请求独占一把锁。
type streamWriteGuard struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	flusher  http.Flusher
	dataSeen bool
}

func (g *streamWriteGuard) writeData(s string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, _ = fmt.Fprintf(g.w, "data: %s\n\n", s)
	g.flusher.Flush()
	g.dataSeen = true
}

func (g *streamWriteGuard) writeComment() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dataSeen {
		return // 已有真实数据，无需再心跳
	}
	_, _ = fmt.Fprint(g.w, ": keepalive\n\n")
	g.flusher.Flush()
}

// startStreamHeartbeat 启动流式心跳：在首个 data 行写出前，每 interval
// 向下游写一条 SSE 注释行。返回 stop 函数。
func startStreamHeartbeat(g *streamWriteGuard, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				g.writeComment()
			}
		}
	}()
	return func() { close(stop) }
}

// canReplayStream 判断流是否需要在「首个有效字节前」安全重放。
//   - scanErr: 上游读错误（nil/非 nil）
//   - sawFirstData: 是否已向客户端写过有效 data 行
//   - budgetLeft: 重放预算是否还有剩余
func canReplayStream(scanErr error, sawFirstData bool, budgetLeft bool) bool {
	if scanErr == nil || sawFirstData {
		return false
	}
	if !budgetLeft {
		return false
	}
	return !errors.Is(scanErr, context.Canceled)
}

// streamChatResponseResilient 是带心跳 + 首字节前重放的流式透传版本。
// 与 streamChatResponse 的语义兼容，但额外：
//   - 在首个有效字节下发前周期性向下游写 SSE 心跳注释；
//   - 若上游在首个有效字节前发生瞬时网络错误，重放整条请求
//     （重建连接 + 重新走账号池轮换，最多 1 次）。
func streamChatResponseResilient(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time, upstreamBytes []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	guard := &streamWriteGuard{w: w, flusher: flusher}
	stopHB := startStreamHeartbeat(guard, currentStreamKeepalive())
	defer stopHB()

	replayBudget := 1
	if !currentFirstByteRetry() {
		replayBudget = 0
	}
	retries := 0

	for attempt := 0; ; attempt++ {
		body := newTTFTReader(resp.Body, startTime)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var usage map[string]any
		finishReason := ""
		sawDone := false
		sawFirstData := false

		for scanner.Scan() {
			cleanData := stripDataPrefix(scanner.Text())
			if cleanData == "" {
				continue
			}
			if cleanData == "[DONE]" {
				sawDone = true
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(cleanData), &chunk) == nil {
				observeFirstToken(body, chunk)
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				finishReason = noteFinishReason(finishReason, chunk)
			}
			if cleanedChunk := cleanChunkJSON(cleanData); cleanedChunk != "" {
				guard.writeData(cleanedChunk)
				sawFirstData = true
			}
		}
		scanErr := scanner.Err()
		completeTokenStream(body, scanErr == nil && finishReason != "")

		// 安全重放条件：
		//  1) 发生了读错误（上游 RST/EOF/超时）
		//  2) 尚未向客户端下发任何有效内容
		//  3) 重放开关开启且有预算
		//  4) 不是客户端主动取消（客户端取消时重放无意义且 r.Context 已终）
		canReplay := canReplayStream(scanErr, sawFirstData, retries < replayBudget)

		if canReplay {
			retries++
			log.Printf("[首字节前重放] traceId=%s requestId=%d 模型=%s 账号=%s 原因=首个有效字节前上游连接中断（%v），客户端尚未收到任何内容，安全重放 第%d次",
				debugTraceID(r), reqID, modelName, acc.Path, safeDebugError(scanErr), retries)
			debugEvent(r, "warn", "stream_first_byte_replay", map[string]any{
				"reason":   safeDebugError(scanErr),
				"attempt":  attempt + 1,
				"business_impact": "客户端未收到任何有效内容，重放可安全规避边缘节点静默期误杀",
			})
			resp.Body.Close()
			newResp, newAcc, newProf, ok := upstreamChat(w, r, reqID, modelName, upstreamBytes, startTime)
			if !ok {
				return // upstreamChat 已向客户端写入错误响应
			}
			resp, acc, prof = newResp, newAcc, newProf
			continue // 下次迭代重新扫新 resp 的流
		}

		finishFirstToken(r, modelName, reqID, body)

		if scanErr != nil {
			debugEvent(r, "error", "stream_response_failed", map[string]any{
				"error_type": debugErrorType(scanErr),
				"error":      safeDebugError(scanErr),
			})
			reason := "上游流式响应中断，本次回复不完整"
			if errors.Is(scanErr, context.DeadlineExceeded) {
				reason = fmt.Sprintf("上游超过 %v 无数据，判定连接卡死并中断，本次回复不完整", currentIdleTimeout())
			}
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": reason,
					"type":    "upstream_stream_interrupted",
					"code":    "stream_interrupted",
				},
			})
			guard.writeData(string(errPayload))
			log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游流式读取 账号=%s 异常=%v 业务影响=本次响应不完整，已向客户端下发中断错误而非伪造结束", debugTraceID(r), reqID, acc.Path, scanErr)
		} else if finishReason == "" {
			debugEvent(r, "error", "stream_closed_without_finish", map[string]any{
				"error_type":      "stream_closed_without_finish",
				"finish_reason":   "",
				"saw_done":        sawDone,
				"business_impact": "上游干净结束但没有 finish_reason，已拒绝当成成功，未补发 [DONE]",
			})
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": errStreamClosedWithoutFinish.Error(),
					"type":    "upstream_stream_incomplete",
					"code":    "stream_closed_without_finish",
				},
			})
			guard.writeData(string(errPayload))
			log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游流结束 账号=%s 结果=拒绝当成成功 原因=连接正常关闭但没有非空 finish_reason 是否看到DONE=%t 业务影响=不补发 [DONE]，客户端收到 stream_closed_without_finish 是否已处理=是",
				debugTraceID(r), reqID, acc.Path, sawDone)
		} else {
			if sawDone {
				guard.writeData("[DONE]")
			}
			debugEvent(r, "info", "stream_response_completed", map[string]any{
				"status_code":   http.StatusOK,
				"finish_reason": finishReason,
				"saw_done":      sawDone,
			})
		}

		observeModelCredit(acc, modelName, usage, reqID)
		statsObserveUsage(r, usage, scanErr == nil && finishReason != "")
		recordModelTokens(modelName, usage, reqID)
		recordModelLatency(modelName, time.Since(startTime))
		if scanErr == nil && finishReason != "" {
			log.Printf("[#%d] 流式输出完成 (账号 %s [%s], 结束原因=%s, 耗时 %v, 首Token(v2) %s)", reqID, acc.Path, prof.Label, finishReason, time.Since(startTime), body.text())
		}
		return
	}
}