package main

import (
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// restoreTestPlan 手工构造还原计划，测试不依赖全局配置状态。
func restoreTestPlan(rules ...identityRestoreRule) identityRestore {
	maxFrom := 0
	for _, rule := range rules {
		if len(rule.from) > maxFrom {
			maxFrom = len(rule.from)
		}
	}
	hold := maxFrom
	plan := identityRestore{rules: rules, hold: hold}
	for _, rule := range rules {
		if rule.from != "" {
			plan.firstBytes[rule.from[0]] = true
		}
	}
	return plan
}

func standardRestorePlan() identityRestore {
	return restoreTestPlan(
		identityRestoreRule{from: "WorkBuddy", to: "Claude Code", exactName: true},
	)
}

func requestWithPlan(plan identityRestore) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return r.WithContext(context.WithValue(r.Context(), identityRestoreContextKey{}, plan))
}

// enableRestore 临时打开全局身份还原配置，测试结束自动复原。
// 复原使用默认配置（其他 identity 测试同样以 setIdentityConfig(runtimeIdentityConfig{}) 收尾），
// 避免把测试中新的还原目标泄漏到后续用例。
func enableRestore(t *testing.T, replacements map[string]string) {
	t.Helper()
	if replacements == nil {
		replacements = map[string]string{"Claude Code": "WorkBuddy"}
	}
	enabled := true
	setIdentityConfig(runtimeIdentityConfig{
		Enabled: &enabled, Target: "WorkBuddy", Replacements: replacements, ResponseRestore: &enabled,
	})
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
}

// 单元测试直接验证计划本身，不经过 HTTP 链路：只有被改写成 target 的客户端名
// 才导出身份名对照；拼写修正、包含 target 的组合替换与通配规则都不参与。
func TestIdentityRestoreRulesDerivedFromReplacements(t *testing.T) {
	rules := buildIdentityRestoreRules("WorkBuddy", []identityReplacement{
		{Old: "Claude Code", New: "WorkBuddy"},
		{Old: "Grok", New: "WorkBuddy"},
		{Old: "workspace root", New: "workspace base"},
		{Old: "are paths the user", New: "are paths that the user"},
		{Old: "Kimi", New: "Kimi (WorkBuddy)"},
		{Old: "You are(.*)released by xAI", New: "WorkBuddy", Wildcard: true, Prefix: "You are", Suffix: "released by xAI"},
	})
	want := map[identityRestoreRule]bool{
		{from: "Claude Code", to: "WorkBuddy", exactName: true}: true,
		{from: "Grok", to: "WorkBuddy", exactName: true}:        true,
	}
	if len(rules) != len(want) {
		t.Fatalf("rules = %#v", rules)
	}
	for _, rule := range rules {
		if !want[rule] {
			t.Fatalf("unexpected rule %#v", rule)
		}
	}
	// 去重：同一客户端名只保留首条规则。
	deduped := buildIdentityRestoreRules("WorkBuddy", []identityReplacement{
		{Old: "Claude Code", New: "WorkBuddy"}, {Old: "Claude Code", New: "WorkBuddy"},
	})
	if len(deduped) != 1 || deduped[0].from != "Claude Code" {
		t.Fatalf("dedupe = %#v", deduped)
	}
}

func TestRestoreIdentityTextExactCaseAndBoundaries(t *testing.T) {
	plan := standardRestorePlan()
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "I am WorkBuddy.", "I am Claude Code."},
		{"case_sensitive", "workbuddy and WORKBUDDY stay", "workbuddy and WORKBUDDY stay"},
		{"suffix_digit", "WorkBuddy2 stays", "WorkBuddy2 stays"},
		{"prefix_letter", "myWorkBuddy stays", "myWorkBuddy stays"},
		{"prefix_underscore", "_WorkBuddy stays", "_WorkBuddy stays"},
		{"suffix_underscore", "WorkBuddy_id stays", "WorkBuddy_id stays"},
		{"cjk_neighbours", "我是WorkBuddy助手", "我是Claude Code助手"},
		{"emoji_neighbours", "🤖WorkBuddy🤖", "🤖Claude Code🤖"},
		{"punct_neighbours", "(WorkBuddy)", "(Claude Code)"},
		{"multi", "WorkBuddy and WorkBuddy", "Claude Code and Claude Code"},
	} {
		if got := restoreIdentityText(plan.rules, tc.in); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// restoreIdentityTextForTest 是独立于生产代码的参考实现：逐字节扫描，按边界规则
// 就地替换。用于校验流式实现与一次性结果的等价性。
func restoreIdentityTextForTest(rules []identityRestoreRule, text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		matched := ""
		for _, rule := range rules {
			if !strings.HasPrefix(text[i:], rule.from) {
				continue
			}
			end := i + len(rule.from)
			if i > 0 && isIdentifierByteForTest(text[i-1]) {
				continue
			}
			if end < len(text) && isIdentifierByteForTest(text[end]) {
				continue
			}
			if len(rule.from) > len(matched) {
				matched = rule.from
			}
		}
		if matched == "" {
			b.WriteByte(text[i])
			i++
			continue
		}
		for _, rule := range rules {
			if rule.from == matched {
				b.WriteString(rule.to)
				break
			}
		}
		i += len(matched)
	}
	return b.String()
}

func isIdentifierByteForTest(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// 任意分片切分（含多字节字符中间）都必须与一次性还原整段文本一致。
// 期望值取自独立参考实现，避免生产代码自己验证自己。
func TestIdentityRestorerMatchesWholeTextForEverySplit(t *testing.T) {
	plan := standardRestorePlan()
	inputs := []string{
		"prefix WorkBuddy suffix",
		"WorkBuddy WorkBuddy",
		"中文WorkBuddy中文，😀WorkBuddy😀",
		"tail partial WorkBu",
		"ends with Work",
		"nothing to restore",
		"workspace base and WorkBuddy",
		"workspace base WorkBuddy😀workspace baseworkspace basebase\nworkspace base2WorkWorkClaude Codebase",
		"WorkBuddy and WorkBuddy_2, but not WorkBuddy.",
	}
	for _, input := range inputs {
		want := restoreIdentityTextForTest(plan.rules, input)
		if got := restoreIdentityText(plan.rules, input); got != want {
			t.Fatalf("whole text %q: got %q want %q", input, got, want)
		}
		for cut := 0; cut <= len(input); cut++ {
			restorer := newIdentityRestorer(plan)
			out := restorer.Write(input[:cut]) + restorer.Write(input[cut:]) + restorer.Flush()
			if out != want {
				t.Fatalf("input %q cut %d: got %q want %q", input, cut, out, want)
			}
			for cut2 := cut; cut2 <= len(input); cut2++ {
				restorer := newIdentityRestorer(plan)
				out := restorer.Write(input[:cut]) + restorer.Write(input[cut:cut2]) + restorer.Write(input[cut2:]) + restorer.Flush()
				if out != want {
					t.Fatalf("input %q cuts %d,%d: got %q want %q", input, cut, cut2, out, want)
				}
			}
		}
	}
}

func TestIdentityRestorerBoundsCarry(t *testing.T) {
	plan := standardRestorePlan()
	restorer := newIdentityRestorer(plan)
	// 无候选时只保留最后一个 rune 作为下一片的边界上下文。
	if got := restorer.Write("plain text "); got != "plain text" {
		t.Fatalf("plain text was held: %q", got)
	}
	// "Work" 只是完整目标的前缀：无法判定，必须扣留到下一分片。
	if got := restorer.Write("see Work"); got != " see" {
		t.Fatalf("prefix of target must be held, got %q", got)
	}
	if got := restorer.Write("Buddy"); got != "" {
		t.Fatalf("complete target needs one lookahead byte; got %q", got)
	}
	if got := restorer.Flush(); got != " Claude Code" {
		t.Fatalf("flush completed target = %q", got)
	}
	if len(restorer.carry) != 0 {
		t.Fatalf("carry not drained: %q", restorer.carry)
	}
	tail := newIdentityRestorer(plan)
	if out := tail.Write(strings.Repeat("a", identityRestoreMaxTailBytes*2)); len(out) != identityRestoreMaxTailBytes*2-1 {
		t.Fatalf("long body was buffered: %d", len(out))
	}
}

func TestIdentityRestorerDoesNotHoldResolvedTarget(t *testing.T) {
	plan := standardRestorePlan()
	restorer := newIdentityRestorer(plan)
	first := restorer.Write("Hello WorkBuddy world")
	if !strings.Contains(first, "Claude Code") {
		t.Fatalf("resolved target was held: %q", first)
	}
	if got := first + restorer.Flush(); got != "Hello Claude Code world" {
		t.Fatalf("resolved stream = %q", got)
	}
}

func TestIdentityRestorerCandidateTailStaysBounded(t *testing.T) {
	plan := standardRestorePlan()
	restorer := newIdentityRestorer(plan)
	out := restorer.Write(strings.Repeat("a", 10000) + "Work")
	if len(out) < 9990 {
		t.Fatalf("candidate caused an unbounded hold: emitted=%d", len(out))
	}
	if got := out + restorer.Flush(); got != strings.Repeat("a", 10000)+"Work" {
		t.Fatalf("candidate tail changed text")
	}
}

func TestIdentityRestorerDoesNotSplitUTF8AtCarryBoundary(t *testing.T) {
	plan := standardRestorePlan()
	restorer := newIdentityRestorer(plan)
	first := restorer.Write("中Work")
	if !utf8.ValidString(first) {
		t.Fatalf("stream emitted invalid UTF-8 boundary: %q", first)
	}
	second := restorer.Write("Buddy")
	if !utf8.ValidString(second) {
		t.Fatalf("second stream chunk emitted invalid UTF-8 boundary: %q", second)
	}
	if got := first + second + restorer.Flush(); got != "中Claude Code" {
		t.Fatalf("UTF-8 boundary changed text: %q", got)
	}
}

func TestChatRestorersAreIsolatedByChoice(t *testing.T) {
	plan := standardRestorePlan()
	restorers := map[chatRestoreKey]*identityRestorer{}
	var choice0, choice1 strings.Builder
	for _, raw := range []string{
		`{"choices":[{"index":0,"delta":{"content":"A Work"}},{"index":1,"delta":{"content":"B Work"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Buddy!"}},{"index":1,"delta":{"content":"Buddy!"}}]}`,
	} {
		obj := decodeObject(t, []byte(cleanChunkObject(raw, plan, restorers)))
		for _, rawChoice := range obj["choices"].([]any) {
			choice := rawChoice.(map[string]any)
			delta := choice["delta"].(map[string]any)
			text, _ := delta["content"].(string)
			switch intFloat(choice["index"]) {
			case 0:
				choice0.WriteString(text)
			case 1:
				choice1.WriteString(text)
			}
		}
	}
	choice0.WriteString(restorers[chatRestoreKeyFor(0, "content")].Flush())
	choice1.WriteString(restorers[chatRestoreKeyFor(1, "content")].Flush())
	if choice0.String() != "A Claude Code!" || choice1.String() != "B Claude Code!" {
		t.Fatalf("choice streams crossed: choice0=%q choice1=%q", choice0.String(), choice1.String())
	}
	tail := decodeObject(t, []byte(tailChunkJSON(1, "content", "!")))
	if tail["choices"].([]any)[0].(map[string]any)["index"] != float64(1) {
		t.Fatalf("tail chunk lost choice index: %#v", tail)
	}
}

func TestMessagesStreamingFlushesThinkingTailBeforeSignature(t *testing.T) {
	state := newRestoringAnthropicStreamState("m", true, standardRestorePlan())
	state.feed(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"reasoning_content": "I am Work"},
	}}})
	state.feed(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"reasoning_content": "Buddy"},
	}}})
	out := state.finishEvents()
	if got := state.thinking.buf.String(); got != "I am Claude Code" {
		t.Fatalf("thinking tail was not restored: %q", got)
	}
	if !strings.Contains(string(out), `"type":"signature_delta"`) {
		t.Fatalf("signature event missing: %s", out)
	}
}

func TestChatTailDeltaPrecedesFinishFrame(t *testing.T) {
	plan := standardRestorePlan()
	restorers := map[chatRestoreKey]*identityRestorer{}
	writer := newLockedResponseWriter()
	guard := &streamWriteGuard{w: writer, flusher: writer}
	for _, raw := range []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"A Work"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"Buddy"},"finish_reason":"stop"}]}`,
	} {
		cleaned := cleanChunkObject(raw, plan, restorers)
		writeRestoredChatChunk(guard, cleaned, decodeObject(t, []byte(raw)), restorers)
	}
	frames := sseDataObjects(t, writer.String())
	var streamed strings.Builder
	contentIndex, tailIndex, finishIndex := -1, -1, -1
	for i, frame := range frames {
		choices, _ := frame["choices"].([]any)
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			if finish := strOf(choice["finish_reason"]); finish != "" {
				finishIndex = i
			}
			delta, _ := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				streamed.WriteString(text)
				if contentIndex < 0 && text == "A" {
					contentIndex = i
				}
				if text == " Claude Code" {
					tailIndex = i
				}
			}
		}
	}
	if streamed.String() != "A Claude Code" {
		t.Fatalf("stream content order = %q", streamed.String())
	}
	if contentIndex < 0 || tailIndex < 0 || finishIndex < 0 ||
		contentIndex >= tailIndex || tailIndex >= finishIndex {
		t.Fatalf("content/tail/finish order: content=%d tail=%d finish=%d frames=%s", contentIndex, tailIndex, finishIndex, writer.String())
	}
}

func TestMessagesThinkingEventOrder(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"I am Work"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"Buddy"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	rec := httptest.NewRecorder()
	req := requestWithPlan(standardRestorePlan())
	req.URL.Path = "/v1/messages"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
	streamMessagesResponse(rec, req, resp, "m", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now(), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := sseDataObjects(t, rec.Body.String())
	thinkingIndex := -1
	lastThinkingDelta, signature, thinkingStop := -1, -1, -1
	var thinking strings.Builder
	for i, event := range events {
		switch event["type"] {
		case "content_block_start":
			block, _ := event["content_block"].(map[string]any)
			if block["type"] == "thinking" {
				thinkingIndex = intFloat(event["index"])
			}
		case "content_block_delta":
			delta, _ := event["delta"].(map[string]any)
			if delta["type"] == "thinking_delta" {
				lastThinkingDelta = i
				thinking.WriteString(strOf(delta["thinking"]))
			}
			if delta["type"] == "signature_delta" {
				signature = i
			}
		case "content_block_stop":
			if intFloat(event["index"]) == thinkingIndex {
				thinkingStop = i
			}
		}
	}
	if thinking.String() != "I am Claude Code" {
		t.Fatalf("thinking deltas=%q", thinking.String())
	}
	if lastThinkingDelta < 0 || signature < 0 || thinkingStop < 0 ||
		lastThinkingDelta > signature || signature > thinkingStop {
		t.Fatalf("thinking order: delta=%d signature=%d stop=%d", lastThinkingDelta, signature, thinkingStop)
	}
}

func TestResponsesStreamingRestoresBothChannelsAndTailDeltas(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"I am Work"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Answer Work"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"Buddy"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Buddy"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	rec := httptest.NewRecorder()
	req := requestWithPlan(standardRestorePlan())
	req.URL.Path = "/v1/responses"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
	streamResponsesResponse(rec, req, resp, "m", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := sseDataObjects(t, rec.Body.String())
	var reasoningDelta, textDelta, reasoningDone, textDone strings.Builder
	lastReasoningDelta, lastTextDelta := -1, -1
	reasoningDoneIndex, textDoneIndex, completedIndex := -1, -1, -1
	completed := false
	for i, event := range events {
		switch event["type"] {
		case "response.reasoning_summary_text.delta":
			reasoningDelta.WriteString(strOf(event["delta"]))
			lastReasoningDelta = i
		case "response.output_text.delta":
			textDelta.WriteString(strOf(event["delta"]))
			lastTextDelta = i
		case "response.reasoning_summary_text.done":
			reasoningDone.WriteString(strOf(event["text"]))
			reasoningDoneIndex = i
		case "response.output_text.done":
			textDone.WriteString(strOf(event["text"]))
			textDoneIndex = i
		case "response.completed":
			completed = true
			completedIndex = i
		case "response.failed":
			t.Fatalf("unexpected response.failed: %s", rec.Body.String())
		}
	}
	if reasoningDelta.String() != reasoningDone.String() || reasoningDone.String() != "I am Claude Code" {
		t.Fatalf("reasoning delta=%q done=%q", reasoningDelta.String(), reasoningDone.String())
	}
	if textDelta.String() != textDone.String() || textDone.String() != "Answer Claude Code" {
		t.Fatalf("text delta=%q done=%q", textDelta.String(), textDone.String())
	}
	if !completed || lastReasoningDelta < 0 || reasoningDoneIndex < lastReasoningDelta ||
		lastTextDelta < 0 || textDoneIndex < lastTextDelta || completedIndex < reasoningDoneIndex || completedIndex < textDoneIndex {
		t.Fatalf("response.completed missing: %s", rec.Body.String())
	}
}

func TestIncompleteRestoreStreamsDoNotFlushHeldTails(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		writer := newLockedResponseWriter()
		req := requestWithPlan(standardRestorePlan())
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(
			`data: {"choices":[{"index":0,"delta":{"content":"Hello WorkBuddy"},"finish_reason":null}]}` + "\n\n",
		))}
		streamChatResponseWithReplay(writer, req, resp, "m", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now(), nil)
		out := writer.String()
		if strings.Contains(out, "Claude Code") || strings.Contains(out, "[DONE]") ||
			!strings.Contains(out, `"code":"stream_closed_without_finish"`) {
			t.Fatalf("incomplete chat response changed meaning: %s", out)
		}
	})
	t.Run("responses", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := requestWithPlan(standardRestorePlan())
		req.URL.Path = "/v1/responses"
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(
			`data: {"choices":[{"delta":{"reasoning_content":"I am WorkBuddy"}}]}` + "\n\n",
		))}
		streamResponsesResponse(rec, req, resp, "m", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now())
		out := rec.Body.String()
		if strings.Contains(out, "Claude Code") || strings.Contains(out, "response.completed") ||
			!strings.Contains(out, "response.failed") {
			t.Fatalf("incomplete responses stream changed meaning: %s", out)
		}
	})
	t.Run("chat interrupted", func(t *testing.T) {
		writer := newLockedResponseWriter()
		req := requestWithPlan(standardRestorePlan())
		resp := &http.Response{Body: io.NopCloser(io.MultiReader(
			strings.NewReader(`data: {"choices":[{"index":0,"delta":{"content":"Hello WorkBuddy"},"finish_reason":null}]}`+"\n\n"),
			messagesFailReader{},
		))}
		streamChatResponseWithReplay(writer, req, resp, "m", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now(), nil)
		out := writer.String()
		if strings.Contains(out, "Claude Code") || strings.Contains(out, "[DONE]") ||
			!strings.Contains(out, `"code":"stream_interrupted"`) {
			t.Fatalf("interrupted chat response changed meaning: %s", out)
		}
	})
	t.Run("responses interrupted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := requestWithPlan(standardRestorePlan())
		req.URL.Path = "/v1/responses"
		resp := &http.Response{Body: io.NopCloser(io.MultiReader(
			strings.NewReader(`data: {"choices":[{"delta":{"reasoning_content":"I am WorkBuddy"}}]}`+"\n\n"),
			messagesFailReader{},
		))}
		streamResponsesResponse(rec, req, resp, "m", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now())
		out := rec.Body.String()
		if strings.Contains(out, "Claude Code") || strings.Contains(out, "response.completed") ||
			!strings.Contains(out, "response.failed") || !strings.Contains(out, "stream_interrupted") {
			t.Fatalf("interrupted responses stream changed meaning: %s", out)
		}
	})
}

func sseDataObjects(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range strings.Split(raw, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if !json.Valid([]byte(data)) {
				continue
			}
			out = append(out, decodeObject(t, []byte(data)))
			break
		}
	}
	return out
}

func TestIdentityRestoreDisabledPlanPassesThrough(t *testing.T) {
	restorer := newIdentityRestorer(identityRestore{})
	if got := restorer.Write("WorkBu"); got != "WorkBu" {
		t.Fatalf("inactive plan modified delta: %q", got)
	}
	if got := restoreIdentityText(nil, "WorkBuddy"); got != "WorkBuddy" {
		t.Fatalf("inactive plan modified text: %q", got)
	}
}

// 随机性质测试：固定种子 20261104、3000 组用例，任意切分下的流式输出
// 必须与一次性还原整段文本完全一致，且始终保持合法 UTF-8。
func TestIdentityRestoreStreamingEqualsAggregate(t *testing.T) {
	const seed, cases = 20261104, 3000
	plan := restoreTestPlan(
		identityRestoreRule{from: "WorkBuddy", to: "Claude Code"},
		identityRestoreRule{from: "WorkBuddy", to: "Codex"},
		identityRestoreRule{from: "WorkBuddy", to: "B"},
	)
	pieces := []string{
		"WorkBuddy", "workspace base", "Work", "Bu", "ddy", "Buddy", "base", "space",
		"中文", "😀", "🙂‍↔️", "e\u0301", " ", "\n", "a", "_", "2", ".", "Claude Code", "",
	}
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < cases; i++ {
		var b strings.Builder
		for n := rng.Intn(14); n >= 0; n-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		input := b.String()
		want := restoreIdentityTextForTest(plan.rules, input)
		var cuts []int
		if len(input) > 0 {
			for n := rng.Intn(6); n >= 0; n-- {
				cuts = append(cuts, rng.Intn(len(input)+1))
			}
		}
		sortInts(cuts)
		restorer := newIdentityRestorer(plan)
		var out strings.Builder
		last := 0
		for _, cut := range cuts {
			out.WriteString(restorer.Write(input[last:cut]))
			last = cut
		}
		out.WriteString(restorer.Write(input[last:]))
		out.WriteString(restorer.Flush())
		if out.String() != want {
			t.Fatalf("case %d input=%q cuts=%v: stream=%q want=%q", i, input, cuts, out.String(), want)
		}
		if !utf8.ValidString(out.String()) {
			t.Fatalf("case %d produced invalid UTF-8: %q", i, out.String())
		}
	}
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// 客户端身份只从原始入站请求的 instructions/system/developer 位置推断。
// 只有被改写过的身份名（identity.replacements 的值为 target）才算证据；
// 客户端提示词本来就会出现的 WorkBuddy 产品名不能反推客户端身份。
func TestOriginalClientIdentitiesSources(t *testing.T) {
	enableRestore(t, map[string]string{"Claude Code": "WorkBuddy", "Codex": "WorkBuddy", "Grok": "WorkBuddy"})
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"chat_system", `{"messages":[{"role":"system","content":"You are Claude Code, WorkBuddy."}]}`, []string{"Claude Code"}},
		{"chat_developer_blocks", `{"messages":[{"role":"developer","content":[{"type":"text","text":"I am Codex for WorkBuddy."}]}]}`, []string{"Codex"}},
		{"responses_instructions", `{"instructions":"You are Grok, the WorkBuddy runtime.","input":"hi"}`, []string{"Grok"}},
		{"messages_system", `{"system":[{"type":"text","text":"You are Codex. Running WorkBuddy."}],"messages":[]}`, []string{"Codex"}},
		{"plain_target_is_not_identity", `{"instructions":"You are WorkBuddy."}`, nil},
		{"user_ignored", `{"messages":[{"role":"user","content":"I am Claude Code"}]}`, nil},
		{"assistant_ignored", `{"messages":[{"role":"assistant","content":"I am Claude Code"}]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := originalClientIdentities(decodeObject(t, []byte(tc.body)))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("identities = %#v want %#v", got, tc.want)
			}
		})
	}
}

// 同一行出现多个身份名时不作为身份原文，避免把上游注入的身份句当成客户端身份。
func TestOriginalClientIdentitiesSkipsAmbiguousLines(t *testing.T) {
	enableRestore(t, map[string]string{"Claude Code": "WorkBuddy", "Codex": "WorkBuddy"})
	found := originalClientIdentities(map[string]any{
		"instructions": "Claude Code or Codex? Either way you are WorkBuddy.",
	})
	if len(found) != 0 {
		t.Fatalf("ambiguous line produced identities: %#v", found)
	}
}

// 计划只从入站原始请求推断一次；客户端身份直接取自 identity.replacements。
func TestPlanIdentityRestoreUsesConfiguredClientIdentity(t *testing.T) {
	enableRestore(t, map[string]string{"Claude Code": "WorkBuddy"})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	planIdentityRestore(r, decodeObject(t, []byte(`{"messages":[{"role":"system","content":"You are Claude Code."}]}`)))
	plan := identityRestoreFor(r)
	if !plan.active() || len(plan.rules) != 1 || plan.rules[0].from != "WorkBuddy" || plan.rules[0].to != "Claude Code" {
		t.Fatalf("plan = %#v", plan)
	}
	if got := restoreIdentityText(plan.rules, "I am WorkBuddy."); got != "I am Claude Code." {
		t.Fatalf("restored=%q", got)
	}
}

// 未开启 responseRestore 时不生成计划；没有命中身份的请求同样保持空计划。
func TestPlanIdentityRestoreDefaultOff(t *testing.T) {
	enableRestore(t, map[string]string{"Claude Code": "WorkBuddy"})
	enabled := true
	setIdentityConfig(runtimeIdentityConfig{Enabled: &enabled, Target: "WorkBuddy",
		Replacements: map[string]string{"Claude Code": "WorkBuddy"}})
	disabled := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	planIdentityRestore(disabled, decodeObject(t, []byte(`{"instructions":"You are Claude Code."}`)))
	if identityRestoreFor(disabled).active() {
		t.Fatal("disabled responseRestore produced a plan")
	}
	setIdentityConfig(runtimeIdentityConfig{Enabled: &enabled, Target: "WorkBuddy",
		Replacements: map[string]string{"Claude Code": "WorkBuddy"}, ResponseRestore: &enabled})
	missing := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	planIdentityRestore(missing, decodeObject(t, []byte(`{"instructions":"You are a helpful assistant."}`)))
	if identityRestoreFor(missing).active() {
		t.Fatal("request without client identity produced a plan")
	}
}

func TestRestoreChatCompletionIdentityChannels(t *testing.T) {
	r := requestWithPlan(standardRestorePlan())
	raw := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"WorkBuddy here","reasoning_content":"WorkBuddy thinks","tool_calls":[{"id":"call_1","type":"function","function":{"name":"WorkBuddy","arguments":"{\"a\":\"WorkBuddy\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1}}`)
	out := decodeObject(t, restoreChatCompletionIdentity(r, raw))
	message := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "Claude Code here" || message["reasoning_content"] != "Claude Code thinks" {
		t.Fatalf("channels not restored: %#v", message)
	}
	call := message["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if call["name"] != "WorkBuddy" || call["arguments"] != `{"a":"WorkBuddy"}` {
		t.Fatalf("tool call was modified: %#v", call)
	}
	if out["usage"].(map[string]any)["prompt_tokens"] != float64(1) {
		t.Fatalf("usage was modified: %#v", out["usage"])
	}
	// 未开启还原时返回完全相同的字节。
	if untouched := restoreChatCompletionIdentity(httptest.NewRequest(http.MethodPost, "/", nil), raw); string(untouched) != string(raw) {
		t.Fatal("disabled restore changed the aggregate body")
	}
}

func TestRestoreResponsesIdentityParts(t *testing.T) {
	r := requestWithPlan(standardRestorePlan())
	raw, err := json.Marshal(map[string]any{
		"id": "resp_1", "object": "response", "status": "completed",
		"output": []any{
			map[string]any{"id": "rs_1", "type": "reasoning", "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": "WorkBuddy reasons"}}},
			map[string]any{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "WorkBuddy answers", "annotations": []any{}}}},
			map[string]any{"id": "fc_1", "type": "function_call", "status": "completed",
				"call_id": "call_1", "name": "WorkBuddy", "arguments": `{"x":"WorkBuddy"}`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeObject(t, restoreResponsesIdentity(r, raw))
	output := out["output"].([]any)
	reasoning := output[0].(map[string]any)["summary"].([]any)[0].(map[string]any)
	message := output[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	call := output[2].(map[string]any)
	if reasoning["text"] != "Claude Code reasons" || message["text"] != "Claude Code answers" {
		t.Fatalf("text parts not restored: %#v %#v", reasoning, message)
	}
	if call["name"] != "WorkBuddy" || call["arguments"] != `{"x":"WorkBuddy"}` {
		t.Fatalf("function call was modified: %#v", call)
	}
}
