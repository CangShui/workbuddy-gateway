package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withRedact(t *testing.T, c redactFileConfig) {
	t.Helper()
	old := currentRedactOptions()
	setRedactConfig(c)
	t.Cleanup(func() {
		redactSettings.Store(&old)
	})
}

const (
	synthKey   = "sk-proj-AbCdEfGhIjKlMnOpQrStUv1234567890"
	synthEmail = "zhang.san.synthetic@qq.com"
	synthPhone = "13812345678"
)

func TestRedactTextRules(t *testing.T) {
	o := redactOptions{secrets: true, personal: true, words: []string{"SyntheticFalconWord"}}
	in := "key " + synthKey + "\npassword=Hunter2xyz9\nmail " + synthEmail + " 手机 " + synthPhone +
		"\nproject SyntheticFalconWord\nkeep user@example.com 订单号 13812345679 model gpt-4o max_tokens=4096"
	var hits []redactHit
	out := redactText(in, o, redactTag, &hits)
	for _, secret := range []string{synthKey, "Hunter2xyz9", synthEmail, synthPhone, "SyntheticFalconWord"} {
		if strings.Contains(out, secret) {
			t.Errorf("未脱敏: %s\n%s", secret, out)
		}
	}
	for _, keep := range []string{"user@example.com", "订单号 13812345679", "gpt-4o", "max_tokens=4096"} {
		if !strings.Contains(out, keep) {
			t.Errorf("误脱敏: %s\n%s", keep, out)
		}
	}
	if len(hits) != 5 {
		t.Fatalf("命中数=%d %+v", len(hits), hits)
	}
	// 只开个人信息时不动密钥
	out = redactText(in, redactOptions{personal: true}, redactTag, nil)
	if !strings.Contains(out, synthKey) || strings.Contains(out, synthEmail) {
		t.Fatalf("类别开关无效:\n%s", out)
	}
}

func TestMaskRequestJSONSkipsStructure(t *testing.T) {
	o := redactOptions{secrets: true, personal: true}
	body := `{"model":"glm-5","tools":[{"type":"function","function":{"name":"f","description":"uses ` + synthKey + `"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"mail me at ` + synthEmail + `"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + synthKey + `"}}]},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"login","arguments":"{\"user\":\"bob\",\"token\":\"ghp_abcdefghijklmnopqrstuvwxyz0123456789\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"ok"}],"stream":true,"max_tokens":12345678901}`
	out, res, err := maskRequestJSON([]byte(body), o)
	if err != nil {
		t.Fatal(err)
	}
	hits := res.hits
	if res.model != "glm-5" || len(res.segments) != 2 {
		t.Fatalf("记录信息错误: model=%q segments=%+v", res.model, res.segments)
	}
	for _, seg := range res.segments {
		if strings.Contains(seg.Text, synthEmail) || !strings.Contains(seg.Text, "{{") {
			t.Fatalf("记录片段必须是上游收到的脱敏文字: %+v", seg)
		}
	}
	if !strings.Contains(res.segments[0].Where+res.segments[1].Where, "(user)") {
		t.Fatalf("片段应标明消息角色: %+v", res.segments)
	}
	if len(hits) != 2 {
		t.Fatalf("命中数=%d %+v\n%s", len(hits), hits, out)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, synthEmail) || strings.Contains(s, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("消息内容未脱敏: %s", s)
	}
	if strings.Count(s, synthKey) != 2 || !strings.Contains(s, `"call_1"`) || !strings.Contains(s, "12345678901") {
		t.Fatalf("工具定义、图片数据、ID 或数字被改动: %s", s)
	}
	msgs := doc["messages"].([]any)
	args := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	var parsed map[string]string
	if err := json.Unmarshal([]byte(args), &parsed); err != nil || parsed["user"] != "bob" || !strings.HasPrefix(parsed["token"], "{{API_KEY_") {
		t.Fatalf("工具参数应保持合法 JSON 并只替换值: %q %v", args, err)
	}
	// 全关：原样返回
	if same, none, _ := maskRequestJSON([]byte(body), redactOptions{}); string(same) != body || none != nil {
		t.Fatal("关闭时不得改动请求")
	}
	if _, _, err := maskRequestJSON([]byte(`{bad`), o); err == nil {
		t.Fatal("无效 JSON 必须拒绝发送")
	}
}

func TestRedactRestoreVariants(t *testing.T) {
	p := redactPlaceholder("EMAIL", synthEmail)
	body := strings.Trim(p, "{}")
	for _, in := range []string{p, "{{ " + body + " }}", body, "{" + body + "}}", strings.ToLower(body), strings.ToUpper(body[:len(body)-8]) + strings.ToUpper(body[len(body)-8:])} {
		if got := redactRestore("to "+in+".", false); got != "to "+synthEmail+"." {
			t.Errorf("%q 还原为 %q", in, got)
		}
	}
	if got := redactRestore("X"+body, false); got != "X"+body {
		t.Fatalf("长词中的一段不应还原: %q", got)
	}
}

func readRestored(t *testing.T, sse string) (content, args string, raw string) {
	t.Helper()
	b := newRedactRestoreBody(io.NopCloser(strings.NewReader(sse)), nil)
	all, err := io.ReadAll(b)
	if err != nil {
		t.Fatal(err)
	}
	raw = string(all)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &chunk); err != nil {
			t.Fatalf("输出不是合法 JSON: %s (%v)", line, err)
		}
		for _, c := range chunk.Choices {
			content += c.Delta.Content
			for _, tc := range c.Delta.ToolCalls {
				args += tc.Function.Arguments
			}
		}
	}
	return
}

func sseChunk(delta string, finish string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + f + "}]}\n\n"
}

func TestRedactStreamRestoresSplitPlaceholders(t *testing.T) {
	p := redactPlaceholder("EMAIL", synthEmail)
	q := redactPlaceholder("TERM", `Acme"Corp`)
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	var sse strings.Builder
	sse.WriteString(": keepalive\n\n")
	sse.WriteString(sseChunk(`{"role":"assistant","content":""}`, ""))
	for _, part := range []string{"写信给 ", p[:5], p[5:14], p[14:], " 吧 {"} {
		sse.WriteString(sseChunk(`{"content":`+quote(part)+`}`, ""))
	}
	args := `{"q":"` + q + `"}`
	for i, part := range []string{args[:9], args[9:20], args[20:]} {
		head := ""
		if i == 0 {
			head = `"id":"call_1","type":"function",`
		}
		sse.WriteString(sseChunk(`{"tool_calls":[{"index":0,`+head+`"function":{"arguments":`+quote(part)+`}}]}`, ""))
	}
	sse.WriteString(sseChunk(`{}`, "tool_calls"))
	sse.WriteString("data: [DONE]\n\n")

	content, gotArgs, raw := readRestored(t, sse.String())
	if content != "写信给 "+synthEmail+" 吧 {" {
		t.Fatalf("正文还原错误: %q\n%s", content, raw)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(gotArgs), &parsed); err != nil || parsed["q"] != `Acme"Corp` {
		t.Fatalf("工具参数还原错误: %q %v", gotArgs, err)
	}
	if strings.Contains(raw, "EMAIL_") || !strings.Contains(raw, ": keepalive") || !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("SSE 框架或还原异常:\n%s", raw)
	}
}

func TestRedactStreamFlushesHeldTailAtEOF(t *testing.T) {
	p := redactPlaceholder("PHONE", synthPhone)
	sse := sseChunk(`{"content":"拨打 `+p[:9]+`"}`, "")
	content, _, raw := readRestored(t, sse)
	if content != "拨打 "+p[:9] {
		t.Fatalf("流意外结束时扣下的内容必须补发: %q\n%s", content, raw)
	}
}

func TestDoUpstreamRequestRedactsAndRestores(t *testing.T) {
	withRedact(t, redactFileConfig{Secrets: true, Personal: true})
	oldClient := cfg.HttpClient
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	var sent string
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		var reader io.Reader = req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			reader = gz
		}
		body, _ := io.ReadAll(reader)
		sent = string(body)
		p := redactPlaceholderRe.FindString(sent)
		reply := sseChunk(`{"content":"你的 Key 是 `+p+`"}`, "") + sseChunk(`{}`, "stop") + "data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
	})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	acc := &Account{Path: "redact.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	body := `{"model":"m","messages":[{"role":"user","content":"我的 key 是 ` + synthKey + `"}],"stream":true}`
	resp, cancel, err := doUpstreamRequest(req, acc, &profileCN, []byte(body), 1, "redact-test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer resp.Body.Close()
	if strings.Contains(sent, synthKey) || !strings.Contains(sent, "{{API_KEY_") {
		t.Fatalf("上游收到了原文: %s", sent)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), synthKey) {
		t.Fatalf("回复中的占位符未还原: %s", got)
	}
}

func TestRedactAdminAPIPersistsAndPreviews(t *testing.T) {
	isolateAdminFeatures(t)
	t.Cleanup(func() { setRedactConfig(redactFileConfig{}) })
	handler := newWebUIHandler()
	for _, bad := range []string{`{}`, `{"secrets":"yes"}`, `{"words":["a"]}`, `{"words":["x\ny"]}`, `{"other":true}`} {
		if w := adminRequest(handler, "POST", "/admin/api/redact", "feature-admin-key", bad); w.Code != 400 {
			t.Fatalf("非法设置被接受: %s -> %d %s", bad, w.Code, w.Body.String())
		}
	}
	if w := adminRequest(handler, "POST", "/admin/api/redact", "feature-api-key", `{"secrets":true}`); w.Code != 401 {
		t.Fatal("模型 API Key 不能修改脱敏设置")
	}
	w := adminRequest(handler, "POST", "/admin/api/redact", "feature-admin-key", `{"secrets":true,"words":[" SyntheticFalconWord ","SyntheticFalconWord"]}`)
	if w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	if o := currentRedactOptions(); !o.secrets || o.personal || len(o.words) != 1 {
		t.Fatalf("运行时未生效: %+v", o)
	}
	raw, _ := os.ReadFile(runtimeConfigFile)
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil || !c.Redact.Secrets || len(c.Redact.Words) != 1 || c.SystemPrompt.Force != "visible prompt text" {
		t.Fatalf("配置未正确落盘或覆盖了其他段: %s", raw)
	}
	w = adminRequest(handler, "GET", "/admin/api/redact", "feature-admin-key", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"SyntheticFalconWord"`) || !strings.Contains(w.Body.String(), `"active":true`) {
		t.Fatalf("读取设置失败: %s", w.Body.String())
	}
	redactStore.RLock()
	before := len(redactStore.values)
	redactStore.RUnlock()
	w = adminRequest(handler, "POST", "/admin/api/redact-preview", "feature-admin-key", `{"text":"key `+synthKey+` SyntheticFalconWord"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), synthKey) || !strings.Contains(w.Body.String(), "{{TERM_") {
		t.Fatalf("预览失败: %s", w.Body.String())
	}
	redactStore.RLock()
	after := len(redactStore.values)
	redactStore.RUnlock()
	if after != before {
		t.Fatal("预览不得登记可还原的原值")
	}
	// 关闭后不再改动请求
	if w := adminRequest(handler, "POST", "/admin/api/redact", "feature-admin-key", `{"secrets":false,"words":[]}`); w.Code != 200 || currentRedactOptions().active() {
		t.Fatalf("关闭失败: %s", w.Body.String())
	}
}

func TestRedactRecordsLifecycle(t *testing.T) {
	isolateAdminFeatures(t)
	t.Cleanup(func() { setRedactConfig(redactFileConfig{}); _, _ = clearRedactRecords("test-cleanup") })
	if _, err := clearRedactRecords("test-start"); err != nil {
		t.Fatal(err)
	}
	withRedact(t, redactFileConfig{Secrets: true, Personal: true})
	oldClient := cfg.HttpClient
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		var reader io.Reader = req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			gz, _ := gzip.NewReader(req.Body)
			reader = gz
		}
		body, _ := io.ReadAll(reader)
		p := redactPlaceholderRe.FindString(string(body))
		reply := sseChunk(`{"content":"已收到 `+p+`"}`, "") + sseChunk(`{}`, "stop") + "data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(reply))}, nil
	})}
	acc := &Account{Path: "dir/workbuddy-a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}}
	body := `{"model":"glm-5","messages":[{"role":"user","content":"邮箱 ` + synthEmail + ` 和 key ` + synthKey + `"}]}`
	// 同一请求换账号重试两次：合并为一条记录
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		resp, cancel, err := doUpstreamRequest(req, acc, &profileCN, []byte(body), 42, "trace-records")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if !strings.Contains(string(got), synthEmail) && !strings.Contains(string(got), synthKey) {
			t.Fatalf("回复未还原: %s", got)
		}
	}
	handler := newWebUIHandler()
	w := adminRequest(handler, "GET", "/admin/api/redact-records?days=7", "feature-admin-key", "")
	if w.Code != 200 {
		t.Fatalf("读取记录失败: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Records []struct {
			ID       string `json:"id"`
			Endpoint string `json:"endpoint"`
			Model    string `json:"model"`
			Account  string `json:"account"`
			Values   int    `json:"values"`
			Attempts int    `json:"attempts"`
			Restored int    `json:"restored"`
			HasText  bool   `json:"hasText"`
			Hits     []redactRecordHit
		} `json:"records"`
		Summary struct {
			Requests  int            `json:"requests"`
			Values    int            `json:"values"`
			Restored  int            `json:"restored"`
			Kinds     map[string]int `json:"kinds"`
			TopValues []map[string]any
		} `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Records) != 1 || list.Summary.Requests != 1 || list.Summary.Values != 2 {
		t.Fatalf("重试应合并为一条记录: %s", w.Body.String())
	}
	rec := list.Records[0]
	if rec.Endpoint != "/v1/messages" || rec.Model != "glm-5" || rec.Account != "workbuddy-a.json" || rec.Attempts != 2 || rec.Restored != 2 || !rec.HasText {
		t.Fatalf("记录字段错误: %+v", rec)
	}
	if list.Summary.Kinds["EMAIL"] != 1 || list.Summary.Kinds["API_KEY"] != 1 || len(list.Summary.TopValues) != 2 {
		t.Fatalf("汇总错误: %+v", list.Summary)
	}
	for _, h := range rec.Hits {
		if h.Kind == "API_KEY" && (h.Head != "sk-" || h.Len != len(synthKey)) || h.Kind == "EMAIL" && (h.Head != "z" || h.Len != len(synthEmail)) {
			t.Fatalf("提示应只含开头字符与长度: %+v", h)
		}
	}
	if strings.Contains(w.Body.String(), synthKey) || strings.Contains(w.Body.String(), synthEmail) {
		t.Fatal("记录列表不得含原值")
	}
	w = adminRequest(handler, "GET", "/admin/api/redact-record?id="+rec.ID, "feature-admin-key", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "{{API_KEY_") || strings.Contains(w.Body.String(), synthKey) || !strings.Contains(w.Body.String(), `"valuesInMemory":2`) {
		t.Fatalf("记录详情错误: %s", w.Body.String())
	}
	w = adminRequest(handler, "POST", "/admin/api/redact-reveal", "feature-admin-key", `{"id":"`+rec.ID+`"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), synthKey) {
		t.Fatalf("查看原值失败: %s", w.Body.String())
	}
	if w := adminRequest(handler, "POST", "/admin/api/redact-reveal", "feature-api-key", `{"id":"`+rec.ID+`"}`); w.Code != 401 {
		t.Fatal("查看原值必须使用管理 Key")
	}
	files, _ := filepath.Glob(filepath.Join(logDir, "redact-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("记录文件数=%d", len(files))
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), synthKey) || strings.Contains(string(raw), synthEmail) || strings.Contains(string(raw), "邮箱") {
		t.Fatalf("落盘记录不得含原值或请求正文: %s", raw)
	}
	// 模拟重启：从文件读回，文字不在了，还原次数仍在
	redactRecords.Lock()
	redactRecords.list, redactRecords.byKey, redactRecords.loaded = nil, map[string]*redactRecord{}, false
	redactRecords.Unlock()
	loadRedactRecords()
	w = adminRequest(handler, "GET", "/admin/api/redact-records?days=30&kind=EMAIL", "feature-admin-key", "")
	if !strings.Contains(w.Body.String(), `"restored":2`) || !strings.Contains(w.Body.String(), `"hasText":false`) || !strings.Contains(w.Body.String(), `"attempts":1`) {
		t.Fatalf("重启后读回错误: %s", w.Body.String())
	}
	if w := adminRequest(handler, "GET", "/admin/api/redact-records?kind=PHONE", "feature-admin-key", ""); !strings.Contains(w.Body.String(), `"records":[]`) {
		t.Fatalf("类型筛选错误: %s", w.Body.String())
	}
	if w := adminRequest(handler, "POST", "/admin/api/redact-records-clear", "feature-admin-key", `{}`); w.Code != 200 {
		t.Fatalf("清空失败: %s", w.Body.String())
	}
	if files, _ := filepath.Glob(filepath.Join(logDir, "redact-*.jsonl")); len(files) != 0 {
		t.Fatal("清空后记录文件仍在")
	}
	if w := adminRequest(handler, "GET", "/admin/api/redact-records", "feature-admin-key", ""); !strings.Contains(w.Body.String(), `"requests":0`) {
		t.Fatalf("清空后仍有记录: %s", w.Body.String())
	}
}
