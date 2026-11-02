package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// 联网搜索转发（POST /v1/search）
//
// 上游接口由 REA 从 WorkBuddy 客户端产物中定位并活体验证：
//
//	C:\Program Files\WorkBuddy\resources\app.asar.unpacked\cli\dist\codebuddy-lite-wb.mjs
//	  WEB_SEARCH_API_PATH = "/agenttool/v1/search"
//	  WEB_SEARCH_TIMEOUT_MS = 20000
//	  URL    = productManager.endpoint + WEB_SEARCH_API_PATH
//	  body   = {query, type:"text2text", max_results:5[, allowed_domains][, freshness]}
//	  响应   = {query, type, provider, results[{title,url,snippet,site?,favicon?}],
//	            total_results, response_time_ms}
//
// DSH uses count and a stricter result schema than WorkBuddy's upstream endpoint.
// -----------------------------------------------------------------------------

const (
	webSearchAPIPath = "/agenttool/v1/search"

	// 官方客户端把搜索请求上限设为 20s，这里保持一致。
	webSearchTimeout = 20 * time.Second

	// 官方客户端硬编码 max_results=5。
	webSearchDefaultMaxResults = 5
	webSearchMaxResults        = 20

	webSearchMaxQueryLength = 2000
	webSearchMaxBodyBytes   = 1 << 20

	// -------------------------------------------------------------------------
	// 网页抓取（POST /v1/fetch）-> 上游 /agenttool/v1/webfetch
	// -------------------------------------------------------------------------
	webFetchAPIPath = "/agenttool/v1/webfetch"

	// 官方客户端 WEB_FETCH 相关常量：cc=2e4（API 超时 20s）、cu=1e5（max_length）。
	webFetchUpstreamTimeout = 30 * time.Second
	webFetchMaxLength       = 100000

	// webFetchDefaultPrompt 是上游 webfetch 必填的 prompt（客户端由模型生成，
	// 网关侧没有模型上下文，给一个稳定的「提取正文」指令）。
	webFetchDefaultPrompt = "Extract the main content of this page as clean Markdown. Keep headings, lists, code blocks and key facts. Remove navigation, ads and boilerplate."
)

func (p *upstreamProfile) agentToolBase() string {
	if p == nil {
		return ""
	}
	base := p.Base
	if p.ChatBase != "" && (p.DefaultBase == "" || p.Base == p.DefaultBase) {
		base = p.ChatBase
	}
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

// webSearchRequest is the DSH Search API request shape.
type webSearchRequest struct {
	Query       string `json:"query"`
	Count       int    `json:"count,omitempty"`
	NeedContent *bool  `json:"need_content,omitempty"`
	OriQuestion string `json:"ori_question,omitempty"`
}

type workbuddyWebSearchRequest struct {
	Query      string `json:"query"`
	Type       string `json:"type"`
	MaxResults int    `json:"max_results"`
}

// webSearchFetchRequest 是 POST /v1/fetch 的请求体（DSH Fetch API 形状）。
type webSearchFetchRequest struct {
	URL string `json:"url"`
}

// webSearchSearchError carries an upstream request failure.
// status==0 表示网络层失败（没有拿到 HTTP 响应）。
type webSearchSearchError struct {
	status int
	err    error
}

func (e *webSearchSearchError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("upstream status %d", e.status)
	}
	return e.err.Error()
}

func (e *webSearchSearchError) Unwrap() error { return e.err }

func newWebSearchError(status int, format string, args ...any) *webSearchSearchError {
	return &webSearchSearchError{status: status, err: fmt.Errorf(format, args...)}
}

// handleWebSearch maps the DSH Search API request and response shapes.
func handleWebSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}
	traceID := w.Header().Get("X-Trace-ID")
	if traceID == "" {
		traceID = newTraceID()
	}

	body, readErr := io.ReadAll(io.LimitReader(r.Body, webSearchMaxBodyBytes))
	_ = r.Body.Close()
	if readErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}

	var req webSearchRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
			return
		}
	}
	if err := normalizeWebSearchRequest(&req); err != nil {
		log.Printf("[联网搜索] traceId=%s 阶段=参数校验 结果=拒绝 原因=%s 业务影响=未调用上游，不消耗账号额度", traceID, err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// 搜索同样需要一组会话标识：上游按 X-Conversation-* 归属调用来源。
	// 客户端若带了 X-Conversation-ID，会在这里被登记并复用。
	prepareConversationContext(r, nil)

	acc := webSearchAccount()
	if acc == nil {
		log.Printf("[联网搜索] traceId=%s 阶段=选择账号 结果=失败 原因=没有可用凭据", traceID)
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "没有可用的登录凭据，请先登录账号")
		return
	}
	if err := ensureValidTokenFor(acc); err != nil {
		log.Printf("[联网搜索] traceId=%s 阶段=凭据续期 结果=失败 原因=%s", traceID, safeDebugError(err))
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_failed", "账号凭据续期失败")
		return
	}
	auth, prof := webSearchAccountSnapshot(acc)
	if auth == nil || prof == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "没有可用的登录凭据，请先登录账号")
		return
	}

	payload, err := json.Marshal(workbuddyWebSearchRequest{
		Query: req.Query, Type: "text2text", MaxResults: req.Count,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "构造上游请求体失败")
		return
	}

	started := time.Now()
	obj, callErr := requestWebSearch(r, payload, auth, prof, prof.agentToolBase())
	if callErr == nil {
		log.Printf("[联网搜索] traceId=%s 阶段=完成 账号=%s 结果=成功 耗时=%v", traceID, baseNameOf(acc), time.Since(started))
		writeWebSearchSuccess(w, obj)
		return
	}
	status, errType, message := classifyWebSearchFailure(callErr.status, callErr)
	log.Printf("[联网搜索] traceId=%s 阶段=上游调用 账号=%s 状态码=%d 结果=失败 原因=%s",
		traceID, baseNameOf(acc), callErr.status, safeDebugError(callErr))
	writeUpstreamError(w, r, status, errType, message)
}

// handleWebFetch 处理 POST /v1/fetch：转发到上游 /agenttool/v1/webfetch，返回 DSH Fetch API 形状。
//
// 上游请求体（REA 取证自 CLI 产物 fetchContentViaApi）：
//
//	{url, format:"markdown", prompt, timeout, max_length[, model_name]}
//
// 上游响应：{url, title, content, content_type, content_length, truncated, status_code, context}
// DSH 新版 Search / Fetch API（deepseek-fetch-api）的解码器只读顶层
// {content: string}；它自己再包装成 {url, statusCode, body:{kind:"text",content}, truncated}。
// DSH reads the top-level content field and wraps it into its Fetch result type.
func handleWebFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}
	traceID := w.Header().Get("X-Trace-ID")
	if traceID == "" {
		traceID = newTraceID()
	}

	body, readErr := io.ReadAll(io.LimitReader(r.Body, webSearchMaxBodyBytes))
	_ = r.Body.Close()
	if readErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	var req webSearchFetchRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
			return
		}
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "url 不能为空")
		return
	}
	parsed, err := url.Parse(req.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "url 必须是 http/https 绝对地址")
		return
	}

	prepareConversationContext(r, nil)
	acc := webSearchAccount()
	if acc == nil {
		log.Printf("[网页抓取] traceId=%s 阶段=选择账号 结果=失败 原因=没有可用凭据", traceID)
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "没有可用的登录凭据，请先登录账号")
		return
	}
	if err := ensureValidTokenFor(acc); err != nil {
		log.Printf("[网页抓取] traceId=%s 阶段=凭据续期 结果=失败 原因=%s", traceID, safeDebugError(err))
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_failed", "账号凭据续期失败")
		return
	}
	auth, prof := webSearchAccountSnapshot(acc)
	if auth == nil || prof == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "没有可用的登录凭据，请先登录账号")
		return
	}

	payload, err := json.Marshal(map[string]any{
		"url":        req.URL,
		"format":     "markdown",
		"prompt":     webFetchDefaultPrompt,
		"timeout":    int(webFetchUpstreamTimeout / time.Second),
		"max_length": webFetchMaxLength,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "构造上游请求体失败")
		return
	}

	started := time.Now()
	obj, callErr := requestWebFetch(r, payload, auth, prof, prof.agentToolBase())
	if callErr == nil {
		log.Printf("[网页抓取] traceId=%s 阶段=完成 账号=%s 结果=成功 耗时=%v", traceID, baseNameOf(acc), time.Since(started))
		writeWebFetchSuccess(w, traceID, obj)
		return
	}
	status, errType, message := classifyWebSearchFailure(callErr.status, callErr)
	log.Printf("[网页抓取] traceId=%s 阶段=上游调用 账号=%s 状态码=%d 结果=失败 原因=%s",
		traceID, baseNameOf(acc), callErr.status, safeDebugError(callErr))
	writeUpstreamError(w, r, status, errType, message)
}

// writeWebFetchSuccess 把上游 webfetch 结果投影成 DSH Fetch API 形状。
func writeWebFetchSuccess(w http.ResponseWriter, traceID string, obj map[string]any) {
	content := webSearchString(obj["content"])
	if content == "" {
		// DSH 把空 content 视为不可用内容；这里如实报错而不是返回空正文。
		log.Printf("[网页抓取] traceId=%s 结果=失败 原因=上游未返回 content 业务影响=不伪造正文", traceID)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "上游未返回可用的页面正文")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content": content,
	})
}

func requestWebFetch(r *http.Request, payload []byte, auth *StoredAuth, prof *upstreamProfile, base string) (map[string]any, *webSearchSearchError) {
	return postAgentTool(r, payload, auth, prof, base, webFetchAPIPath, webFetchUpstreamTimeout)
}

// applyAgentToolHeaders 为国内站 /agenttool/* 补齐官方桌面身份头。
//
// 模型请求的同类头由 applyObservedDesktopHeaders 统一处理；搜索/抓取不经过
// 模型路由，因此在端点侧独立补齐。保留 backendHeaders 设置的 Origin/Referer
// 与 X-Client-ID，不套用模型请求的整套替换。
func applyAgentToolHeaders(req *http.Request, auth *StoredAuth, prof *upstreamProfile) {
	if prof == nil || prof.Key != "cn" {
		return
	}
	for name, value := range map[string]string{
		"X-Requested-With":    "XMLHttpRequest",
		"X-IDE-Type":          "WorkBuddy",
		"X-IDE-Name":          "WorkBuddy",
		"X-IDE-Version":       "5.8.3",
		"X-Private-Data":      "true",
		"X-Codebuddy-Request": "1",
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-Agent-Type":        "main",
	} {
		req.Header.Set(name, value)
	}
	if auth != nil && auth.Account.EnterpriseID != "" {
		req.Header.Set("X-Tenant-Id", auth.Account.EnterpriseID)
	}
	if traceparent := agentToolTraceparent(); traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
}

// agentToolTraceparent 生成符合 W3C 格式的 traceparent；失败时省略该头。
func agentToolTraceparent() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return "00-" + hex.EncodeToString(buf[:16]) + "-" + hex.EncodeToString(buf[16:]) + "-01"
}

func postAgentTool(r *http.Request, payload []byte, auth *StoredAuth, prof *upstreamProfile, base, path string, timeout time.Duration) (map[string]any, *webSearchSearchError) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	endpoint := strings.TrimRight(strings.TrimSpace(base), "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, newWebSearchError(0, "构造上游请求失败: %w", err)
	}
	backendHeaders(req, auth, prof)
	applyAgentToolHeaders(req, auth, prof)

	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, newWebSearchError(0, "上游接口超时（%v）", timeout)
		}
		return nil, newWebSearchError(0, "上游请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, webSearchMaxBodyBytes))
	if resp.StatusCode >= 400 {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, newWebSearchError(resp.StatusCode, "上游拒绝凭据（401），响应非 JSON")
		}
		return nil, newWebSearchError(resp.StatusCode, "上游状态码 %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, newWebSearchError(http.StatusBadGateway, "解析上游响应失败: %w", err)
	}
	// 上游成功响应没有 code 字段（实测：{"query":...,"type":...,"provider":...,"results":[...]}）；
	// 只有业务错误才会带非 0 code（例如 15003 query is required）。因此不能要求 code 必须存在。
	if code, present := webSearchUpstreamCode(obj); present && code != 0 {
		return nil, &webSearchSearchError{status: http.StatusBadRequest, err: &webSearchUpstreamError{code: code, msg: webSearchUpstreamMessage(obj)}}
	}
	return obj, nil
}

func requestWebSearch(r *http.Request, payload []byte, auth *StoredAuth, prof *upstreamProfile, base string) (map[string]any, *webSearchSearchError) {
	obj, callErr := postAgentTool(r, payload, auth, prof, base, webSearchAPIPath, webSearchTimeout)
	if callErr != nil {
		return nil, callErr
	}
	if _, ok := obj["results"]; !ok {
		return nil, newWebSearchError(http.StatusBadGateway, "上游搜索响应缺少 results 字段")
	}
	return obj, nil
}

// webSearchUpstreamError 表示上游 200 包络里的业务错误（code 非 0）。
type webSearchUpstreamError struct {
	code int
	msg  string
}

func (e *webSearchUpstreamError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("上游搜索接口返回 code=%d", e.code)
	}
	return fmt.Sprintf("上游搜索接口返回 code=%d: %s", e.code, e.msg)
}

func writeWebSearchSuccess(w http.ResponseWriter, obj map[string]any) {
	writeJSON(w, http.StatusOK, map[string]any{"results": webSearchResults(obj)})
}

// normalizeWebSearchRequest validates the DSH input and applies the supported count range.
func normalizeWebSearchRequest(req *webSearchRequest) error {
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return fmt.Errorf("query 不能为空")
	}
	if utf8Len(req.Query) < 2 {
		return fmt.Errorf("query 至少需要 2 个字符")
	}
	if utf8Len(req.Query) > webSearchMaxQueryLength {
		return fmt.Errorf("query 过长（最多 %d 个字符）", webSearchMaxQueryLength)
	}
	if req.Count <= 0 {
		req.Count = webSearchDefaultMaxResults
	}
	if req.Count > webSearchMaxResults {
		req.Count = webSearchMaxResults
	}
	return nil
}

func webSearchAccount() *Account {
	accountMu.Lock()
	defer accountMu.Unlock()
	now := time.Now()
	for _, acc := range accounts {
		if acc == nil || acc.Disabled || acc.Auth == nil || strings.TrimSpace(acc.Auth.Auth.AccessToken) == "" || acc.CooldownUntil.After(now) {
			continue
		}
		return acc
	}
	return nil
}

// webSearchAccountSnapshot 在锁内取一份凭据与站点参数的拷贝，避免并发调度期间读到半截状态。
func webSearchAccountSnapshot(acc *Account) (*StoredAuth, *upstreamProfile) {
	accountMu.Lock()
	defer accountMu.Unlock()
	if acc == nil || acc.Auth == nil {
		return nil, nil
	}
	auth := *acc.Auth
	return &auth, profileForEdition(auth.Edition)
}

func classifyWebSearchFailure(lastStatus int, lastErr error) (int, string, string) {
	switch {
	case lastStatus == http.StatusUnauthorized:
		return http.StatusUnauthorized, "upstream_auth_failed", "上游拒绝当前凭据（401），请重新登录账号"
	case lastStatus == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "upstream_rate_limited", "上游限流（429），请稍后重试"
	case lastStatus == http.StatusForbidden:
		return http.StatusForbidden, "upstream_forbidden", "上游拒绝本次搜索请求（403）"
	case lastStatus >= 400 && lastStatus < 500:
		return http.StatusBadRequest, "upstream_error",
			fmt.Sprintf("上游拒绝搜索请求（%d）：%s", lastStatus, safeDebugError(lastErr))
	default:
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("上游搜索接口调用失败：%s", safeDebugError(lastErr))
	}
}

// webSearchUpstreamCode 读取上游业务码。
// 返回的 bool 表示「响应里确实出现了 code 字段」；成功响应没有该字段。
func webSearchUpstreamCode(obj map[string]any) (int, bool) {
	value, ok := obj["code"]
	if !ok {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return int(v), true
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, false
		}
		// 非数字 code 视为业务错误包络，message 由 msg/message 提供。
		return -1, true
	case nil:
		return 0, false
	default:
		return -1, true
	}
}

func webSearchUpstreamMessage(obj map[string]any) string {
	if msg := webSearchString(obj["msg"]); msg != "" {
		return msg
	}
	return webSearchString(obj["message"])
}

// webSearchResults 归一化结果数组。
//
// 上游可能给出「只有 site 没有 url」的站点型结果（实测行情/数据库类结果 url 为空串）。
// 但 DSH 的 mapSearchResponse 要求 url 必须是非空字符串，否则整个响应被判非法，
// 所以这里把无 url 的条目降级成 site 首页链接；实在无法构造时才丢弃。
//
// DSH requires string title, url, and snippet fields, plus a nullable date.
func webSearchResults(obj map[string]any) []map[string]any {
	list, _ := obj["results"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		site := webSearchString(raw["site"])
		url := strings.TrimSpace(webSearchString(raw["url"]))
		if url == "" {
			url = webSearchSiteFallbackURL(site)
		}
		if url == "" {
			// 既无 url 也无法从 site 推断，DSH 侧必然判非法，直接丢弃更诚实。
			continue
		}
		out = append(out, map[string]any{
			"title":   webSearchString(raw["title"]),
			"url":     url,
			"snippet": webSearchString(raw["snippet"]),
			"date":    nil,
		})
	}
	return out
}

// webSearchSiteFallbackURL 把上游只给 site 名称的结果补成可点击链接。
// site 可能是域名（go.dev）也可能是中文名（股票数据库），后者无法构造 URL 时返回空。
func webSearchSiteFallbackURL(site string) string {
	site = strings.TrimSpace(site)
	if site == "" || strings.ContainsAny(site, " \t") {
		return ""
	}
	// 含点的才当成域名；纯中文名不当域名处理。
	if !strings.Contains(site, ".") {
		return ""
	}
	return "https://" + strings.Trim(site, ".")
}

func webSearchString(value any) string {
	s, _ := value.(string)
	return s
}

func baseNameOf(acc *Account) string {
	if acc == nil {
		return ""
	}
	if idx := strings.LastIndexAny(acc.Path, `/\`); idx >= 0 {
		return acc.Path[idx+1:]
	}
	return acc.Path
}

// utf8Len 按字符数计数，避免中文查询被按字节长度误判为过长。
func utf8Len(s string) int {
	return len([]rune(s))
}
