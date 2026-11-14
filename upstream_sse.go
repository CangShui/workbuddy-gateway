package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

type upstreamSSEFailure struct {
	message string
}

func (e *upstreamSSEFailure) Error() string {
	return "上游返回错误事件，本次响应未完成"
}

// upstreamSSEError 只识别顶层错误信封，不检查正文、工具参数或嵌套用户数据。
func upstreamSSEError(payload string) error {
	if isAccountRestrictionPayload([]byte(payload)) {
		return errUpstreamAccountRestricted
	}
	if !strings.Contains(payload, `"error"`) && !strings.Contains(payload, `"code"`) {
		return nil
	}
	var envelope map[string]any
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return nil
	}
	source, failed := envelope, envelope["error"] != nil
	if nested, ok := envelope["error"].(map[string]any); ok {
		source = nested
	}
	if _, choices := envelope["choices"]; !choices && source["code"] != nil {
		code := fmt.Sprint(source["code"])
		failed = failed || code != "0" && (source["msg"] != nil || source["message"] != nil)
	}
	if !failed {
		return nil
	}
	message := strOf(source["message"])
	if message == "" {
		message = strOf(source["msg"])
	}
	if message == "" {
		message = "上游返回错误事件，本次响应未完成"
	}
	if code := source["code"]; code != nil {
		message = fmt.Sprintf("上游错误（业务码 %v）：%s", code, message)
	}
	return &upstreamSSEFailure{message: message}
}

func upstreamSSEClientError(err error) map[string]any {
	var failure *upstreamSSEFailure
	if !errors.As(err, &failure) {
		return nil
	}
	return map[string]any{"type": "api_error", "code": "upstream_error", "message": failure.message}
}

// splitUpstreamSSELine 只拆分同一行中完整、连续的 JSON 对象。
// 普通帧、心跳、元数据和损坏的 JSON 原样保留，不根据正文中的括号切割。
func splitUpstreamSSELine(line string) []string {
	payload := strings.TrimSpace(line)
	if !strings.HasPrefix(payload, "data:") && !strings.HasPrefix(payload, "{") {
		return nil
	}
	payload = stripDataPrefix(payload)
	if !strings.HasPrefix(payload, "{") || json.Valid([]byte(payload)) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	var parts []string
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil || len(raw) == 0 || raw[0] != '{' {
			return nil
		}
		parts = append(parts, "data: "+string(raw))
	}
	if len(parts) < 2 {
		return nil
	}
	return parts
}

func logUpstreamSSESplit(r *http.Request, count int) {
	log.Printf("[流式协议] traceId=%s 阶段=上游粘帧解析 拆分数=%d 结果=逐个处理完整JSON对象 业务影响=正文、工具、用量和错误不再因粘帧丢失；不记录帧正文",
		debugTraceID(r), count)
	debugEvent(r, "info", "upstream_sse_split", map[string]any{
		"object_count": count, "business_impact": "完整JSON对象分别解析，原有顺序与参数内容保留",
	})
}

// upstreamSSEScanner 让三种协议、聚合和恢复共享同一行解析规则。
// 每次 Scan 只交付一个对象；遇到明确风控错误即停止，后续帧不得覆盖错误终态。
type upstreamSSEScanner struct {
	*bufio.Scanner
	request    *http.Request
	pending    []string
	text       string
	err        error
	passErrors bool // 重组已捕获的恢复帧时保留错误，由最终响应出口处理。
}

func newUpstreamSSEScanner(reader io.Reader, requests ...*http.Request) *upstreamSSEScanner {
	s := &upstreamSSEScanner{Scanner: bufio.NewScanner(reader)}
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if len(requests) > 0 {
		s.request = requests[0]
	}
	return s
}

func (s *upstreamSSEScanner) Scan() bool {
	if s.err != nil {
		return false
	}
	if len(s.pending) == 0 {
		if !s.Scanner.Scan() {
			return false
		}
		s.text = s.Scanner.Text()
		if parts := splitUpstreamSSELine(s.text); len(parts) > 1 {
			logUpstreamSSESplit(s.request, len(parts))
			s.pending = parts
		}
	}
	if len(s.pending) > 0 {
		s.text, s.pending = s.pending[0], s.pending[1:]
	}
	if !s.passErrors {
		if err := upstreamSSEError(stripDataPrefix(s.text)); err != nil {
			s.err = err
			log.Printf("[流式协议] traceId=%s 阶段=上游错误事件 结果=停止读取 业务影响=错误后不再交付成功终态；不记录错误帧正文",
				debugTraceID(s.request))
			debugEvent(s.request, "warn", "upstream_sse_error", map[string]any{
				"account_restricted": errors.Is(err, errUpstreamAccountRestricted),
				"business_impact":    "保留失败语义，后续终态不覆盖错误，不触发网络重放",
			})
			return false
		}
	}
	return true
}

func (s *upstreamSSEScanner) Text() string { return s.text }

func (s *upstreamSSEScanner) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.Scanner.Err()
}
