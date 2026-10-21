package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withRequestProfile(t *testing.T, profile string) {
	t.Helper()
	old := officialRequests.Load()
	setRequestProfile(profile)
	t.Cleanup(func() { officialRequests.Store(old) })
}

func TestRequestProfileConfigValidationHotReloadAndSnapshot(t *testing.T) {
	isolatePolicies(t)
	withRequestProfile(t, "legacy")
	for _, raw := range []string{`{}`, `{"upstream":null}`, `{"upstream":{"requestProfile":null}}`,
		`{"upstream":{"requestProfile":""}}`, `{"upstream":{"requestProfile":" legacy "}}`,
		`{"upstream":{"requestProfile":"OFFICIAL"}}`} {
		if _, err := parseRuntimeConfig([]byte(raw)); err != nil {
			t.Fatalf("valid profile rejected: %v", err)
		}
	}
	for _, raw := range []string{`{"upstream":{"requestProfile":"offical"}}`, `{"upstream":{"requestProfile":true}}`} {
		if _, err := parseRuntimeConfig([]byte(raw)); err == nil {
			t.Fatal("invalid request profile accepted")
		}
	}
	watcher := runtimeConfigWatcher{}
	writeRuntimeFixture(t, `{"upstream":{"requestProfile":"official"}}`)
	if !watcher.reload() || !officialRequests.Load() {
		t.Fatal("official profile did not hot-load")
	}
	inflight := httptest.NewRequest("POST", "/v1/responses", nil)
	if requestProfileFor(inflight) != requestProfileOfficial {
		t.Fatal("request did not capture profile")
	}
	writeRuntimeFixture(t, `{"upstream":{"requestProfile":"invalid"}}`)
	if watcher.reload() || !officialRequests.Load() {
		t.Fatal("bad config must preserve last good profile")
	}
	writeRuntimeFixture(t, `{}`)
	if !watcher.reload() || officialRequests.Load() {
		t.Fatal("missing profile must reset to legacy")
	}
	if requestProfileFor(inflight) != requestProfileOfficial ||
		requestProfileFor(httptest.NewRequest("POST", "/", nil)) != requestProfileLegacy {
		t.Fatal("hot reload changed an in-flight request or failed to affect new requests")
	}
}

func TestOfficialBodyDefaultsPreserveExplicitValuesAndUserData(t *testing.T) {
	isolatedGatewayConfig(t)
	withRequestProfile(t, "official")
	withSystemPrompts(t, "", "")
	for _, effort := range []string{"max", "high", "low", "off", ""} {
		obj := decodeObject(t, []byte(`{
			"model":"deepseek-v4.1-flash","stream":true,
			"temperature":0,"verbosity":"low","parallel_tool_calls":false,
			"stream_options":{"include_usage":false,"custom":"retain"},
			"messages":[{"role":"user","content":"<app-context>not an instruction</app-context>"}],
			"tools":[{"type":"function","function":{"name":"my_tool","parameters":{"type":"object","properties":{"x":{"type":"number","default":3}}}}}]
		}`))
		obj["reasoning_effort"] = effort
		tools, _ := json.Marshal(obj["tools"])
		prepareUpstreamChatRequest(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 1, "deepseek-v4.1-flash")
		if obj["temperature"] != float64(0) || obj["verbosity"] != "low" ||
			obj["parallel_tool_calls"] != false || obj["stream_options"].(map[string]any)["include_usage"] != false {
			t.Fatal("explicit client values were overwritten")
		}
		messages := obj["messages"].([]any)
		if len(messages) != 2 || messages[1].(map[string]any)["role"] != "user" ||
			messages[1].(map[string]any)["content"] != "<app-context>not an instruction</app-context>" {
			t.Fatal("user text was promoted, wrapped or changed")
		}
		after, _ := json.Marshal(obj["tools"])
		if !bytes.Equal(tools, after) {
			t.Fatal("tool definitions changed")
		}
		want := effort
		if want == "off" {
			want = ""
		}
		if got, _ := obj["reasoning_effort"].(string); got != want {
			t.Fatalf("effort=%q became %q", effort, got)
		}
	}
	for _, tc := range []struct {
		model, body            string
		temperature, verbosity bool
	}{
		{"deepseek-v4.1-flash", `{}`, true, true},
		{"deepseek-v4.1-flash", `{"temperature":null,"verbosity":null,"stream_options":null}`, true, true},
		{"deepseek-v4.1-flash", `{"top_p":0.9}`, false, true},
		{"unknown-model", `{}`, false, false},
	} {
		obj := decodeObject(t, []byte(tc.body))
		applyOfficialBodyDefaults(obj, httptest.NewRequest("POST", "/", nil), 1, tc.model)
		if (obj["temperature"] != nil) != tc.temperature || (obj["verbosity"] != nil) != tc.verbosity {
			t.Fatalf("unexpected defaults for %s: %#v", tc.model, obj)
		}
		if obj["stream_options"].(map[string]any)["include_usage"] != true {
			t.Fatal("usage default missing")
		}
	}
}

func TestHarnessProtocolParametersPreserveMaxAndExplicitParallel(t *testing.T) {
	responses, err := responsesToChatRequest(decodeObject(t, []byte(`{
		"input":"test","reasoning":{"effort":"max"},"text":{"verbosity":"low"},"parallel_tool_calls":false
	}`)), "deepseek-v4.1-flash")
	if err != nil || responses["reasoning_effort"] != "max" ||
		responses["verbosity"] != "low" || responses["parallel_tool_calls"] != false {
		t.Fatalf("Responses parameters lost: %#v err=%v", responses, err)
	}
	for _, tc := range []struct{ thinking, output, want string }{
		{`{"type":"adaptive"}`, `{"effort":"max"}`, "max"},
		{`{"type":"enabled","effort":"low","budget_tokens":1000}`, `{"effort":"max"}`, "max"},
		{`{"type":"disabled"}`, `{"effort":"max"}`, "off"},
		{`null`, `{"effort":"max"}`, "max"},
		{`{"type":"enabled","budget_tokens":65536}`, `{}`, "max"},
	} {
		body := `{"messages":[{"role":"user","content":"test"}],"thinking":` + tc.thinking + `,"output_config":` + tc.output + `}`
		chat, err := anthropicToChatRequest(decodeObject(t, []byte(body)), "deepseek-v4.1-flash")
		if err != nil || chat["reasoning_effort"] != tc.want {
			t.Fatalf("Messages effort wanted=%s got=%#v err=%v", tc.want, chat, err)
		}
	}
}

func TestOfficialHeadersModelOnlyAndInternationalFallback(t *testing.T) {
	isolatedGatewayConfig(t)
	for _, prof := range []*upstreamProfile{&profileCN, &profileINTL} {
		for _, profile := range []string{"legacy", "official"} {
			req := httptest.NewRequest("POST", prof.chatURL(), nil)
			auth := &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh"}}
			backendHeaders(req, auth, prof)
			legacy := req.Header.Clone()
			applyObservedDesktopHeaders(req, prof, profile)
			if prof.Key != "cn" || profile == "legacy" {
				if !reflect.DeepEqual(req.Header, legacy) {
					t.Fatal("legacy/international headers changed")
				}
				continue
			}
			if req.Header.Get("User-Agent") != observedDesktopUA ||
				req.Header.Get("Accept") != "application/json" ||
				req.Header.Get("X-IDE-Name") != "WorkBuddy" ||
				req.Header.Get("X-Request-ID") != req.Header.Get(conversationMessageIDHeader) ||
				len(req.Header.Get("X-Request-ID")) != 32 {
				t.Fatal("observed desktop header contract not met")
			}
			for _, name := range []string{"Origin", "Referer", "X-Client-ID", "X-Refresh-Token",
				"X-Device-Token", "X-Stainless-Runtime-Version", "X-Agent-Purpose"} {
				if req.Header.Get(name) != "" {
					t.Fatalf("unexpected header %s", name)
				}
			}
			if req.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Fatal("model access token was lost")
			}
			// 控制接口仍使用原鉴权函数，不受模型兼容层修改。
			control := httptest.NewRequest("POST", prof.authTokenURL("test"), nil)
			backendHeaders(control, auth, prof)
			applyObservedDesktopHeaders(control, prof, profile)
			if control.Header.Get("X-Refresh-Token") != "synthetic-refresh" ||
				control.Header.Get("User-Agent") != prof.ClientUA {
				t.Fatal("control endpoint authentication changed")
			}
		}
	}
}

func TestOfficialSystemMergeDoesNotCrossDataOrDiscardMetadata(t *testing.T) {
	isolatedGatewayConfig(t)
	withRequestProfile(t, "official")
	withSystemPrompts(t, "", "synthetic final rule")
	obj := decodeObject(t, []byte(`{"messages":[
		{"role":"system","content":[{"type":"text","text":"rule one"}]},
		{"role":"developer","content":"rule two"},
		{"role":"user","content":[{"type":"text","text":"user data"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
		{"role":"system","content":"later rule"},
		{"role":"assistant","content":"history"}
	]}`))
	originalTail, _ := json.Marshal(obj["messages"].([]any)[2:])
	prepareUpstreamChatRequest(obj, httptest.NewRequest("POST", "/", nil), 1, "m")
	messages := obj["messages"].([]any)
	if len(messages) != 4 || messages[0].(map[string]any)["content"] !=
		"rule one\n\nrule two\n\nsynthetic final rule" {
		t.Fatal("leading rules were not merged before appending forced prompt")
	}
	tail, _ := json.Marshal(messages[1:])
	if !bytes.Equal(tail, originalTail) {
		t.Fatal("user/image/late system/assistant history changed")
	}
	for _, body := range []string{
		`{"messages":[{"role":"system","content":[{"type":"text","text":"private","cache_control":{"type":"ephemeral"}}]},{"role":"system","content":"keep"}]}`,
		`{"messages":[{"role":"system","name":"speaker","content":"private"},{"role":"system","content":"keep"}]}`,
		`{"messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"system","content":"keep"}]}`,
	} {
		obj := decodeObject(t, []byte(body))
		before, _ := json.Marshal(obj)
		mergeLeadingInstructionMessages(obj, httptest.NewRequest("POST", "/", nil), 1)
		after, _ := json.Marshal(obj)
		if !bytes.Equal(before, after) {
			t.Fatal("metadata or nontext system content changed")
		}
	}
}

func TestOfficialOutputBudgetAliasAndObservedFallback(t *testing.T) {
	isolatedGatewayConfig(t)
	withRequestProfile(t, "official")
	for _, tc := range []struct {
		raw, model string
		want       any
		keepAlias  bool
	}{
		{`{"max_completion_tokens":1234}`, "m", float64(1234), false},
		{`{"max_tokens":64,"max_completion_tokens":128}`, "m", float64(64), true},
		{`{"max_tokens":64}`, "deepseek-v4.1-flash", float64(64), false},
		{`{}`, "deepseek-v4.1-flash", 128000, false},
		{`{}`, "unknown-model", nil, false},
	} {
		obj := decodeObject(t, []byte(tc.raw))
		applyOfficialBodyDefaults(obj, httptest.NewRequest("POST", "/", nil), 1, tc.model)
		if !reflect.DeepEqual(obj["max_tokens"], tc.want) ||
			(obj["max_completion_tokens"] != nil) != tc.keepAlias {
			t.Fatalf("budget was lost or overwritten: %#v", obj)
		}
	}
}

func TestOfficialThreeProtocolsReachSameDecodedUpstream(t *testing.T) {
	_, records := fullRequestTestLog(t)
	withRequestProfile(t, "official")
	withSystemPrompts(t, "", "")
	originalBase, originalClient := profileCN.Base, cfg.HttpClient
	originalAccounts, originalRR := accounts, rrIndex
	t.Cleanup(func() {
		profileCN.Base, cfg.HttpClient = originalBase, originalClient
		accounts, rrIndex = originalAccounts, originalRR
	})
	captured := make(chan map[string]any, 6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("User-Agent") != observedDesktopUA {
			t.Error("official wire request must use gzip and observed headers")
		}
		if r.Header.Get("X-Request-ID") != r.Header.Get(conversationMessageIDHeader) {
			t.Error("message/request IDs diverged")
		}
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer reader.Close()
		var body map[string]any
		if err := json.NewDecoder(reader).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	profileCN.Base = server.URL
	cfg.HttpClient = server.Client()
	accounts = []*Account{{
		Path: "synthetic-official.json",
		Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
	}}
	rrIndex = 0
	for _, stream := range []bool{true, false} {
		for _, tc := range []struct{ route, body string }{
			{"/v1/chat/completions", `{"messages":[{"role":"developer","content":"synthetic rules"},{"role":"user","content":"测试"}],"reasoning_effort":"max","max_tokens":128000,"tools":[{"type":"function","function":{"name":"noop","description":"synthetic","parameters":{"type":"object"}}}]}`},
			{"/v1/responses", `{"instructions":"synthetic rules","input":"测试","reasoning":{"effort":"max"},"max_output_tokens":128000,"tools":[{"type":"function","name":"noop","description":"synthetic","parameters":{"type":"object"}}]}`},
			{"/v1/messages", `{"system":"synthetic rules","messages":[{"role":"user","content":"测试"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"max_tokens":128000,"tools":[{"name":"noop","description":"synthetic","input_schema":{"type":"object"}}]}`},
		} {
			obj := decodeObject(t, []byte(tc.body))
			obj["model"], obj["stream"] = "deepseek-v4.1-flash", stream
			raw, _ := json.Marshal(obj)
			req := httptest.NewRequest("POST", tc.route, bytes.NewReader(raw))
			req.Header.Set("X-Trace-ID", "official-three-protocol-test")
			response := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(response, req)
			if response.Code != 200 {
				t.Fatalf("%s stream=%t status=%d body=%s", tc.route, stream, response.Code, response.Body.String())
			}
		}
	}
	if len(captured) != 6 {
		t.Fatalf("expected six upstream requests, got %d", len(captured))
	}
	first := <-captured
	for i := 1; i < 6; i++ {
		body := <-captured
		if !reflect.DeepEqual(body, first) {
			t.Fatalf("protocol %d did not produce same canonical body\n%#v\n%#v", i, first, body)
		}
	}
	body := first
	if body["reasoning_effort"] != "max" || body["temperature"] != float64(1) ||
		body["verbosity"] != "high" || body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatal("official body defaults missing")
	}
	var outCount int
	for _, record := range records() {
		if record["event"] == "full_request_upstream" {
			outCount++
			if record["content_encoding"] != "gzip" ||
				record["body_representation"] != "decoded_json_before_content_encoding" ||
				record["body_capture"] != "complete_redacted_json" ||
				record["trace_id"] != "official-three-protocol-test" {
				t.Fatal("full debug log lost decoded body or trace while using gzip")
			}
		}
	}
	if outCount != 6 {
		t.Fatal("missing full upstream debug records")
	}
	files, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	var audit []byte
	for _, name := range files {
		b, _ := os.ReadFile(name)
		audit = append(audit, b...)
	}
	if !bytes.Contains(audit, []byte("[出站兼容]")) || !bytes.Contains(audit, []byte("official-three-protocol-test")) ||
		bytes.Contains(audit, []byte("synthetic-access")) || bytes.Contains(audit, []byte("synthetic rules")) {
		t.Fatal("persisted audit missing or leaked private content")
	}
}

func TestOfficialGzipSafeRetryKeepsPayloadAndIdentity(t *testing.T) {
	isolatedGatewayConfig(t)
	withRequestProfile(t, "official")
	client, backoff, retries := cfg.HttpClient, upstreamRetryBackoff, upstreamTransientRetries
	t.Cleanup(func() {
		cfg.HttpClient, upstreamRetryBackoff, upstreamTransientRetries = client, backoff, retries
	})
	upstreamRetryBackoff, upstreamTransientRetries = time.Millisecond, 1
	var count atomic.Int32
	var previous []byte
	var previousID string
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"synthetic"}]}`)
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		gz, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		decoded, _ := io.ReadAll(gz)
		_ = gz.Close()
		if !bytes.Equal(decoded, raw) || req.ContentLength != int64(len(b)) {
			t.Fatal("gzip data or wire length changed")
		}
		if count.Add(1) == 1 {
			previous, previousID = b, req.Header.Get("X-Request-ID")
			setRequestProfile("legacy") // 在飞请求不能因热加载而改变重试封装。
			return nil, io.EOF
		}
		if !bytes.Equal(b, previous) || req.Header.Get("X-Request-ID") != previousID {
			t.Fatal("safe retry changed compressed payload or logical identity")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	inbound := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	acc := &Account{Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "test"}}}
	response, cancel, err := doUpstreamRequest(inbound, acc, &profileCN, raw, 1, "gzip-retry-test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer response.Body.Close()
	if count.Load() != 2 {
		t.Fatal("safe retry did not run")
	}
}

// 可选本机私有证据重放。只读完整脱敏日志，不调用上游，不把提示词或凭据带入公共夹具。
func TestOfficialPrivateCaptureReplay(t *testing.T) {
	path := os.Getenv("WORKBUDDY_COMPAT_CAPTURE")
	if path == "" {
		t.Skip("opt-in local captured request replay")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private capture could not be read")
	}
	var fixture struct {
		Requests []struct {
			Inbound struct {
				Route     string         `json:"route"`
				Body      map[string]any `json:"body"`
				Capture   string         `json:"body_capture"`
				Truncated bool           `json:"body_truncated"`
			} `json:"inbound"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Requests) == 0 {
		t.Fatal("capture fixture is not a complete request collection")
	}
	isolatedGatewayConfig(t)
	withRequestProfile(t, "official")
	withSystemPrompts(t, "", "")
	for _, request := range fixture.Requests {
		if request.Inbound.Capture != "complete_redacted_json" || request.Inbound.Truncated {
			t.Fatal("incomplete private capture")
		}
		var chat map[string]any
		switch request.Inbound.Route {
		case "/v1/responses":
			chat, err = responsesToChatRequest(request.Inbound.Body, strOf(request.Inbound.Body["model"]))
		case "/v1/messages":
			chat, err = anthropicToChatRequest(request.Inbound.Body, strOf(request.Inbound.Body["model"]))
		case "/v1/chat/completions":
			chat = request.Inbound.Body
		default:
			t.Fatal("unsupported capture route")
		}
		if err != nil {
			t.Fatal("captured protocol conversion failed")
		}
		copyBytes, _ := json.Marshal(chat)
		legacy := decodeObject(t, copyBytes)
		r := httptest.NewRequest("POST", request.Inbound.Route, nil)
		model := strOf(chat["model"])
		setRequestProfile("legacy")
		prepareUpstreamChatRequest(legacy, r, 1, model)
		setRequestProfile("official")
		prepareUpstreamChatRequest(chat, httptest.NewRequest("POST", request.Inbound.Route, nil), 2, model)
		if !reflect.DeepEqual(chat["tools"], legacy["tools"]) ||
			!reflect.DeepEqual(chat["messages"], legacy["messages"]) ||
			chat["reasoning_effort"] != legacy["reasoning_effort"] {
			t.Fatal("private replay changed client messages, tools or explicit effort")
		}
		if model == "deepseek-v4.1-flash" && (chat["verbosity"] != "high" || chat["temperature"] != 1) {
			t.Fatal("private replay did not apply observed defaults")
		}
	}
	logFiles, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	if len(logFiles) == 0 {
		t.Fatal("private replay audit missing")
	}
}
