package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func resetStatsState(t *testing.T) {
	t.Helper()
	requestStats.Lock()
	requestStats.document = statsDocument{Schema: statsSchema, Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}
	requestStats.revision, requestStats.saved, requestStats.lastSaved = 0, 0, 0
	requestStats.blocked, requestStats.enabled, requestStats.mode, requestStats.problem = false, false, "pending", ""
	requestStats.Unlock()
	t.Cleanup(func() {
		requestStats.Lock()
		requestStats.document = statsDocument{Schema: statsSchema, Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}
		requestStats.revision, requestStats.saved, requestStats.lastSaved = 0, 0, 0
		requestStats.blocked, requestStats.enabled, requestStats.mode, requestStats.problem = false, false, "pending", ""
		requestStats.Unlock()
	})
}

func statsTestRequest(account, model string, at time.Time, tokens *int64, credit *float64) statsRequest {
	return statsRequest{Account: account, Model: model, At: at.UnixMilli(), Status: 200, Complete: true,
		TotalTokens: tokens, Credit: credit, LatencyMS: 1000}
}

func int64p(v int64) *int64       { return &v }
func float64p(v float64) *float64 { return &v }

func statsQuery(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleWebUIStats(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats?"+query, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil && rec.Code == http.StatusOK {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	return rec.Code, body
}

// 缺文件时静默启用，从空统计开始，不影响网关运行。
func TestStatsMissingFileStartsEmpty(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	defer requestStats.Unlock()
	if !requestStats.enabled || requestStats.blocked || requestStats.mode != "pending" {
		t.Fatalf("缺文件应静默启用: %+v", requestStats.mode)
	}
	if len(requestStats.document.Requests) != 0 || len(requestStats.document.Accounts) != 0 {
		t.Fatal("缺文件时应从空统计开始")
	}
}

// 坏文件不得被覆盖，也不得阻断运行。
func TestStatsCorruptFileIsPreserved(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	const broken = `{"schema":1,"requests":`
	if err := os.WriteFile(statsFile, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	loadOptionalStats()
	requestStats.Lock()
	blocked, mode := requestStats.blocked, requestStats.mode
	requestStats.Unlock()
	if !blocked || mode != "memory" {
		t.Fatalf("坏文件应转为仅内存: blocked=%t mode=%s", blocked, mode)
	}
	// 追加数据后尝试落盘：必须保留原文件，不得覆盖。
	requestStats.Lock()
	requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
		Account: "a", Model: "m", At: time.Now().UnixMilli(), Status: 200, LatencyMS: 1})
	requestStats.revision++
	requestStats.Unlock()
	// 明确跳过写入（而不是报错重试）：坏文件必须原样保留。
	if err := flushOptionalStats(); err != nil {
		t.Fatalf("坏文件状态应静默跳过落盘，实际: %v", err)
	}
	raw, err := os.ReadFile(statsFile)
	if err != nil || string(raw) != broken {
		t.Fatalf("原文件被改动: %q err=%v", string(raw), err)
	}
}

func TestStatsValidFileRestores(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	at := time.Now().Add(-time.Hour).UnixMilli()
	// 统计标识由「文件 + 站点 + UID + 企业 ID」派生，这里用同一账号算出真实标识。
	accountMu.Lock()
	acc := &Account{Path: "workbuddy-a.json", Edition: "cn", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "u1"}}}
	id, view := statsAccountIDLocked(acc)
	accountMu.Unlock()
	view.ModelStates = map[string]modelStateSnapshot{
		"glm-5.3-flash": {CostClass: modelCostPaid, QuotaBlocked: true}}
	doc := statsDocument{Schema: statsSchema, StartedAt: at,
		Accounts: map[string]statsAccount{id: view},
		Requests: []statsRequest{{Account: id, Model: "glm-5.3-flash", At: at, Status: 200,
			Complete: true, TotalTokens: int64p(120), LatencyMS: 500}}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(statsFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loadOptionalStats()
	requestStats.Lock()
	got := requestStats.document
	mode := requestStats.mode
	requestStats.Unlock()
	if mode != "file" || len(got.Requests) != 1 || len(got.Accounts) != 1 {
		t.Fatalf("有效文件未恢复: mode=%s requests=%d accounts=%d", mode, len(got.Requests), len(got.Accounts))
	}
	// 恢复的模型账本必须落回账号，且保持原有收费属性。
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	restoreStatsAccountStatesLocked()
	accounts = oldAccounts
	accountMu.Unlock()
	state := acc.ModelStates["glm-5.3-flash"]
	if state == nil || state.CostClass != modelCostPaid || !state.QuotaBlocked {
		t.Fatalf("模型账本未按账号恢复: %+v", state)
	}
}

func TestAggregateModelStatsWindow(t *testing.T) {
	resetStatsState(t)
	now := time.Now()
	tokensA, tokensB := int64(100), int64(50)
	ttft := 800.0
	requestStats.Lock()
	requestStats.document.Requests = []statsRequest{
		{Account: "a", Model: "m1", At: now.Add(-time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokensA, LatencyMS: 2000, TTFTMS: &ttft, TTFTMetric: ttftMetric},
		{Account: "a", Model: "m1", At: now.Add(-2 * time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokensA, LatencyMS: 4000, TTFTMS: &ttft, TTFTMetric: ttftMetric},
		{Account: "a", Model: "m1", At: now.Add(-6 * time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokensB, LatencyMS: 9000},
		{Account: "a", Model: "m2", At: now.Add(-time.Hour).UnixMilli(), Status: 500, Complete: false,
			LatencyMS: 1000},
	}
	requestStats.Unlock()

	window := aggregateModelStatsWindow(now.Add(-5*time.Hour), now)
	m1, ok := window["m1"]
	if !ok || m1.Requests != 2 || m1.Success != 2 || m1.Failed != 0 || m1.Tokens != 200 {
		t.Fatalf("m1 窗口聚合错误: %+v", m1)
	}
	if m1.AvgTTFTMS == nil || *m1.AvgTTFTMS != 800 {
		t.Fatalf("m1 首Token均值错误: %+v", m1.AvgTTFTMS)
	}
	if m1.AvgLatencyMS == nil || *m1.AvgLatencyMS != 3000 {
		t.Fatalf("m1 总耗时均值错误: %+v", m1.AvgLatencyMS)
	}
	m2, ok := window["m2"]
	if !ok || m2.Requests != 1 || m2.Failed != 1 || m2.AvgTTFTMS != nil {
		t.Fatalf("m2 失败请求不应计入首Token: %+v", m2)
	}
}

func TestHandleWebUIModelsRange5hUsesPersistedStats(t *testing.T) {
	isolatedGatewayConfig(t)
	resetStatsState(t)
	now := time.Now()
	snap := statusSnapshot{UpdatedAt: now.Unix(), ModelCatalog: []modelStatSnapshot{
		{ID: "m1", Requests: 999, Tokens: 999999, HasTTFT: true, AvgTTFTMs: 9999, HasLatency: true, AvgLatencyMs: 9999},
		{ID: "m2", Requests: 0},
	}}
	raw, _ := json.Marshal(snap)
	if err := os.WriteFile(statusSnapshotFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	tokens := int64(100)
	ttft := 800.0
	requestStats.Lock()
	requestStats.document.Requests = []statsRequest{
		{Account: "a", Model: "m1", At: now.Add(-time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokens, LatencyMS: 2000, TTFTMS: &ttft, TTFTMetric: ttftMetric},
		{Account: "a", Model: "m1", At: now.Add(-6 * time.Hour).UnixMilli(), Status: 200, Complete: true,
			TotalTokens: &tokens, LatencyMS: 9000},
	}
	requestStats.Unlock()

	rec := httptest.NewRecorder()
	handleWebUIModels(rec, httptest.NewRequest(http.MethodGet, "/admin/api/models?range=5h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("range=5h 状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Range  string              `json:"range"`
		Models []modelStatSnapshot `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Range != "5h" {
		t.Fatalf("响应未标注统计窗口: %#v", body.Range)
	}
	byID := map[string]modelStatSnapshot{}
	for _, m := range body.Models {
		byID[m.ID] = m
	}
	m1 := byID["m1"]
	if m1.Requests != 1 || m1.Tokens != 100 || m1.AvgTTFTMs != 800 || !m1.HasTTFT ||
		m1.AvgLatencyMs != 2000 || !m1.HasLatency {
		t.Fatalf("模型列表没有复用 stats.json 窗口: %+v", m1)
	}
	if m2 := byID["m2"]; m2.Requests != 0 || m2.HasTTFT || m2.HasLatency {
		t.Fatalf("窗口内无调用的模型应显示空统计: %+v", m2)
	}

	rec = httptest.NewRecorder()
	handleWebUIModels(rec, httptest.NewRequest(http.MethodGet, "/admin/api/models?range=24h", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 range 应返回 400: %d %s", rec.Code, rec.Body.String())
	}
}

// 每次实际账号调用记一次，失败重试分开；未选号的拒绝不记。
func TestStatsRecordsAttemptWithUsageAndTiming(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	body := newTTFTReader(strings.NewReader("data: x"), time.Now().Add(-300*time.Millisecond))
	_, _ = body.Read(make([]byte, 4))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	attempt := &statsAttempt{id: "acct", account: statsAccount{File: "workbuddy-a.json", Edition: "cn"},
		model: "glm-5.3-flash", started: time.Now().Add(-1200 * time.Millisecond)}
	r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, attempt))
	observeFirstToken(body, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "hello"}}}})
	completeTokenStream(body, true)
	finishFirstToken(r, "glm-5.3-flash", 1, body)
	statsObserveUsage(r, map[string]any{"total_tokens": float64(300), "prompt_tokens": float64(200),
		"completion_tokens": float64(100), "credit": 1.25}, true)
	statsFinishAttempt(r, http.StatusOK)
	requestStats.Lock()
	defer requestStats.Unlock()
	if len(requestStats.document.Requests) != 1 {
		t.Fatalf("应记录 1 条: %d", len(requestStats.document.Requests))
	}
	row := requestStats.document.Requests[0]
	if row.TotalTokens == nil || *row.TotalTokens != 300 || row.Credit == nil || *row.Credit != 1.25 {
		t.Fatalf("usage 未落盘: %+v", row)
	}
	if row.TTFTMS == nil || *row.TTFTMS <= 0 {
		t.Fatalf("首字耗时未记录: %+v", row.TTFTMS)
	}
	if row.LatencyMS <= 0 {
		t.Fatalf("总耗时未记录: %v", row.LatencyMS)
	}
	if _, ok := requestStats.document.Accounts["acct"]; !ok {
		t.Fatal("账号选择时必须登记账号身份")
	}
}

// 未选到账号的拒绝（鉴权、参数错误）不产生账号消耗记录。
func TestStatsIgnoresRequestsWithoutAccountSelection(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, &statsAttempt{}))
	statsFinishAttempt(r, http.StatusBadRequest)
	requestStats.Lock()
	defer requestStats.Unlock()
	if len(requestStats.document.Requests) != 0 {
		t.Fatalf("未选号不得记录: %+v", requestStats.document.Requests)
	}
}

// 缺失 usage 记为空样本，而不是零消耗；0 积分与未知积分必须可区分。
func TestStatsDistinguishesUnknownFromZero(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	now := time.Now()
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Requests = []statsRequest{
		statsTestRequest("a", "m", now, nil, nil),
		statsTestRequest("a", "m", now, int64p(0), float64p(0)),
	}
	requestStats.Unlock()
	code, body := statsQuery(t, "period=day&account=a")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	summary := body["summary"].(map[string]any)
	if summary["tokenSamples"] != float64(1) || summary["tokens"] != float64(0) {
		t.Fatalf("未知 Token 与零 Token 未区分: %+v", summary)
	}
	if summary["creditSamples"] != float64(1) || summary["credits"] != float64(0) {
		t.Fatalf("未知积分与零积分未区分: %+v", summary)
	}
	if summary["requests"] != float64(2) {
		t.Fatalf("请求数应包含全部调用: %+v", summary)
	}
}

// UTC+8 日历：小时对齐整点，周从周一开始，月按自然月；账号维度互不串数据。
func TestStatsBucketsUseUTC8AndIsolateAccounts(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	// 2026-10-14 是周三；周一应为 2026-10-12。
	monday := time.Date(2026, 10, 12, 9, 30, 0, 0, statsZone)
	sunday := time.Date(2026, 10, 18, 23, 30, 0, 0, statsZone)
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Accounts["b"] = statsAccount{File: "b.json", Edition: "intl"}
	requestStats.document.Requests = []statsRequest{
		statsTestRequest("a", "m", monday, int64p(100), float64p(1)),
		statsTestRequest("a", "m", sunday, int64p(200), float64p(2)),
		statsTestRequest("b", "m", monday, int64p(999), float64p(9)),
	}
	requestStats.Unlock()
	code, body := statsQuery(t, "period=week&account=a&start=2026-10-01&end=2026-10-31")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	buckets := body["buckets"].([]any)
	var total float64
	for _, raw := range buckets {
		row := raw.(map[string]any)
		if row["label"] == "2026-10-12" {
			if row["tokens"] != float64(300) {
				t.Fatalf("周一桶应合并周一与周日: %+v", row)
			}
			total += row["tokens"].(float64)
		}
	}
	if total != 300 {
		t.Fatalf("账号维度未隔离，出现其它账号数据: total=%v", total)
	}
	if body["summary"].(map[string]any)["tokens"] != float64(300) {
		t.Fatalf("汇总未按账号过滤: %+v", body["summary"])
	}
}

// 明细按模型排名：只列有调用的模型，用量（Token）多的在前，
// Token 未知的按请求数兜底，且跟随账号与时间范围过滤。
func TestStatsModelRanking(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	today := time.Now().In(statsZone).Format("2006-01-02")
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Accounts["b"] = statsAccount{File: "b.json", Edition: "intl"}
	requestStats.document.Requests = []statsRequest{
		statsTestRequest("a", "small-model", time.Now(), int64p(100), float64p(1)),
		statsTestRequest("a", "big-model", time.Now(), int64p(9000), float64p(5)),
		statsTestRequest("a", "mid-model", time.Now(), int64p(3000), float64p(2)),
		// 只有调用、没有 usage：仍要列出，靠请求数排在最后。
		statsTestRequest("a", "no-usage", time.Now(), nil, nil),
		// 超出所选日期范围：不得进入排名。
		statsTestRequest("a", "old-model", time.Now().AddDate(0, 0, -40), int64p(999999), float64p(99)),
		// 另一个账号：全局可见，单账号过滤后不可见。
		statsTestRequest("b", "intl-model", time.Now(), int64p(5000), float64p(3)),
	}
	requestStats.Unlock()
	names := func(body map[string]any) []string {
		t.Helper()
		raw, ok := body["models"].([]any)
		if !ok {
			t.Fatalf("响应缺少 models 字段: %v", body["models"])
		}
		out := make([]string, 0, len(raw))
		for _, row := range raw {
			out = append(out, row.(map[string]any)["model"].(string))
		}
		return out
	}
	// 单账号 + 当天：按 Token 降序，无用量模型最后。
	code, body := statsQuery(t, "period=day&account=a&start="+today+"&end="+today)
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	want := []string{"big-model", "mid-model", "small-model", "no-usage"}
	if got := names(body); len(got) != len(want) {
		t.Fatalf("排名行数错误: %v", got)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("排名错误: got %v want %v", got, want)
			}
		}
	}
	// 全局：包含另一账号的模型，仍不含超范围模型。
	if code, body := statsQuery(t, "period=day&start="+today+"&end="+today); code != http.StatusOK {
		t.Fatalf("status=%d", code)
	} else if got := names(body); len(got) != 5 || got[0] != "big-model" {
		t.Fatalf("全局排名错误: %v", got)
	}
}

// 耗时趋势必须按模型分开。同一时间桶里不同模型不能合成一个平均值，
// 某模型没有调用的桶是 null，并跟随账号与日期过滤。
func TestStatsLatencySeriesStaysPerModel(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	day1 := time.Date(2026, 10, 13, 10, 0, 0, 0, statsZone)
	day2 := time.Date(2026, 10, 14, 11, 0, 0, 0, statsZone)
	req := func(account, model string, at time.Time, tokens int64, latency, ttft float64) statsRequest {
		row := statsTestRequest(account, model, at, int64p(tokens), nil)
		row.LatencyMS = latency
		row.TTFTMS, row.TTFTMetric, row.Complete, row.Status = &ttft, ttftMetric, true, 200
		return row
	}
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Accounts["b"] = statsAccount{File: "b.json", Edition: "intl"}
	requestStats.document.Requests = []statsRequest{
		req("a", "model-fast", day1, 10, 100, 30),
		req("a", "model-fast", day1, 10, 300, 50),
		req("a", "model-fast", day2, 10, 300, 60),
		req("a", "model-slow", day1, 50, 900, 300),
		req("b", "other-model", day1, 999, 50, 20),
		req("a", "old-model", time.Date(2026, 10, 1, 10, 0, 0, 0, statsZone), 999999, 10, 10),
	}
	requestStats.Unlock()

	code, body := statsQuery(t, "period=day&account=a&start=2026-10-13&end=2026-10-14")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%v", code, body)
	}
	raw, _ := body["modelLatency"].([]any)
	if len(raw) != 2 {
		t.Fatalf("应只保留所选账号和日期内有调用的模型: %+v", raw)
	}
	if raw[0].(map[string]any)["model"] != "model-slow" || raw[1].(map[string]any)["model"] != "model-fast" {
		t.Fatalf("线条顺序应与用量排名一致: %+v", raw)
	}
	slow := raw[0].(map[string]any)["avgLatencyMs"].([]any)
	fast := raw[1].(map[string]any)["avgLatencyMs"].([]any)
	if len(slow) != 2 || slow[0] != float64(900) || slow[1] != nil {
		t.Fatalf("慢模型第二天没有调用，必须是 null，不能画成 0: %+v", slow)
	}
	if fast[0] != float64(200) || fast[1] != float64(300) {
		t.Fatalf("同模型应单独平均，不能和慢模型合成一个值: %+v", fast)
	}
	slowTtft := raw[0].(map[string]any)["avgTtftMs"].([]any)
	fastTtft := raw[1].(map[string]any)["avgTtftMs"].([]any)
	if len(slowTtft) != 2 || slowTtft[0] != float64(300) || slowTtft[1] != nil {
		t.Fatalf("首字序列也要按模型分桶，缺失桶为 null: %+v", slowTtft)
	}
	if fastTtft[0] != float64(40) || fastTtft[1] != float64(60) {
		t.Fatalf("首字不能把不同模型合成一个值: %+v", fastTtft)
	}
	blended := body["buckets"].([]any)[0].(map[string]any)["avgLatencyMs"].(float64)
	if blended <= 400 || blended >= 500 {
		t.Fatalf("时间桶总平均应仍是全部调用的平均，实际 %v", blended)
	}
	if code, body = statsQuery(t, "period=day&account=b&start=2026-10-13&end=2026-10-14"); code != http.StatusOK {
		t.Fatalf("status=%d", code)
	} else if rows, _ := body["modelLatency"].([]any); len(rows) != 1 || rows[0].(map[string]any)["model"] != "other-model" {
		t.Fatalf("账号过滤未作用到耗时线: %+v", rows)
	}
}

// 首字序列只收 v2 口径、完整且成功的调用；旧口径、未完成、失败都不能混进柱形。
func TestStatsModelTTFTSeriesIgnoresLegacyAndIncomplete(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	day := time.Date(2026, 10, 13, 10, 0, 0, 0, statsZone)
	row := func(metric string, complete bool, status int, ttft float64) statsRequest {
		r := statsTestRequest("a", "m", day, int64p(10), nil)
		r.LatencyMS = 100
		r.TTFTMS, r.TTFTMetric, r.Complete, r.Status = &ttft, metric, complete, status
		return r
	}
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Requests = []statsRequest{
		row(ttftMetric, true, 200, 100),
		row("", true, 200, 900),          // 旧口径，不能进入首字柱形
		row(ttftMetric, false, 200, 900), // 不完整
		row(ttftMetric, true, 429, 900),  // 失败
	}
	requestStats.Unlock()
	code, body := statsQuery(t, "period=day&account=a&start=2026-10-13&end=2026-10-13")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%v", code, body)
	}
	rows, _ := body["modelLatency"].([]any)
	if len(rows) != 1 {
		t.Fatalf("models=%+v", rows)
	}
	series, _ := rows[0].(map[string]any)["avgTtftMs"].([]any)
	if len(series) != 1 || series[0] != float64(100) {
		t.Fatalf("首字柱形只能收 v2、完整、成功的样本: %+v", series)
	}
	latency, _ := rows[0].(map[string]any)["avgLatencyMs"].([]any)
	if len(latency) != 1 || latency[0] != float64(100) {
		t.Fatalf("总耗时折线仍是该桶全部调用的平均: %+v", latency)
	}
}

func TestStatsBucketStartBoundaries(t *testing.T) {
	at := time.Date(2026, 10, 14, 23, 59, 59, 0, statsZone)
	if got := statsBucketStart(at, "hour"); !got.Equal(time.Date(2026, 10, 14, 23, 0, 0, 0, statsZone)) {
		t.Fatalf("小时桶错误: %v", got)
	}
	if got := statsBucketStart(at, "day"); !got.Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, statsZone)) {
		t.Fatalf("日桶错误: %v", got)
	}
	// 周日 00:00 必须归到本周一，而不是下周一。
	sunday := time.Date(2026, 10, 18, 0, 0, 0, 0, statsZone)
	if got := statsBucketStart(sunday, "week"); !got.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, statsZone)) {
		t.Fatalf("周桶错误: %v", got)
	}
	if got := statsBucketStart(at, "month"); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, statsZone)) {
		t.Fatalf("月桶错误: %v", got)
	}
}

// 账号身份变化（UID / 企业 ID / 站点）必须换统计标识，避免继承旧账本。
func TestStatsAccountIdentityFollowsCredentials(t *testing.T) {
	accountMu.Lock()
	defer accountMu.Unlock()
	base := &Account{Path: "workbuddy-a.json", Edition: "cn", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-1"}}}
	first, _ := statsAccountIDLocked(base)
	same := *base.Auth
	renewed := &Account{Path: base.Path, Edition: "cn", Auth: &StoredAuth{
		Edition: "cn", Account: same.Account, Auth: StoredTokens{AccessToken: "rotated"}}}
	if second, _ := statsAccountIDLocked(renewed); second != first {
		t.Fatal("令牌轮换不应改变统计归属")
	}
	// 用新构造的账号表达变更，避免复制含锁的 Account 值。
	for name, changed := range map[string]*Account{
		"换账号": {Path: base.Path, Edition: "cn", Auth: &StoredAuth{
			Edition: "cn", Account: StoredAccount{UID: "u2", EnterpriseID: "ent-1"}}},
		"换企业": {Path: base.Path, Edition: "cn", Auth: &StoredAuth{
			Edition: "cn", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-2"}}},
		"换站点": {Path: base.Path, Edition: "intl", Auth: &StoredAuth{
			Edition: "intl", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-1"}}},
		"换文件": {Path: "workbuddy-b.json", Edition: "cn", Auth: &StoredAuth{
			Edition: "cn", Account: StoredAccount{UID: "u1", EnterpriseID: "ent-1"}}},
	} {
		if id, _ := statsAccountIDLocked(changed); id == first {
			t.Fatalf("%s 后仍沿用旧统计标识", name)
		}
	}
}

// 落盘失败必须降级为仅内存，并且不影响后续统计与查询。
func TestStatsWriteFailureDegradesToMemory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("目录只读语义在 Windows 上不稳定，改由符号链接与坏文件用例覆盖")
	}
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	if err := os.Mkdir(statsFile, 0700); err != nil {
		t.Fatal(err)
	}
	requestStats.Lock()
	requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
		Account: "a", Model: "m", At: time.Now().UnixMilli(), Status: 200, LatencyMS: 1})
	requestStats.revision++
	requestStats.Unlock()
	if err := flushOptionalStats(); err == nil {
		t.Fatal("路径被目录占用时应落盘失败")
	}
	requestStats.Lock()
	mode, problem := requestStats.mode, requestStats.problem
	requestStats.Unlock()
	if mode != "memory" || problem == "" {
		t.Fatalf("应降级为仅内存并给出白话原因: mode=%s problem=%s", mode, problem)
	}
	if code, _ := statsQuery(t, "period=day"); code != http.StatusOK {
		t.Fatalf("落盘失败不得影响查询: %d", code)
	}
}

// 正常落盘后重新加载必须得到同样的记录，且文件权限不放宽。
func TestStatsRoundTripPersists(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "workbuddy-a.json", Edition: "cn"}
	requestStats.document.Requests = append(requestStats.document.Requests,
		statsTestRequest("a", "glm-5.3-flash", time.Now(), int64p(42), float64p(0.5)))
	requestStats.revision++
	requestStats.Unlock()
	if err := flushOptionalStats(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statsFile)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("stats.json 未生成: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("权限应为 0600，实际 %o", perm)
		}
	}
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	defer requestStats.Unlock()
	if len(requestStats.document.Requests) != 1 || *requestStats.document.Requests[0].TotalTokens != 42 {
		t.Fatalf("重新加载丢失记录: %+v", requestStats.document.Requests)
	}
}

// 统计接口只读，写入方法必须被拒绝。
func TestStatsEndpointRejectsWrites(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	rec := httptest.NewRecorder()
	handleWebUIStats(rec, httptest.NewRequest(http.MethodPost, "/admin/api/stats", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("写入统计应被拒绝: %d", rec.Code)
	}
	for _, query := range []string{"period=year", "period=day&start=bad", "period=day&account=missing"} {
		if code, _ := statsQuery(t, query); code != http.StatusBadRequest {
			t.Fatalf("非法查询 %s 应返回 400，实际 %d", query, code)
		}
	}
}

// 落盘只由后台循环负责，且取消上下文时必须冲刷一次。
func TestStatsPersistenceLoopFlushesOnCancel(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json", Edition: "cn"}
	requestStats.document.Requests = append(requestStats.document.Requests,
		statsTestRequest("a", "m", time.Now(), int64p(7), nil))
	requestStats.revision++
	requestStats.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); statsPersistenceLoop(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("统计落盘循环未在取消时退出")
	}
	if _, err := os.Stat(filepath.Clean(statsFile)); err != nil {
		t.Fatalf("取消时必须冲刷统计: %v", err)
	}
}
