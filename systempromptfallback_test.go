package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestFallbackDrivenBehaviorTable 固定简化后的唯一语义表：
//
//	客户端 system | fallback | 发往上游的 system
//	有            | 非空     | fallback（覆盖客户端，客户端原文丢弃）
//	无            | 非空     | fallback（保底注入）
//	有            | 空/空白  | 客户端原 system（透传）
//	无            | 空/空白  | 内置兜底（defaultSystemPrompt）
//
// 覆盖 Chat / Responses / Anthropic Messages 三个入口，每个入口都跑满四种组合。
func TestFallbackDrivenBehaviorTable(t *testing.T) {
	const fallback = "【网关人设】"
	const client = "【客户端自己的系统提示词】"
	for _, tc := range []struct {
		name  string
		fb    string
		route string
		body  string
		want  string
	}{
		// 三入口 × fallback 非空 × 客户端自带 system：一律覆盖
		{
			"chat_有system_非空_覆盖", fallback,
			"/v1/chat/completions",
			`{"model":"m","stream":true,"messages":[{"role":"system","content":"` + client + `"},{"role":"user","content":"hi"}]}`,
			fallback,
		},
		{
			"responses_有instructions_非空_覆盖", fallback,
			"/v1/responses",
			`{"model":"m","stream":true,"instructions":"` + client + `","input":"hi"}`,
			fallback,
		},
		{
			"messages_有system_非空_覆盖", fallback,
			"/v1/messages",
			`{"model":"m","stream":true,"system":"` + client + `","messages":[{"role":"user","content":"hi"}]}`,
			fallback,
		},
		{
			"messages_内容块system_非空_覆盖", fallback,
			"/v1/messages",
			`{"model":"m","stream":true,"system":[{"type":"text","text":"规则一"},{"type":"text","text":"规则二"}],"messages":[{"role":"user","content":"hi"}]}`,
			fallback,
		},
		// 三入口 × fallback 非空 × 客户端没给 system：保底注入同一段 fallback
		{
			"chat_无system_非空_保底", fallback,
			"/v1/chat/completions",
			`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			fallback,
		},
		{
			"responses_无instructions_非空_保底", fallback,
			"/v1/responses",
			`{"model":"m","stream":true,"input":"hi"}`,
			fallback,
		},
		{
			"messages_无system_非空_保底", fallback,
			"/v1/messages",
			`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			fallback,
		},
		// 三入口 × fallback 空（含纯空白）× 客户端自带 system：透传
		{
			"chat_有system_空_透传", "",
			"/v1/chat/completions",
			`{"model":"m","stream":true,"messages":[{"role":"system","content":"` + client + `"},{"role":"user","content":"hi"}]}`,
			client,
		},
		{
			"chat_有system_空白_透传", "  \n\t ", // 只有空白的 fallback 等同未配置
			"/v1/chat/completions",
			`{"model":"m","stream":true,"messages":[{"role":"system","content":"` + client + `"},{"role":"user","content":"hi"}]}`,
			client,
		},
		{
			"responses_有instructions_空_透传", "",
			"/v1/responses",
			`{"model":"m","stream":true,"instructions":"` + client + `","input":"hi"}`,
			client,
		},
		{
			"messages_有system_空_透传", "",
			"/v1/messages",
			`{"model":"m","stream":true,"system":"` + client + `","messages":[{"role":"user","content":"hi"}]}`,
			client,
		},
		{
			"messages_内容块system_空_透传", "",
			"/v1/messages",
			`{"model":"m","stream":true,"system":[{"type":"text","text":"规则一"},{"type":"text","text":"规则二"}],"messages":[{"role":"user","content":"hi"}]}`,
			"规则一\n规则二",
		},
		// 三入口 × fallback 空 × 客户端没给 system：内置兜底
		{
			"chat_无system_空_内置兜底", "",
			"/v1/chat/completions",
			`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			defaultSystemPrompt,
		},
		{
			"responses_无instructions_空_内置兜底", "",
			"/v1/responses",
			`{"model":"m","stream":true,"input":"hi"}`,
			defaultSystemPrompt,
		},
		{
			"messages_无system_空_内置兜底", "",
			"/v1/messages",
			`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			defaultSystemPrompt,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSystemPrompts(t, tc.fb, "")
			messages := captureUpstreamMessages(t, tc.route, tc.body)
			first, ok := messages[0].(map[string]any)
			if !ok {
				t.Fatalf("首条消息结构异常: %#v", messages[0])
			}
			if role := first["role"]; role != "system" {
				t.Fatalf("首条必须是 system: %#v", first)
			}
			got, _ := systemContentText(first["content"])
			if got != tc.want {
				t.Fatalf("上游 system 正文不符: got=%q want=%q", got, tc.want)
			}
			if got := messages[1].(map[string]any)["content"]; got != "hi" {
				t.Fatalf("user 内容不应被改写: %#v", got)
			}
			if strings.Contains(got, client) && tc.want != client {
				t.Fatalf("覆盖场景不得残留客户端原文: %q", got)
			}
		})
	}
}

// TestFallbackReplacesContentBlockArrayShape 固定「内容块数组也直接替换」并保留数组形态。
func TestFallbackReplacesContentBlockArrayShape(t *testing.T) {
	const fallback = "【网关人设】"
	withSystemPrompts(t, fallback, "")
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "客户端块一"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			map[string]any{"type": "text", "text": "客户端块二"},
		}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 11, "trace-blocks")
	blocks, ok := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("覆盖后应保留内容块数组形态: %#v", obj["messages"])
	}
	if len(blocks) != 1 {
		t.Fatalf("覆盖后应只保留承载 fallback 的文本块: %#v", blocks)
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != fallback {
		t.Fatalf("文本块内容应为 fallback: %#v", block)
	}
}

// TestFallbackCombinesWithForce force 后置追加、fallback 覆盖，两者各司其职。
func TestFallbackCombinesWithForce(t *testing.T) {
	const fallback = "【网关人设】"
	const force = "【强制后置】"
	withSystemPrompts(t, fallback, force)

	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "【客户端自己的系统提示词】"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 12, "trace-force")
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != fallback+"\n\n"+force {
		t.Fatalf("覆盖后 force 仍应后置: %#v", got)
	}
	if strings.Contains(obj["messages"].([]any)[0].(map[string]any)["content"].(string), "【客户端自己的系统提示词】") {
		t.Fatal("覆盖场景下客户端原文必须被丢弃")
	}
	// 客户端无 system 时走保底注入，force 同样跟在 fallback 之后。
	obj = map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/responses", nil), 13, "trace-force-2")
	parts, ok := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !ok || parts != fallback+"\n\n"+force {
		t.Fatalf("无 system 时保底注入与 force 组合异常: %#v", obj["messages"])
	}
}

// TestInjectedFallbackIsNotReportedAsOverride 固定审计语义：
// 客户端完全不带 system 时，首条 system 是网关注入的保底（内容本身就是 fallback），
// 不应记成"覆盖了客户端 system"，也不应写 client_system_body。
func TestInjectedFallbackIsNotReportedAsOverride(t *testing.T) {
	var output bytes.Buffer
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = &debugJSONSink{writer: &output}
	debugSinkMu.Unlock()
	oldEnabled, oldLogBody := cfg.DebugEnabled, cfg.DebugLogSystemPrompt
	t.Cleanup(func() {
		cfg.DebugEnabled, cfg.DebugLogSystemPrompt = oldEnabled, oldLogBody
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})
	cfg.DebugEnabled = true
	cfg.DebugLogSystemPrompt = true
	const fallback = "【网关人设】"
	withSystemPrompts(t, fallback, "")

	obj := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(req, "injected-trace", time.Now())
	prepareSystemPromptForUpstream(obj, req, 31, "injected-trace")

	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != fallback {
		t.Fatalf("保底内容应为 fallback: %#v", got)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["fallback_source"] != "配置保底" || record["override_applied"] != false || record["prompt_mode"] != "覆盖" {
		t.Fatalf("注入保底的模式应为覆盖、且不记为覆盖客户端: %#v", record)
	}
	if _, ok := record["client_system_body"]; ok {
		t.Fatalf("客户端没给 system 时不得记录 client_system_body: %#v", record)
	}
	if record["final_system_body"] != fallback {
		t.Fatalf("final_system_body 应为 fallback: %#v", record["final_system_body"])
	}
	// 客户端自带 system 时必须记为覆盖（同一轮请求的对照）。
	output.Reset()
	obj = map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "【客户端自己的系统提示词】"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(req, "override-trace", time.Now())
	prepareSystemPromptForUpstream(obj, req, 32, "override-trace")
	record = map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["fallback_source"] != "配置覆盖" || record["override_applied"] != true || record["prompt_mode"] != "覆盖" {
		t.Fatalf("客户端自带 system 时必须记为覆盖: %#v", record)
	}
	if record["client_system_body"] != "【客户端自己的系统提示词】" {
		t.Fatalf("client_system_body 应为客户端原文: %#v", record["client_system_body"])
	}
	// 空 fallback 时记为透传。
	output.Reset()
	withSystemPrompts(t, "", "")
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(req, "passthrough-trace", time.Now())
	obj = map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "【客户端自己的系统提示词】"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	prepareSystemPromptForUpstream(obj, req, 33, "passthrough-trace")
	record = map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["fallback_source"] != "客户端透传" || record["override_applied"] != false || record["prompt_mode"] != "透传" {
		t.Fatalf("透传场景审计字段不符: %#v", record)
	}
	// fallback 为空 + 客户端没给 system：内置兜底，记为保底。
	output.Reset()
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(req, "builtin-trace", time.Now())
	obj = map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prepareSystemPromptForUpstream(obj, req, 34, "builtin-trace")
	record = map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["fallback_source"] != "内置保底" || record["override_applied"] != false || record["prompt_mode"] != "保底" {
		t.Fatalf("内置兜底场景应记为保底: %#v", record)
	}
	if record["final_system_body"] != defaultSystemPrompt {
		t.Fatalf("内置兜底正文不符: %#v", record["final_system_body"])
	}
}

// TestUnsupportedSystemContentKindIsDisambiguated 锁定"system 内容形态"在日志里可分辨：
// 内容不是字符串/内容块数组时，final_system_chars 为 0，必须靠 final_system_kind 区分
// "取不到正文"与"没有 system"，同时不得静默改写客户端数据。
func TestUnsupportedSystemContentKindIsDisambiguated(t *testing.T) {
	var output bytes.Buffer
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = &debugJSONSink{writer: &output}
	debugSinkMu.Unlock()
	oldEnabled, oldLogBody := cfg.DebugEnabled, cfg.DebugLogSystemPrompt
	t.Cleanup(func() {
		cfg.DebugEnabled, cfg.DebugLogSystemPrompt = oldEnabled, oldLogBody
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})
	cfg.DebugEnabled = true
	cfg.DebugLogSystemPrompt = true
	// fallback 为空 → 不覆盖，原始内容形态得以保留；force 非空 → 应跳过强制后置。
	withSystemPrompts(t, "", "【强制后置】")

	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": 12345},
		map[string]any{"role": "user", "content": "hi"},
	}}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(req, "kind-trace", time.Now())
	prepareSystemPromptForUpstream(obj, req, 35, "kind-trace")

	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != 12345 {
		t.Fatalf("非法的 system 内容不应被静默改写: %#v", got)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["final_system_kind"] != "未支持的内容类型" || record["final_system_chars"].(float64) != 0 {
		t.Fatalf("内容形态应可分辨: %#v", record)
	}
	if record["prompt_mode"] != "透传" || record["fallback_source"] != "客户端透传" {
		t.Fatalf("该场景模式应为透传: %#v", record)
	}
	if record["forced_applied"] != false || record["forced_content_kind"] != "未支持的内容类型，已跳过强制后置" {
		t.Fatalf("不支持的内容类型应跳过强制后置: %#v", record)
	}
}

// TestSystemPromptPlainLogWording 固定普通运行日志的三种表达，避免"结果"与"文本来源"语义漂移：
// 覆盖 / 客户端未给 system（注入配置 fallback）/ 客户端未给 system（内置兜底）/ 透传。
func TestSystemPromptPlainLogWording(t *testing.T) {
	oldLogWriter := log.Writer()
	var plainLog bytes.Buffer
	log.SetOutput(&plainLog)
	t.Cleanup(func() { log.SetOutput(oldLogWriter) })

	const client = "【客户端自己的系统提示词】"
	newReq := func() *http.Request {
		return httptest.NewRequest("POST", "/v1/chat/completions", nil)
	}
	withClientSystem := func() map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": client},
			map[string]any{"role": "user", "content": "hi"},
		}}
	}
	withoutClientSystem := func() map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	}

	for _, tc := range []struct {
		name, fallback, wantMode, wantSource, wantEffect string
		obj                                              func() map[string]any
	}{
		{"override", "【网关人设】", "覆盖", "配置覆盖", "覆盖客户端system为配置fallback", withClientSystem},
		{"injected_config_fallback", "【网关人设】", "覆盖", "配置保底", "客户端未提供system，已注入配置fallback", withoutClientSystem},
		{"injected_builtin", "", "保底", "内置保底", "客户端未提供system，已使用内置兜底提示词", withoutClientSystem},
		{"passthrough", "", "透传", "客户端透传", "保留客户端原有system", withClientSystem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plainLog.Reset()
			withSystemPrompts(t, tc.fallback, "")
			obj := tc.obj()
			prepareSystemPromptForUpstream(obj, newReq(), 41, "log-trace")
			line := plainLog.String()
			for _, want := range []string{
				"生效模式=" + tc.wantMode,
				"文本来源=" + tc.wantSource,
				"结果=" + tc.wantEffect + "且未记录提示词正文",
			} {
				if !strings.Contains(line, want) {
					t.Fatalf("普通日志缺少 %q:\n%s", want, line)
				}
			}
		})
	}
}

// TestSystemPromptBodyLoggingSwitch 固定需求 2：正文默认不记录，显式开启后才落 JSON 调试日志。
func TestSystemPromptBodyLoggingSwitch(t *testing.T) {
	var output bytes.Buffer
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = &debugJSONSink{writer: &output}
	debugSinkMu.Unlock()
	oldEnabled, oldLogBody := cfg.DebugEnabled, cfg.DebugLogSystemPrompt
	oldLogWriter := log.Writer()
	t.Cleanup(func() {
		cfg.DebugEnabled, cfg.DebugLogSystemPrompt = oldEnabled, oldLogBody
		log.SetOutput(oldLogWriter)
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
	})

	const fallback = "【网关人设】"
	const client = "【客户端自己的系统提示词】"
	const force = "【强制后置】"
	withSystemPrompts(t, fallback, force)

	newRequest := func() (*http.Request, map[string]any) {
		obj := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": client},
			map[string]any{"role": "user", "content": "hi"},
		}}
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		ensureDebugRequestContext(req, "prompt-trace", time.Now())
		return req, obj
	}

	// 开关关闭：调试日志与普通运行日志都不得出现提示词正文。
	cfg.DebugEnabled = true
	cfg.DebugLogSystemPrompt = false
	var plainLog bytes.Buffer
	log.SetOutput(&plainLog)
	req, obj := newRequest()
	prepareSystemPromptForUpstream(obj, req, 21, "prompt-trace")
	if strings.Contains(output.String(), fallback) || strings.Contains(output.String(), client) {
		t.Fatalf("logSystemPrompt 关闭时调试日志不得记录正文: %s", output.String())
	}
	if !strings.Contains(output.String(), "system_prompt_policy_applied") {
		t.Fatalf("调试事件缺失: %s", output.String())
	}
	// 关闭正文记录时仍必须保留长度与指纹这类非正文审计信息，否则无法判断"哪一版提示词发出去了"。
	var offRecord map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &offRecord); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	for _, key := range []string{"final_system_kind", "final_system_chars", "final_system_sha256_prefix", "client_system_chars", "client_system_sha256_prefix"} {
		if _, ok := offRecord[key]; !ok {
			t.Errorf("关闭正文记录时缺少审计字段 %q: %#v", key, offRecord)
		}
	}
	if _, ok := offRecord["final_system_body"]; ok {
		t.Fatal("关闭正文记录时不得写入 final_system_body")
	}
	if got := offRecord["final_system_chars"].(float64); got != float64(utf8.RuneCountInString(fallback+"\n\n"+force)) {
		t.Fatalf("final_system_chars 不符: %#v", offRecord["final_system_chars"])
	}
	if strings.Contains(plainLog.String(), fallback) || strings.Contains(plainLog.String(), client) || strings.Contains(plainLog.String(), force) {
		t.Fatalf("普通运行日志不得记录提示词正文: %s", plainLog.String())
	}

	// 开关开启：记录最终正文与覆盖前的客户端正文。
	output.Reset()
	cfg.DebugLogSystemPrompt = true
	req, obj = newRequest()
	prepareSystemPromptForUpstream(obj, req, 22, "prompt-trace")
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["event"] != "system_prompt_policy_applied" {
		t.Fatalf("事件名不符: %#v", record["event"])
	}
	if record["fallback_source"] != "配置覆盖" || record["override_applied"] != true || record["prompt_mode"] != "覆盖" {
		t.Fatalf("覆盖审计字段不符: %#v", record)
	}
	if record["final_system_body"] != fallback+"\n\n"+force {
		t.Fatalf("final_system_body 不符: %#v", record["final_system_body"])
	}
	if record["client_system_body"] != client {
		t.Fatalf("client_system_body 不符: %#v", record["client_system_body"])
	}

	// 内容块数组场景：final_system_body 记录各 text 块拼接后的纯文本。
	output.Reset()
	obj = map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "块一"},
			map[string]any{"type": "text", "text": "块二"},
		}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(req, "prompt-trace", time.Now())
	prepareSystemPromptForUpstream(obj, req, 23, "prompt-trace")
	record = map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["client_system_body"] != "块一\n块二" {
		t.Fatalf("内容块数组应拼接为纯文本: %#v", record["client_system_body"])
	}
	// 覆盖后的 fallback 与 force 各占一个文本块，拼接后以单个换行分隔。
	if record["final_system_body"] != fallback+"\n"+force {
		t.Fatalf("final_system_body 不符: %#v", record["final_system_body"])
	}

	// 未发生覆盖时（fallback 为空）不应写入 client_system_body，避免把透传正文当成覆盖证据。
	output.Reset()
	withSystemPrompts(t, "", "")
	req, obj = newRequest()
	prepareSystemPromptForUpstream(obj, req, 24, "prompt-trace")
	record = map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("调试日志不是单行 JSON: %v: %s", err, output.String())
	}
	if record["fallback_source"] != "客户端透传" || record["override_applied"] != false || record["prompt_mode"] != "透传" {
		t.Fatalf("透传场景审计字段不符: %#v", record)
	}
	if _, ok := record["client_system_body"]; ok {
		t.Fatalf("未覆盖时不应记录 client_system_body: %#v", record)
	}
	if record["final_system_body"] != client {
		t.Fatalf("final_system_body 应记录透传后的正文: %#v", record["final_system_body"])
	}
}
