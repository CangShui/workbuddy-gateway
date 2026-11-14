package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var emptyResponseRecovery atomic.Bool

type emptyRecoveryContextKey struct{}

func snapshotEmptyResponseRecovery(r *http.Request) bool {
	if value, ok := r.Context().Value(emptyRecoveryContextKey{}).(bool); ok {
		return value
	}
	value := emptyResponseRecovery.Load()
	*r = *r.WithContext(context.WithValue(r.Context(), emptyRecoveryContextKey{}, value))
	return value
}

type prefixedResponseBody struct {
	io.Reader
	io.Closer
	firstToken time.Duration
}

func (b *prefixedResponseBody) observedFirstToken() time.Duration { return b.firstToken }

type responseReadError struct{ err error }

func (r responseReadError) Read([]byte) (int, error) { return 0, r.err }

// recoverEmptyResponse 的关闭路径不读响应、不改请求。开启后暂存首段推理，
// 一旦出现正文/工具便立即恢复原流；只有正常 length 空回复才允许额外调用一次。
func recoverEmptyResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, acc *Account, prof *upstreamProfile, raw []byte, model string, reqID uint64, started time.Time, stream bool) *http.Response {
	if !snapshotEmptyResponseRecovery(r) || prof.Key != "cn" || normalizeModelName(model) != "deepseek-v4.1-flash" {
		return resp
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || !thinkingExplicitlyEnabled(body) {
		return resp
	}
	if n, ok := usageNumber(body, "n"); ok && n > 1 {
		return resp
	}
	if n, ok := usageNumber(body, "max_tokens"); ok && n < 16 {
		return resp // 保留官方 max_tokens=1 探活，不额外生成。
	}
	if stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return resp
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-Reasoning-Recovery-Policy", "retry-once-without-thinking")
		heartbeat := startStreamHeartbeat(&streamWriteGuard{w: w, flusher: flusher}, currentStreamKeepalive())
		defer heartbeat.Stop() // 下一阶段写数据前等待心跳退出，不并发写 ResponseWriter。
	}
	reader := bufio.NewReader(resp.Body)
	var prefix bytes.Buffer
	var output completionOutput
	var usage map[string]any
	var firstToken time.Duration
	finish := ""
	for {
		line, readErr := reader.ReadString('\n')
		prefix.WriteString(line)
		data := stripDataPrefix(line)
		lines := []string{line}
		if parts := splitUpstreamSSELine(line); len(parts) > 1 {
			logUpstreamSSESplit(r, len(parts))
			lines = parts
		}
		for _, part := range lines {
			var chunk map[string]any
			payload := stripDataPrefix(part)
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if firstToken == 0 && hasFirstTokenDelta(chunk) {
				firstToken = time.Since(started)
			}
			output.observe(chunk)
			if u, ok := chunk["usage"].(map[string]any); ok {
				usage = u
			}
			finish = noteFinishReason(finish, chunk)
			// 连只有 id/index 的工具片也不重放，避免已声明的调用被执行两次。
			if output.content || hasAnyToolDelta(chunk) || upstreamSSEError(payload) != nil ||
				(finish != "" && finish != "length") {
				resp.Body = &prefixedResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader), Closer: resp.Body, firstToken: firstToken}
				return resp
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			resp.Body = &prefixedResponseBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), responseReadError{readErr}), Closer: resp.Body, firstToken: firstToken}
			return resp
		}
		if data == "[DONE]" || readErr != nil {
			break
		}
	}
	if !output.emptyLimit(finish) || r.Context().Err() != nil {
		resp.Body = &prefixedResponseBody{Reader: bytes.NewReader(prefix.Bytes()), Closer: resp.Body, firstToken: firstToken}
		return resp
	}
	_ = resp.Body.Close() // 先释放首轮并发名额，再从同一账号获取恢复名额。
	logOutputBudget(r, reqID, model, finish, output, usage)
	statsObserveUsage(r, usage, false)
	statsFinishAttempt(r, http.StatusOK)

	recovery := map[string]any{
		"upstream_attempts": 1, "original_effort": body["reasoning_effort"],
		"recovered": false,
	}
	delete(body, "reasoning_effort")
	delete(body, "reasoningEffort")
	delete(body, "reasoning_summary")
	delete(body, "reasoning")
	body["thinking"] = map[string]any{"type": "disabled"}
	recoveryRaw, err := json.Marshal(body)
	if err != nil {
		recovery["error"] = "encode_error"
		return recoveredSSE(resp, emptyBudgetSSE(model), combineRecoveryUsage(usage, nil, recovery), 0, r)
	}
	log.Printf("[空正文恢复] traceId=%s requestId=%d 阶段=开始 模型=%s 账号=%s 原因=推理耗尽预算且正文工具均为空 结果=同账号关思考重发一次 业务影响=可能额外计费；不换模型、不增加预算、不记录推理正文",
		debugTraceID(r), reqID, model, acc.Path)
	debugEvent(r, "warn", "empty_response_recovery_started", map[string]any{
		"max_extra_attempts": 1, "effective_thinking": "disabled",
		"business_impact": "用户已开启实验恢复；同账号同模型额外生成一次，费用单独记录并汇总",
	})
	recoveryStarted := time.Now()
	next, recoveryRequest, sent, sendErr := sendEmptyRecovery(r, acc, prof, recoveryRaw, model, reqID)
	if sent {
		recovery["upstream_attempts"] = 2
		recovery["effective_thinking"] = "disabled"
	}
	var nextUsage map[string]any
	var recoveryFrames []byte
	var recoveredFirstToken time.Duration
	if sendErr != nil {
		recovery["error"] = "recovery_send_failed"
		if sent {
			statsFinishAttempt(recoveryRequest, http.StatusBadGateway)
		}
	} else if next.StatusCode != http.StatusOK {
		recovery["error"] = "recovery_http_error"
		recovery["status"] = next.StatusCode
		errorBody, _ := io.ReadAll(next.Body)
		if isAccountRestrictionPayload(errorBody) {
			recovery["error"] = "account_restricted"
			recoveryFrames = []byte("data: " + string(errorBody) + "\n\n")
		}
		_ = next.Body.Close()
		statsFinishAttempt(recoveryRequest, next.StatusCode)
	} else {
		var captured bytes.Buffer
		if p := statsAttemptFor(recoveryRequest); p != nil && !p.started.IsZero() {
			recoveryStarted = p.started
		}
		tokenReader := newTTFTReader(io.TeeReader(next.Body, &captured), recoveryStarted)
		completion, aggregateErr := aggregateCompletion(tokenReader, model, recoveryRequest)
		_ = next.Body.Close()
		nextFinish := ""
		scanner := newUpstreamSSEScanner(bytes.NewReader(captured.Bytes()), recoveryRequest)
		for scanner.Scan() {
			var chunk map[string]any
			if json.Unmarshal([]byte(stripDataPrefix(scanner.Text())), &chunk) == nil {
				if u, ok := chunk["usage"].(map[string]any); ok {
					nextUsage = u
				}
				nextFinish = noteFinishReason(nextFinish, chunk)
			}
		}
		statsObserveUsage(recoveryRequest, nextUsage, aggregateErr == nil && nextFinish != "length")
		if aggregateErr == nil {
			if p := statsAttemptFor(recoveryRequest); p != nil && tokenReader.duration() > 0 {
				value := durationMilliseconds(tokenReader.duration())
				p.ttft = &value
			}
			result, end := completionOutputFromJSON(completion)
			recovery["recovered"] = end != "length" && (result.content || result.tool)
			if !result.content && !result.tool {
				recovery["error"] = "empty_recovery_output"
			} else {
				recoveryFrames = captured.Bytes()
				if tokenReader.duration() > 0 {
					recoveredFirstToken = recoveryStarted.Sub(started) + tokenReader.duration()
				}
			}
		} else if errors.Is(aggregateErr, errUpstreamAccountRestricted) {
			recovery["error"] = "account_restricted"
			recoveryFrames = captured.Bytes()
		} else if upstreamSSEClientError(aggregateErr) != nil {
			recovery["error"] = "recovery_upstream_error"
			recoveryFrames = captured.Bytes()
		} else {
			recovery["error"] = "recovery_incomplete_stream"
		}
		statsFinishAttempt(recoveryRequest, http.StatusOK)
	}
	if recoveryFrames == nil {
		recoveryFrames = emptyBudgetSSE(model)
	}
	combined := combineRecoveryUsage(usage, nextUsage, recovery)
	log.Printf("[空正文恢复] traceId=%s requestId=%d 阶段=结束 模型=%s 上游尝试=%v 恢复成功=%v 恢复错误=%v 总耗时=%v 结果=停止恢复，不再重试 业务影响=原请求与恢复费用均保留，实际思考模式在usage.reasoning_recovery中返回",
		debugTraceID(r), reqID, model, recovery["upstream_attempts"], recovery["recovered"], recovery["error"], time.Since(started))
	debugEvent(r, "info", "empty_response_recovery_finished", recovery)
	return recoveredSSE(resp, recoveryFrames, combined, recoveredFirstToken, r)
}

func hasAnyToolDelta(chunk map[string]any) bool {
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if calls, _ := delta["tool_calls"].([]any); len(calls) > 0 {
			return true
		}
		if call, ok := delta["function_call"].(map[string]any); ok && !isEmptyFunctionCall(call) {
			return true
		}
	}
	return false
}

// 不经过账号选择器：恢复必须使用原账号，新的模型调用使用新的 message ID。
func sendEmptyRecovery(r *http.Request, acc *Account, prof *upstreamProfile, raw []byte, model string, reqID uint64) (*http.Response, *http.Request, bool, error) {
	nextRequest := r.Clone(r.Context())
	if h := conversationHeadersFor(r); h != nil {
		next := *h
		next.MessageID = strings.ReplaceAll(uuid.NewString(), "-", "")
		nextRequest = nextRequest.WithContext(context.WithValue(nextRequest.Context(), conversationContextKey{}, &next))
	}
	if r.Context().Err() != nil {
		return nil, nextRequest, false, r.Context().Err()
	}
	release, err := acquireModelConcurrency(nextRequest, acc, reqID, debugTraceID(r))
	if err != nil {
		return nil, nextRequest, false, err
	}
	if !tryAccountLockWithContext(r.Context(), acc.lock.TryRLock) {
		release()
		return nil, nextRequest, false, r.Context().Err()
	}
	accountMu.Lock()
	unavailable := acc.Disabled || acc.Auth == nil
	accountMu.Unlock()
	if unavailable || r.Context().Err() != nil {
		acc.lock.RUnlock()
		release()
		return nil, nextRequest, false, fmt.Errorf("恢复账号不可用或请求已取消")
	}
	statsSelectAccount(nextRequest, acc, model)
	recordModelRequest(model)
	resp, cancel, err := doUpstreamRequest(nextRequest, acc, prof, raw, reqID, debugTraceID(r))
	acc.lock.RUnlock()
	if err != nil {
		release()
		recordModelFailure(model, "recovery_network_error")
		return nil, nextRequest, true, err
	}
	cancel = bindModelConcurrency(nextRequest, resp, cancel, release)
	resp.Body = newIdleReadCloser(resp.Body, currentIdleTimeout(), cancel)
	if resp.StatusCode == http.StatusOK {
		recordModelSuccess(model)
	} else {
		recordModelFailure(model, fmt.Sprint(resp.StatusCode))
	}
	return resp, nextRequest, true, nil
}

func combineRecoveryUsage(first, second map[string]any, recovery map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "completion_thinking_tokens",
		"cached_tokens", "cache_read_input_tokens", "cache_creation_input_tokens",
		"prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "prompt_cache_write_tokens"} {
		a, aOK := usageNumber(first, key)
		b, bOK := usageNumber(second, key)
		if aOK || bOK {
			out[key] = a + b // 缺失项只汇总已知部分，完整性标记与两轮原始usage一起返回。
		}
	}
	for _, key := range []string{"prompt_tokens_details", "completion_tokens_details"} {
		a, _ := first[key].(map[string]any)
		b, _ := second[key].(map[string]any)
		details := map[string]any{}
		for _, name := range []string{"cached_tokens", "reasoning_tokens", "audio_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens"} {
			x, xOK := usageNumber(a, name)
			y, yOK := usageNumber(b, name)
			if xOK || yOK {
				details[name] = x + y
			}
		}
		if len(details) > 0 {
			out[key] = details
		}
	}
	a, aOK := usageCredit(first)
	b, bOK := usageCredit(second)
	if aOK && bOK {
		out["credit"] = a + b
		recovery["total_credit"] = a + b
	}
	if quota := second["quota"]; quota != nil {
		out["quota"] = quota // 额度快照不能相加。
	} else if quota := first["quota"]; quota != nil {
		out["quota"] = quota
	}
	recovery["usage_complete"] = first != nil && second != nil && aOK && bOK
	recovery["initial_usage"], recovery["recovery_usage"] = first, second
	out["reasoning_recovery"] = recovery
	return out
}

func emptyBudgetSSE(model string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model": model, "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": "length",
		}},
	})
	return append(append([]byte("data: "), raw...), '\n', '\n')
}

// 恢复结果完整聚合后才交给原有六条响应路径，避免重复角色、结束事件或半截工具执行。
func recoveredSSE(original *http.Response, frames []byte, usage map[string]any, firstToken time.Duration, requests ...*http.Request) *http.Response {
	var output bytes.Buffer
	scanner := newUpstreamSSEScanner(bytes.NewReader(frames), requests...)
	scanner.passErrors = true
	for scanner.Scan() {
		line := scanner.Text()
		data := stripDataPrefix(line)
		if data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) == nil {
			if _, exists := chunk["usage"]; exists {
				delete(chunk, "usage")
				var encoded bytes.Buffer
				encoder := json.NewEncoder(&encoded)
				encoder.SetEscapeHTML(false)
				_ = encoder.Encode(chunk)
				line = "data: " + strings.TrimSuffix(encoded.String(), "\n")
			}
		}
		output.WriteString(line)
		output.WriteByte('\n')
	}
	// 保留原增量大小和工具分片，不能把多兆输出合成一个超大 SSE JSON 帧。
	encoded, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": usage})
	fmt.Fprintf(&output, "\ndata: %s\n\ndata: [DONE]\n\n", encoded)
	next := *original
	reader := bytes.NewReader(output.Bytes())
	next.Body = &prefixedResponseBody{Reader: reader, Closer: io.NopCloser(reader), firstToken: firstToken}
	return &next
}
