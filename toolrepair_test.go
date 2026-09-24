package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testToolCall(id, name string) map[string]any {
	return map[string]any{
		"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": "{}"},
	}
}

func testAssistantCalls(calls ...map[string]any) map[string]any {
	items := make([]any, 0, len(calls))
	for _, call := range calls {
		items = append(items, call)
	}
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": items}
}

func testToolOutput(id, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
}

func messageContent(messageAny any) string {
	message, _ := messageAny.(map[string]any)
	content, _ := message["content"].(string)
	return content
}

func outputIDs(messages []any) []string {
	ids := make([]string, 0)
	for _, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if ok && roleOfMessage(message) == "tool" {
			ids = append(ids, toolOutputID(message))
		}
	}
	return ids
}

func callIDs(messageAny any) []string {
	_, calls, ok := assistantCalls(messageAny)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(calls))
	for _, callAny := range calls {
		ids = append(ids, toolCallID(callAny))
	}
	return ids
}

func TestRepairParallelToolMessageSequence(t *testing.T) {
	callA := testToolCall("call_a", "a")
	callB := testToolCall("call_b", "b")

	cases := []struct {
		name             string
		messages         []any
		wantRoles        []string
		wantCallIDs      []string
		wantOutputIDs    []string
		wantTailContents []string
		wantMoved        int
		wantMerged       int
	}{
		{
			name:          "正常并行序列保持有效",
			messages:      []any{msg("system", "s"), testAssistantCalls(callA, callB), testToolOutput("call_a", "A"), testToolOutput("call_b", "B")},
			wantRoles:     []string{"system", "assistant", "tool", "tool"},
			wantCallIDs:   []string{"call_a", "call_b"},
			wantOutputIDs: []string{"call_a", "call_b"},
		},
		{
			name:          "逆序结果保持逆序",
			messages:      []any{msg("system", "s"), testAssistantCalls(callA, callB), testToolOutput("call_b", "B"), testToolOutput("call_a", "A")},
			wantRoles:     []string{"system", "assistant", "tool", "tool"},
			wantCallIDs:   []string{"call_a", "call_b"},
			wantOutputIDs: []string{"call_b", "call_a"},
		},
		{
			name:             "并行调用中间的assistant消息移到结果后",
			messages:         []any{msg("system", "s"), testAssistantCalls(callA), msg("assistant", "插入消息"), testAssistantCalls(callB), testToolOutput("call_a", "A"), testToolOutput("call_b", "B")},
			wantRoles:        []string{"system", "assistant", "tool", "tool", "assistant"},
			wantCallIDs:      []string{"call_a", "call_b"},
			wantOutputIDs:    []string{"call_a", "call_b"},
			wantTailContents: []string{"插入消息"},
			wantMoved:        1,
			wantMerged:       1,
		},
		{
			name:             "并行调用中间的user消息移到结果后",
			messages:         []any{msg("system", "s"), testAssistantCalls(callA, callB), msg("user", "插入用户消息"), testToolOutput("call_b", "B"), testToolOutput("call_a", "A")},
			wantRoles:        []string{"system", "assistant", "tool", "tool", "user"},
			wantCallIDs:      []string{"call_a", "call_b"},
			wantOutputIDs:    []string{"call_b", "call_a"},
			wantTailContents: []string{"插入用户消息"},
			wantMoved:        1,
		},
		{
			name:             "并行结果之间的消息移到全部结果后",
			messages:         []any{msg("system", "s"), testAssistantCalls(callA, callB), testToolOutput("call_a", "A"), msg("assistant", "结果间消息"), testToolOutput("call_b", "B")},
			wantRoles:        []string{"system", "assistant", "tool", "tool", "assistant"},
			wantCallIDs:      []string{"call_a", "call_b"},
			wantOutputIDs:    []string{"call_a", "call_b"},
			wantTailContents: []string{"结果间消息"},
			wantMoved:        1,
		},
		{
			name:             "单调用中间消息保持原位",
			messages:         []any{msg("system", "s"), testAssistantCalls(callA), msg("assistant", "单调用插入"), testToolOutput("call_a", "A")},
			wantRoles:        []string{"system", "assistant", "assistant", "tool"},
			wantCallIDs:      []string{"call_a"},
			wantOutputIDs:    []string{"call_a"},
			wantTailContents: []string{"单调用插入"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := map[string]any{"messages": tc.messages}
			report := repairToolMessageSequence(obj)
			messages := obj["messages"].([]any)
			if got := rolesOf(obj); !reflect.DeepEqual(got, tc.wantRoles) {
				t.Fatalf("roles=%v want=%v messages=%#v", got, tc.wantRoles, messages)
			}
			assistantIndex := -1
			for i, messageAny := range messages {
				if len(callIDs(messageAny)) > 0 {
					assistantIndex = i
					break
				}
			}
			if assistantIndex < 0 || !reflect.DeepEqual(callIDs(messages[assistantIndex]), tc.wantCallIDs) {
				t.Fatalf("call IDs=%v want=%v", callIDs(messages[assistantIndex]), tc.wantCallIDs)
			}
			if got := outputIDs(messages); !reflect.DeepEqual(got, tc.wantOutputIDs) {
				t.Fatalf("output IDs=%v want=%v", got, tc.wantOutputIDs)
			}
			for _, want := range tc.wantTailContents {
				found := false
				for _, messageAny := range messages {
					if messageContent(messageAny) == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing preserved message %q in %#v", want, messages)
				}
			}
			if report.MovedMessages != tc.wantMoved || report.MergedCallMessages != tc.wantMerged {
				t.Fatalf("report=%+v want moved=%d merged=%d", report, tc.wantMoved, tc.wantMerged)
			}
		})
	}
}

func TestRepairToolMessageSequenceSymmetricTrimming(t *testing.T) {
	obj := map[string]any{"messages": []any{
		msg("system", "s"),
		testToolOutput("orphan_before", "x"),
		testAssistantCalls(testToolCall("call_a", "a"), testToolCall("missing", "missing"), testToolCall("call_a", "duplicate")),
		testToolOutput("orphan", "x"),
		testToolOutput("call_a", "first"),
		testToolOutput("call_a", "duplicate"),
	}}
	report := repairToolMessageSequence(obj)
	messages := obj["messages"].([]any)
	if got := rolesOf(obj); !reflect.DeepEqual(got, []string{"system", "assistant", "tool"}) {
		t.Fatalf("roles=%v messages=%#v", got, messages)
	}
	if got := callIDs(messages[1]); !reflect.DeepEqual(got, []string{"call_a"}) {
		t.Fatalf("calls=%v", got)
	}
	if got := outputIDs(messages); !reflect.DeepEqual(got, []string{"call_a"}) {
		t.Fatalf("outputs=%v", got)
	}
	if messageContent(messages[2]) != "first" {
		t.Fatalf("first duplicate output must win: %#v", messages[2])
	}
	if report.DroppedCalls != 2 || report.DroppedOutputs != 3 {
		t.Fatalf("unexpected report: %+v", report)
	}

	before, _ := json.Marshal(obj["messages"])
	second := repairToolMessageSequence(obj)
	after, _ := json.Marshal(obj["messages"])
	if string(before) != string(after) || second.changed() {
		t.Fatalf("repair must be idempotent: before=%s after=%s report=%+v", before, after, second)
	}
}

func TestRepairToolMessageSequencePreservesLegacyFunctionCall(t *testing.T) {
	legacyCall := map[string]any{"role": "assistant", "content": nil, "function_call": map[string]any{"name": "weather", "arguments": "{}"}}
	legacyOutput := map[string]any{"role": "function", "name": "weather", "content": "sunny"}
	obj := map[string]any{"messages": []any{msg("system", "s"), legacyCall, legacyOutput}}
	before, _ := json.Marshal(obj["messages"])
	report := repairToolMessageSequence(obj)
	after, _ := json.Marshal(obj["messages"])
	if string(before) != string(after) || report.changed() {
		t.Fatalf("legacy sequence changed: before=%s after=%s report=%+v", before, after, report)
	}
}

func TestToolMessageTopologyIsContentFree(t *testing.T) {
	messages := []any{
		msg("system", "SECRET_SYSTEM_PROMPT"),
		testAssistantCalls(testToolCall("call_a", "a"), testToolCall("call_b", "b")),
		testToolOutput("call_a", "SECRET_TOOL_RESULT"),
		msg("user", "SECRET_USER_TEXT"),
	}
	topology := toolMessageTopology(messages)
	for _, secret := range []string{"SECRET_SYSTEM_PROMPT", "SECRET_TOOL_RESULT", "SECRET_USER_TEXT"} {
		if strings.Contains(topology, secret) {
			t.Fatalf("topology leaked content %q: %s", secret, topology)
		}
	}
	if topology != "system > assistant[call_a,call_b] > tool[call_a] > user" {
		t.Fatalf("unexpected topology: %s", topology)
	}
}

func TestResponsesParallelToolHistoryRepair(t *testing.T) {
	respReq := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_a", "name": "a", "arguments": "{}"},
			map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed"},
			map[string]any{"type": "message", "role": "assistant", "content": "插入消息"},
			map[string]any{"type": "function_call", "call_id": "call_b", "name": "b", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "B"},
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "A"},
		},
	}
	chat, err := responsesToChatRequest(respReq, "m")
	if err != nil {
		t.Fatal(err)
	}
	ensureLeadingSystemMessage(chat)
	report := repairToolMessageSequence(chat)
	messages := chat["messages"].([]any)
	if got := rolesOf(chat); !reflect.DeepEqual(got, []string{"system", "assistant", "tool", "tool"}) {
		t.Fatalf("roles=%v messages=%#v", got, messages)
	}
	if got := callIDs(messages[1]); !reflect.DeepEqual(got, []string{"call_a", "call_b"}) {
		t.Fatalf("calls=%v", got)
	}
	if got := outputIDs(messages); !reflect.DeepEqual(got, []string{"call_b", "call_a"}) {
		t.Fatalf("outputs=%v", got)
	}
	if messageContent(messages[1]) != "插入消息" {
		t.Fatalf("assistant text must stay with its tool calls: %#v", messages)
	}
	if report.MovedMessages != 0 || report.MergedCallMessages != 0 {
		t.Fatalf("Responses items were not grouped before repair: %+v", report)
	}
}

func captureUpstreamMessages(t *testing.T, route, requestBody string) []any {
	t.Helper()
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode upstream body: %v: %s", err, body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured <- request
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	oldBase := profileCN.Base
	oldClient := cfg.HttpClient
	profileCN.Base = upstream.URL
	cfg.HttpClient = &http.Client{Timeout: 5 * time.Second}
	defer func() {
		profileCN.Base = oldBase
		cfg.HttpClient = oldClient
	}()

	accountMu.Lock()
	oldAccounts := accounts
	oldRRIndex := rrIndex
	accounts = []*Account{{
		Path: "repair-test.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}}
	rrIndex = 0
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		rrIndex = oldRRIndex
		accountMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(requestBody))
	rec := httptest.NewRecorder()
	if route == "/v1/responses" {
		requestAuditMiddleware(http.HandlerFunc(handleResponses)).ServeHTTP(rec, req)
	} else {
		requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)).ServeHTTP(rec, req)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("route %s status=%d body=%s", route, rec.Code, rec.Body.String())
	}
	select {
	case request := <-captured:
		messages, _ := request["messages"].([]any)
		return messages
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not captured")
		return nil
	}
}

func TestChatEndpointRepairsBrokenParallelToolHistory(t *testing.T) {
	body := `{
		"model":"m","stream":true,"messages":[
			{"role":"system","content":"s"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"a","arguments":"{}"}},
				{"id":"call_b","type":"function","function":{"name":"b","arguments":"{}"}}
			]},
			{"role":"assistant","content":"插入消息"},
			{"role":"tool","tool_call_id":"call_b","content":"B"},
			{"role":"tool","tool_call_id":"call_a","content":"A"}
		]}`
	messages := captureUpstreamMessages(t, "/v1/chat/completions", body)
	obj := map[string]any{"messages": messages}
	if got := rolesOf(obj); !reflect.DeepEqual(got, []string{"system", "assistant", "tool", "tool", "assistant"}) {
		t.Fatalf("upstream roles=%v messages=%#v", got, messages)
	}
	if got := outputIDs(messages); !reflect.DeepEqual(got, []string{"call_b", "call_a"}) {
		t.Fatalf("upstream outputs=%v", got)
	}
	if messageContent(messages[4]) != "插入消息" {
		t.Fatalf("interposed message not repaired: %#v", messages)
	}
}

func TestResponsesEndpointRepairsBrokenParallelToolHistory(t *testing.T) {
	body := `{
		"model":"m","stream":true,"input":[
			{"type":"function_call","call_id":"call_a","name":"a","arguments":"{}"},
			{"type":"web_search_call","id":"ws_1","status":"completed"},
			{"type":"message","role":"user","content":"插入消息"},
			{"type":"function_call","call_id":"call_b","name":"b","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_b","output":"B"},
			{"type":"function_call_output","call_id":"call_a","output":"A"}
		]}`
	messages := captureUpstreamMessages(t, "/v1/responses", body)
	obj := map[string]any{"messages": messages}
	if got := rolesOf(obj); !reflect.DeepEqual(got, []string{"system", "assistant", "tool", "tool", "user"}) {
		t.Fatalf("upstream roles=%v messages=%#v", got, messages)
	}
	if got := callIDs(messages[1]); !reflect.DeepEqual(got, []string{"call_a", "call_b"}) {
		t.Fatalf("upstream calls=%v", got)
	}
	if got := outputIDs(messages); !reflect.DeepEqual(got, []string{"call_b", "call_a"}) {
		t.Fatalf("upstream outputs=%v", got)
	}
	if messageContent(messages[4]) != "插入消息" {
		t.Fatalf("interposed message not repaired: %#v", messages)
	}
}

// 从 HTTP 入口一直验证到实际发往 Chat 上游的 JSON：同一次 Responses 回复的
// 推理、正文和并行工具调用必须在同一条 assistant 消息，而不是被拆成两条。
func TestResponsesEndpointReplaysReasoningWithTextAndTools(t *testing.T) {
	body := `{
		"model":"m","stream":true,"reasoning":{"effort":"high"},"input":[
			{"role":"user","content":"查日志"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"我先分析"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"开始检查"}]},
			{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
			{"type":"function_call","call_id":"call_b","name":"grep","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"file"},
			{"type":"function_call_output","call_id":"call_b","output":"match"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"确认好了"}]},
			{"type":"message","role":"assistant","content":"完成"}
		]}`
	messages := captureUpstreamMessages(t, "/v1/responses", body)
	if got := rolesOf(map[string]any{"messages": messages}); !reflect.DeepEqual(got, []string{"system", "user", "assistant", "tool", "tool", "assistant"}) {
		t.Fatalf("upstream roles=%v messages=%#v", got, messages)
	}
	first := messages[2].(map[string]any)
	if first["reasoning_content"] != "我先分析" || !reflect.DeepEqual(callIDs(first), []string{"call_a", "call_b"}) {
		t.Fatalf("first assistant lost reasoning or parallel calls: %#v", first)
	}
	parts := first["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "开始检查" {
		t.Fatalf("first assistant lost text: %#v", first)
	}
	last := messages[5].(map[string]any)
	if last["reasoning_content"] != "确认好了" || last["content"] != "完成" {
		t.Fatalf("next assistant lost reasoning or reply: %#v", last)
	}
}

// 实际复现形态：thinking 模式下先有真实推理，下一步只有正文和并行工具调用。
// 工具结果送回国际站时必须保留空 reasoning_content 字段，不能拆成两条 assistant。
func TestResponsesThinkingNoReasoningStillEchoesEmptyField(t *testing.T) {
	body := `{
		"model":"deepseek-v4.1-flash","stream":true,"reasoning":{"effort":"high"},"input":[
			{"role":"user","content":"检查文件"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"先读取"}]},
			{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"文件存在"},
			{"type":"message","role":"assistant","content":"继续检查"},
			{"type":"function_call","call_id":"call_b","name":"grep","arguments":"{}"},
			{"type":"function_call","call_id":"call_c","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_b","output":"已匹配"},
			{"type":"function_call_output","call_id":"call_c","output":"已读取"}
		]}`
	messages := captureUpstreamMessages(t, "/v1/responses", body)
	if got := rolesOf(map[string]any{"messages": messages}); !reflect.DeepEqual(got, []string{"system", "user", "assistant", "tool", "assistant", "tool", "tool"}) {
		t.Fatalf("unexpected message topology: %v", got)
	}
	if messages[2].(map[string]any)["reasoning_content"] != "先读取" {
		t.Fatal("original reasoning was not preserved")
	}
	second := messages[4].(map[string]any)
	if value, exists := second["reasoning_content"]; !exists || value != "" {
		t.Fatalf("missing explicit empty reasoning field: %#v", second)
	}
	if second["reasoning"] != " " {
		t.Fatalf("thinking assistant without reasoning must carry a whitespace marker: %#v", second)
	}
	if second["content"] != "继续检查" || !reflect.DeepEqual(callIDs(second), []string{"call_b", "call_c"}) {
		t.Fatalf("text and calls were not grouped into the same reply: %#v", second)
	}
}

func TestResponsesWithoutThinkingDoesNotAddReasoningField(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","stream":true,"reasoning":{"effort":"none"},"input":[
		{"role":"user","content":"run"},
		{"type":"message","role":"assistant","content":"running"},
		{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_a","output":"ok"}
	]}`
	messages := captureUpstreamMessages(t, "/v1/responses", body)
	assistant := messages[2].(map[string]any)
	if _, exists := assistant["reasoning_content"]; exists {
		t.Fatalf("non-thinking request should not gain reasoning field: %#v", assistant)
	}
}

func TestChatRouteDoesNotInventReasoningField(t *testing.T) {
	body := `{"model":"m","stream":true,"reasoning_effort":"high","messages":[
		{"role":"system","content":"s"},
		{"role":"user","content":"run"},
		{"role":"assistant","content":"running","tool_calls":[
			{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call_a","content":"ok"}
	]}`
	messages := captureUpstreamMessages(t, "/v1/chat/completions", body)
	if _, exists := messages[2].(map[string]any)["reasoning_content"]; exists {
		t.Fatalf("native Chat input must remain untouched: %#v", messages[2])
	}
}
