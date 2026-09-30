package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// 请求转换
// -----------------------------------------------------------------------------

func TestAnthropicToChatRequestBasic(t *testing.T) {
	body := map[string]any{
		"model":          "hy4-preview",
		"max_tokens":     float64(1024),
		"temperature":    0.5,
		"top_p":          0.9,
		"stop_sequences": []any{"END"},
		"system": []any{
			map[string]any{"type": "text", "text": "你是助手", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "简短回答"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
	}
	chat, err := anthropicToChatRequest(body, "hy4-preview")
	if err != nil {
		t.Fatal(err)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，得到 %d: %v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手\n简短回答" {
		t.Fatalf("system 转换错误: %v", first)
	}
	if chat["max_tokens"] != 1024 || chat["temperature"] != 0.5 {
		t.Fatalf("标量参数转换错误: %v", chat)
	}
	if stop, ok := chat["stop"].([]any); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop_sequences 转换错误: %v", chat["stop"])
	}
	if _, has := chat["reasoning_effort"]; has {
		t.Fatalf("未显式 thinking 时不应注入 reasoning_effort: %v", chat)
	}
}

func TestAnthropicToChatRequestToolRoundTrip(t *testing.T) {
	body := map[string]any{
		"max_tokens": float64(512),
		"messages": []any{
			map[string]any{"role": "user", "content": "查上海天气"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "我来查"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"city": "上海"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
					map[string]any{"type": "text", "text": "晴 28 度"},
				}},
				map[string]any{"type": "text", "text": "那北京呢"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "get_weather", "description": "查天气", "input_schema": map[string]any{"type": "object"}},
		},
		"tool_choice": map[string]any{"type": "auto"},
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
	}
	chat, err := anthropicToChatRequest(body, "m")
	if err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_effort"] != "high" {
		t.Fatalf("thinking enabled 无档位应映射 high: %v", chat["reasoning_effort"])
	}
	msgs := chat["messages"].([]any)
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	want := "user|assistant|tool|user"
	if got := strings.Join(roles, "|"); got != want {
		t.Fatalf("消息序列错误: got=%s want=%s", got, want)
	}
	asst := msgs[1].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["type"] != "function" {
		t.Fatalf("tool_calls 转换错误: %v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("函数名错误: %v", fn)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 应为合法 JSON: %v", fn["arguments"])
	}
	if args["city"] != "上海" {
		t.Fatalf("arguments 内容错误: %v", args)
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "晴 28 度" {
		t.Fatalf("tool_result 转换错误: %v", toolMsg)
	}
	if msgs[3].(map[string]any)["content"] != "那北京呢" {
		t.Fatalf("tool_result 后的文本应合并为 user 消息: %v", msgs[3])
	}
	tools := chat["tools"].([]any)
	tf := tools[0].(map[string]any)["function"].(map[string]any)
	if tf["name"] != "get_weather" || tf["parameters"] == nil || tf["description"] != "查天气" {
		t.Fatalf("tools 转换错误: %v", tf)
	}
	if chat["tool_choice"] != "auto" {
		t.Fatalf("tool_choice auto 应原样: %v", chat["tool_choice"])
	}
}

func TestAnthropicToolChoiceMapping(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want any
	}{
		{map[string]any{"type": "auto"}, "auto"},
		{map[string]any{"type": "none"}, "none"},
		{map[string]any{"type": "any"}, "required"},
		{map[string]any{"type": "any", "name": "f"}, map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
		{map[string]any{"type": "tool", "name": "f"}, map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
	}
	for i, c := range cases {
		if got := convertAnthropicToolChoice(c.in); !equalAny(got, c.want) {
			t.Fatalf("case %d: got=%v want=%v", i, got, c.want)
		}
	}
}

func TestAnthropicMessagesRequired(t *testing.T) {
	if _, err := anthropicToChatRequest(map[string]any{"model": "m"}, "m"); err == nil {
		t.Fatal("messages 为空应报错")
	}
}

func equalAny(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// -----------------------------------------------------------------------------
// SSE 状态机
// -----------------------------------------------------------------------------

func eventNames(out []byte) []string {
	var names []string
	for _, block := range strings.Split(string(out), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				names = append(names, strings.TrimPrefix(line, "event: "))
			}
		}
	}
	return names
}

func mustFeed(t *testing.T, s *anthropicStreamState, line string) []byte {
	t.Helper()
	var chunk map[string]any
	if err := json.Unmarshal([]byte(line), &chunk); err != nil {
		t.Fatal(err)
	}
	return s.feed(chunk)
}

func TestAnthropicStreamStateEventSequence(t *testing.T) {
	s := newAnthropicStreamState("m1")
	var names []string
	first := mustFeed(t, s, `{"model":"m2","choices":[{"delta":{"reasoning_content":"思考"}}]}`)
	names = append(names, eventNames(first)...)
	names = append(names, eventNames(mustFeed(t, s, `{"choices":[{"delta":{"content":"答案"}}]}`))...)
	names = append(names, eventNames(mustFeed(t, s, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`))...)
	names = append(names, eventNames(mustFeed(t, s, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))...)
	names = append(names, eventNames(s.finishEvents())...)

	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", // thinking
		"content_block_stop",
		"content_block_start", "content_block_delta", // text
		"content_block_stop",
		"content_block_start", "content_block_delta", // tool_use
		"content_block_stop",
		"message_delta", "message_stop",
	}
	if len(names) != len(want) {
		t.Fatalf("事件序列长度 %d != %d: %v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("事件[%d]=%s want=%s 全序列=%v", i, names[i], want[i], names)
		}
	}
	// message_start 里 model 用 chunk 覆盖值
	if !strings.Contains(string(first), `"model":"m2"`) {
		t.Fatalf("message_start 应携带上游 chunk 的 model:\n%s", first)
	}
}

func TestAnthropicStreamStateContentBlocks(t *testing.T) {
	s := newAnthropicStreamState("m1")
	var stream strings.Builder
	stream.Write(mustFeed(t, s, `{"choices":[{"delta":{"reasoning_content":"嗯"}}]}`))
	stream.Write(mustFeed(t, s, `{"choices":[{"delta":{"content":"正文"}}]}`))
	stream.Write(mustFeed(t, s, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`))
	stream.Write(mustFeed(t, s, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`))
	stream.Write(s.finishEvents())
	out := stream.String()
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("message_delta stop_reason 错误: %s", out)
	}
	if !strings.Contains(out, `"input_tokens":0`) || !strings.Contains(out, `"output_tokens":0`) {
		t.Fatalf("message_delta usage 缺失: %s", out)
	}
	// tool_use 的 input_json_delta 分两段流出
	if !strings.Contains(out, `"partial_json":"1}"`) {
		t.Fatalf("arguments 增量缺失: %s", out)
	}
}

func TestAnthropicStreamStateAggregate(t *testing.T) {
	s := newAnthropicStreamState("m1")
	mustFeed(t, s, `{"choices":[{"delta":{"reasoning_content":"思考过程"}}]}`)
	mustFeed(t, s, `{"choices":[{"delta":{"content":"最终答案"}}]}`)
	mustFeed(t, s, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"fn","arguments":"{\"k\":2}"}}]}}]}`)
	mustFeed(t, s, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	msg := s.aggregate()
	if msg["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason: %v", msg["stop_reason"])
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"] != 7 || usage["output_tokens"] != 3 {
		t.Fatalf("usage 映射错误: %v", usage)
	}
	blocks := msg["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("content 块数 %d: %v", len(blocks), blocks)
	}
	if blocks[0].(map[string]any)["type"] != "thinking" || blocks[0].(map[string]any)["thinking"] != "思考过程" {
		t.Fatalf("thinking 块错误: %v", blocks[0])
	}
	if blocks[1].(map[string]any)["text"] != "最终答案" {
		t.Fatalf("text 块错误: %v", blocks[1])
	}
	tu := blocks[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_9" || tu["name"] != "fn" {
		t.Fatalf("tool_use 块错误: %v", tu)
	}
	if input, ok := tu["input"].(map[string]any); !ok || input["k"] != float64(2) {
		t.Fatalf("tool_use input 应解析为对象: %v", tu["input"])
	}
	if msg["id"].(string)[:4] != "msg_" || msg["type"] != "message" || msg["role"] != "assistant" {
		t.Fatalf("Message 信封错误: %v", msg)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"tool_calls":     "tool_use",
		"length":         "max_tokens",
		"stop_sequence":  "stop_sequence",
		"content_filter": "end_turn",
		"":               "end_turn",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Fatalf("mapStopReason(%q)=%s want=%s", in, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// 错误出口分发
// -----------------------------------------------------------------------------

func TestWriteUpstreamErrorDispatch(t *testing.T) {
	// Anthropic 形状
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(withAnthropicErrFormat(req.Context()))
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, req, http.StatusUnauthorized, "no_auth", "没有凭据")
	var anth map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &anth); err != nil {
		t.Fatalf("非 JSON: %s", rec.Body.String())
	}
	if anth["type"] != "error" {
		t.Fatalf("应为 Anthropic error 形状: %s", rec.Body.String())
	}
	inner := anth["error"].(map[string]any)
	if inner["type"] != "authentication_error" {
		t.Fatalf("no_auth 应映射 authentication_error: %v", inner)
	}
	// OpenAI 形状（默认路径）
	rec2 := httptest.NewRecorder()
	writeUpstreamError(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), http.StatusUnauthorized, "no_auth", "没有凭据")
	var openai map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &openai); err != nil {
		t.Fatalf("非 JSON: %s", rec2.Body.String())
	}
	if _, hasType := openai["type"]; hasType {
		t.Fatalf("默认应为 OpenAI 形状: %s", rec2.Body.String())
	}
}

// -----------------------------------------------------------------------------
// 端到端（假上游）
// -----------------------------------------------------------------------------

func setupMessagesUpstream(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "free.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
		ModelStates: map[string]*modelRuntimeState{"free-model": {CostClass: modelCostFree}}}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
}

func messagesRequest(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleMessages(rec, req)
	return rec
}

func TestHandleMessagesStreamEndToEnd(t *testing.T) {
	chdirTemp(t)
	setupMessagesUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"Hi"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rec := messagesRequest(t, `{"model":"free-model","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"text":"Hi"`,
		`"type":"text_delta"`,
		"event: content_block_stop",
		`"stop_reason":"end_turn"`,
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("流式响应缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "data: [DONE]") {
		t.Fatal("Anthropic SSE 不应有 [DONE]")
	}
}

func TestHandleMessagesNonStreamEndToEnd(t *testing.T) {
	chdirTemp(t)
	setupMessagesUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"Hello"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rec := messagesRequest(t, `{"model":"free-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("非 JSON: %s", rec.Body.String())
	}
	if msg["type"] != "message" || msg["role"] != "assistant" || msg["stop_reason"] != "end_turn" {
		t.Fatalf("Message 对象错误: %v", msg)
	}
	blocks := msg["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "Hello" {
		t.Fatalf("content 错误: %v", blocks)
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage 错误: %v", usage)
	}
}

func TestHandleMessagesUpstreamBodyIsChat(t *testing.T) {
	chdirTemp(t)
	var captured map[string]any
	setupMessagesUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("上游路径错误: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
	})
	rec := messagesRequest(t, `{"model":"free-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if captured == nil || captured["stream"] != true {
		t.Fatalf("上游请求必须强制 stream=true: %v", captured)
	}
	if captured["model"] != "free-model" {
		t.Fatalf("model 应透传: %v", captured)
	}
}

func TestHandleMessagesInvalidBody(t *testing.T) {
	rec := messagesRequest(t, `{invalid`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("应为 Anthropic 错误形状: %s", rec.Body.String())
	}
}

// -----------------------------------------------------------------------------
// count_tokens
// -----------------------------------------------------------------------------

func TestHandleCountTokens(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","system":"sys","messages":[{"role":"user","content":"你好世界"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`))
	rec := httptest.NewRecorder()
	handleCountTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	n, ok := out["input_tokens"].(float64)
	if !ok || n < 4 {
		t.Fatalf("input_tokens 应至少覆盖 CJK 文本: %v", out)
	}
}

func TestApproxTokens(t *testing.T) {
	if got := approxTokens("你好"); got != 2 {
		t.Fatalf("CJK 应按 1 token/字: %d", got)
	}
	if got := approxTokens("abcd"); got != 1 {
		t.Fatalf("ASCII 应按 4 字符/token: %d", got)
	}
}
