package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// 本文件锁定一个静默失败：上游在**每一个**分片上都下发 finish_reason:""，
// 而 Anthropic 协议翻译层（Claude Code 链路）会取流中「第一个非 null 的
// finish_reason」作为最终 stop_reason，于是首个分片的空串把 stop_reason 锁成
// end_turn，真实终止分片的 "tool_calls" 不再被采纳 —— Claude Code 只在
// stop_reason=tool_use 时才执行工具，工具因此永不执行、终端毫无结果。
//
// 固定分片序列回放（经 cc-switch 翻译）实测：
//   任一分片带 ""                      -> stop_reason=end_turn（工具被丢弃）
//   全部 null + 终止片 "tool_calls"    -> stop_reason=tool_use（工具执行）
// -----------------------------------------------------------------------------

// 中间分片的空 finish_reason 必须归一化为 null；终止分片的真实原因必须保留。
func TestCleanChunkJSONNormalizesEmptyFinishReason(t *testing.T) {
	intermediate := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"","logprobs":null}]}`
	cleaned := cleanChunkJSON(intermediate)
	if strings.Contains(cleaned, `"finish_reason":""`) {
		t.Fatalf("空 finish_reason 必须归一化为 null，否则翻译层会把 stop_reason 锁成 end_turn: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"finish_reason":null`) {
		t.Fatalf("空 finish_reason 必须归一化为 null: %s", cleaned)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(cleaned), &obj); err != nil {
		t.Fatalf("清洗后应为合法 JSON: %v", err)
	}
	if fr := obj["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != nil {
		t.Fatalf("finish_reason 应为 null，实际 %#v", fr)
	}

	terminal := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls","logprobs":null}]}`
	cleanedTerm := cleanChunkJSON(terminal)
	if !strings.Contains(cleanedTerm, `"finish_reason":"tool_calls"`) {
		t.Fatalf("终止分片的 finish_reason 必须原样保留: %s", cleanedTerm)
	}

	// "stop" 同样必须保留，不能被误改
	stopTerm := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	if c := cleanChunkJSON(stopTerm); !strings.Contains(c, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason=stop 必须保留: %s", c)
	}
}

// 旧版 function_call 空壳（name/arguments 均为空）应清除；真实的旧版调用必须保留。
func TestCleanChunkJSONRemovesLegacyFunctionCallStub(t *testing.T) {
	stub := `{"id":"c1","choices":[{"index":0,"delta":{"function_call":{"arguments":"","name":""},"role":"assistant"},"finish_reason":"tool_calls"}]}`
	cleaned := cleanChunkJSON(stub)
	if strings.Contains(cleaned, "function_call") {
		t.Fatalf("空壳 function_call 应被清除: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"finish_reason":"tool_calls"`) {
		t.Fatalf("清理空壳不应影响 finish_reason: %s", cleaned)
	}
	if !strings.Contains(cleaned, `"role":"assistant"`) {
		t.Fatalf("role 属合法字段，不应被删除: %s", cleaned)
	}

	real := `{"id":"c1","choices":[{"index":0,"delta":{"function_call":{"name":"get_weather","arguments":"{}"}},"finish_reason":"function_call"}]}`
	cleanedReal := cleanChunkJSON(real)
	if !strings.Contains(cleanedReal, `"name":"get_weather"`) {
		t.Fatalf("真实的旧版函数调用不应被误删: %s", cleanedReal)
	}
}

// 既有行为回归：delta 中的空值字段仍应被清除，非 JSON 原样返回。
func TestCleanChunkJSONRemovesEmptyDeltaFields(t *testing.T) {
	cleaned := cleanChunkJSON(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"思考中"},"finish_reason":""}]}`)
	if strings.Contains(cleaned, `"content"`) {
		t.Fatalf("delta 中的空值应被清除: %s", cleaned)
	}
	if !strings.Contains(cleaned, "思考中") {
		t.Fatalf("非空字段不应被清除: %s", cleaned)
	}
	if got := cleanChunkJSON("event: ping"); got != "event: ping" {
		t.Fatalf("非 JSON 行应原样返回，实际: %s", got)
	}
}

// runStreamCase 用假账号 + 假上游跑通真实 handleChatCompletions 链路，
// 返回客户端最终收到的状态码与 SSE 字节。
func runStreamCase(t *testing.T, upstream http.HandlerFunc) (int, string) {
	t.Helper()

	srv := httptest.NewServer(upstream)
	defer srv.Close()

	oldBase := profileCN.Base
	oldClient := cfg.HttpClient
	oldVerbose := cfg.Verbose
	profileCN.Base = srv.URL
	cfg.HttpClient = &http.Client{Timeout: 30 * time.Second}
	cfg.Verbose = false
	defer func() {
		profileCN.Base = oldBase
		cfg.HttpClient = oldClient
		cfg.Verbose = oldVerbose
	}()

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{
		Path: "stream-test.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "test", RefreshToken: "rt", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()},
			Account: StoredAccount{UID: "u1", Nickname: "tester"},
		},
	}}
	rrIndex = 0
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	body := `{"model":"hy3-preview","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// 端到端：上游下发「每个分片都带 finish_reason:""」的含工具调用流，
// 客户端收到的分片里绝不能残留空串，终止分片必须是 "tool_calls"。
func TestStreamChatResponseKeepsToolCallFinishReason(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"I will check the git status."},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": \"git status\"}"}}]},"finish_reason":"","logprobs":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"","name":""},"role":"assistant"},"finish_reason":"tool_calls","logprobs":null}],"usage":{"prompt_tokens":9,"completion_tokens":6}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	code, out := runStreamCase(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	})
	t.Logf("状态码=%d 客户端收到:\n%s", code, out)

	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if strings.Contains(out, `"finish_reason":""`) {
		t.Fatalf("客户端不应收到空 finish_reason（会把 stop_reason 锁成 end_turn）: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("终止分片必须保留 finish_reason=tool_calls: %s", out)
	}
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("工具调用必须透传: %s", out)
	}
	if strings.Contains(out, "function_call") {
		t.Fatalf("旧版 function_call 空壳必须被清除: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("缺少 [DONE] 结束标记")
	}
}
