package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetRateDetailCache(t *testing.T) {
	t.Helper()
	rateDetailCache.Lock()
	rateDetailCache.payload, rateDetailCache.at = nil, time.Time{}
	rateDetailCache.Unlock()
}

func TestParseRateLineCoversEveryLoggedSource(t *testing.T) {
	const prefix = "2026/10/13 10:59:36 "
	cases := []struct {
		name      string
		line      string
		site      string
		model     string
		value     string
		unknown   bool
		priority  int
		sourceHas string
	}{
		{
			name: "官方倍率-促销已过期",
			line: prefix + `[Models] 官方倍率 站点=intl 模型=hy4-preview 原始credits="x0.00" 生效倍率=0 有倍率=true 促销="Free now" 促销截止="2026-09-08T00:00:00+08:00" 促销已过期=true 来源=实时接口 刷新时间=2026-10-13T10:59:36`,
			site: "intl", model: "hy4-preview", value: "-", unknown: true,
			priority: ratePriorityPromoStale, sourceHas: "官方目录",
		},
		{
			name: "官方倍率-正常数值",
			line: prefix + `[Models] 官方倍率 站点=cn 模型=hy3 原始credits="x0.29" 生效倍率=0.29 有倍率=true 促销="" 促销截止="" 促销已过期=false 来源=实时接口 刷新时间=2026-10-13T10:59:36`,
			site: "cn", model: "hy3", value: "0.29x", unknown: false,
			priority: ratePriorityOfficial, sourceHas: "官方目录",
		},
		{
			name: "探测缓存结论-收费",
			line: prefix + `[ModelPrice] 载入缓存结论 站点=intl 模型=hy4-preview 判定=paid credit=0.05 tokens=385 探测时间=2026-10-02T00:23:40 说明=usage.credit>0 收费 来源=缓存文件（非本轮新探测）`,
			site: "intl", model: "hy4-preview", value: "收费(倍率未知)", unknown: true,
			priority: ratePriorityProbePaid, sourceHas: "缓存结论",
		},
		{
			name: "探测实测-免费",
			line: prefix + `[ModelPrice] 站点=cn 账号=workbuddy1.json 模型=hy3 -> verdict=free credit=0 tokens=420 (usage.credit=0 免费)`,
			site: "cn", model: "hy3", value: "0.00x", unknown: false,
			priority: ratePriorityProbeFree, sourceHas: "本机实测",
		},
		{
			name: "正常请求实测-收费",
			line: prefix + `[ModelCost] requestId=3472 账号 workbuddy2.json 站点=intl 模型 hy4-preview 实测 usage.credit=0.05，已学习为收费模型`,
			site: "intl", model: "hy4-preview", value: "实测收费", unknown: false,
			priority: ratePriorityLearnedPaid, sourceHas: "正常请求实测",
		},
		{
			name: "正常请求实测-免费",
			line: prefix + `[ModelCost] requestId=84 账号 workbuddy2.json 站点=cn 模型 hy3 实测 usage.credit=0，已学习为免费模型`,
			site: "cn", model: "hy3", value: "0.00x", unknown: false,
			priority: ratePriorityLearnedFree, sourceHas: "正常请求实测",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record, ok := parseRateLine(tc.line, "gateway-2026-10-13.log")
			if !ok {
				t.Fatalf("未解析: %s", tc.line)
			}
			if record.Site != tc.site || record.Model != tc.model || record.Value != tc.value ||
				record.Unknown != tc.unknown || record.Priority != tc.priority {
				t.Fatalf("解析结果不符: %+v", record)
			}
			if !strings.Contains(record.Source, tc.sourceHas) {
				t.Fatalf("来源=%q 期望包含 %q", record.Source, tc.sourceHas)
			}
			if record.UpdatedAt != time.Date(2026, 10, 13, 10, 59, 36, 0, time.Local).Unix() {
				t.Fatalf("时间解析错误: %d", record.UpdatedAt)
			}
			if record.LogLine == "" || record.LogFile != "gateway-2026-10-13.log" {
				t.Fatal("缺少日志出处")
			}
			// 页面用这句白话解释代替大段日志，必须存在且简短。
			if record.Explain == "" {
				t.Fatal("缺少面向用户的简短解释")
			}
			if len([]rune(record.Explain)) > 60 {
				t.Fatalf("解释过长，用户难以一眼看懂: %q", record.Explain)
			}
		})
	}
	for _, line := range []string{
		"2026/10/13 10:59:36 [Models] 实时目录成功：站点=intl，模型数=18，生效促销=0",
		"2026/10/13 10:59:36 [请求到达] traceId=abc 方法=GET 路径=/v1/models",
		"[Models] 官方倍率 站点=cn 模型=hy3",
	} {
		if _, ok := parseRateLine(line, "gateway-2026-10-13.log"); ok {
			t.Fatalf("不应解析无关日志: %s", line)
		}
	}
}

func TestAggregateRateDetailsMatchesDisplayPriority(t *testing.T) {
	now := time.Now().Unix()
	records := []rateLogRecord{
		{Site: "intl", Model: "hy4-preview", Source: "官方目录（实时接口）", UpdatedAt: now, Value: "-",
			Unknown: true, Priority: ratePriorityPromoStale, Detail: "促销已过期"},
		{Site: "intl", Model: "hy4-preview", Source: "价格探测（缓存结论）", UpdatedAt: now - 100,
			Value: "收费(倍率未知)", Unknown: true, Priority: ratePriorityProbePaid, Detail: "credit=0.05"},
		{Site: "cn", Model: "hy3", Source: "官方目录（实时接口）", UpdatedAt: now, Value: "0.29x", Priority: ratePriorityOfficial},
		{Site: "cn", Model: "hy3", Source: "正常请求实测", UpdatedAt: now - 50, Value: "0.00x", Priority: ratePriorityLearnedFree},
		{Site: "intl", Model: "hy3", Source: "官方目录（实时接口）", UpdatedAt: now, Value: "0.50x", Priority: ratePriorityOfficial},
		{Site: "intl", Model: "hy3", Source: "正常请求实测", UpdatedAt: now, Value: "实测收费", Priority: ratePriorityLearnedPaid},
	}
	rates := aggregateRateDetails(records)
	if len(rates) != 3 {
		t.Fatalf("分组数=%d", len(rates))
	}
	intl := rates["intl|hy4-preview"]
	if intl["source"] != "价格探测（缓存结论）" || intl["value"] != "收费(倍率未知)" || intl["unknown"] != true {
		t.Fatalf("实测结论应优先于促销过期: %+v", intl)
	}
	if list, ok := intl["records"].([]rateLogRecord); !ok || len(list) != 2 {
		t.Fatalf("应保留全部来源记录: %+v", intl["records"])
	}
	cn := rates["cn|hy3"]
	if cn["source"] != "正常请求实测" || cn["value"] != "0.00x" {
		t.Fatalf("免费实测应优先: %+v", cn)
	}
	if cn["hasLog"] != true {
		t.Fatal("有记录时必须标记 hasLog")
	}
	if intlHy3 := rates["intl|hy3"]; intlHy3["source"] != "官方目录（实时接口）" || intlHy3["value"] != "0.50x" {
		t.Fatalf("实测收费只作佐证，不应覆盖官方倍率: %+v", intlHy3)
	}
}

func TestRateDetailEndpointReadsLogsAndReportsMissingLog(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	handler := newWebUIHandler()
	path := filepath.Join(logDir, "gateway-2026-10-13.log")
	lines := strings.Join([]string{
		`2026/10/13 10:59:32 [ModelPrice] 载入缓存结论 站点=intl 模型=hy4-preview 判定=paid credit=0.05 tokens=385 探测时间=2026-10-02T00:23:40 说明=usage.credit>0 收费 来源=缓存文件（非本轮新探测）`,
		`2026/10/13 10:59:36 [Models] 官方倍率 站点=cn 模型=hy3 原始credits="x0.29" 生效倍率=0.29 有倍率=true 促销="" 促销截止="" 促销已过期=false 来源=实时接口 刷新时间=2026-10-13T10:59:36`,
		`2026/10/13 10:59:36 [Models] 实时目录成功：站点=intl，模型数=18，生效促销=0`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "feature-api-key"} {
		if w := adminRequest(handler, "GET", "/admin/api/rate-detail", key, ""); w.Code != http.StatusUnauthorized {
			t.Fatal("倍率来源接口绕过管理鉴权")
		}
	}
	if w := adminRequest(handler, "POST", "/admin/api/rate-detail", "feature-admin-key", "{}"); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("倍率来源接口应只读")
	}
	w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("读取失败: %d %s", w.Code, w.Body.String())
	}
	var payload struct {
		Rates       map[string]map[string]any `json:"rates"`
		RecordCount int                       `json:"recordCount"`
		Source      string                    `json:"source"`
		Note        string                    `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RecordCount != 2 {
		t.Fatalf("只应解析价格相关日志: %+v", payload)
	}
	if payload.Rates["intl|hy4-preview"]["value"] != "收费(倍率未知)" {
		t.Fatalf("探测结论未生效: %+v", payload.Rates)
	}
	if payload.Rates["cn|hy3"]["value"] != "0.29x" {
		t.Fatalf("官方倍率未生效: %+v", payload.Rates)
	}
	if _, exists := payload.Rates["cn|not-in-log"]; exists {
		t.Fatal("没有日志的模型不应出现在结果里")
	}
	if !strings.Contains(payload.Note, "没有日志") {
		t.Fatal("接口应说明没有日志时的展示方式")
	}
	var hints struct {
		RefreshHint     string `json:"refreshHint"`
		NoLogHint       string `json:"noLogHint"`
		AutoProbeKnown  bool   `json:"autoProbeKnown"`
		AutoProbeEnable bool   `json:"autoProbeEnabled"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &hints); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hints.RefreshHint, "请求一次") || !strings.Contains(hints.NoLogHint, "请求一次") {
		t.Fatalf("未知与无日志都必须给出可执行的刷新方式: %+v", hints)
	}
	var detailed struct {
		Rates map[string]map[string]any `json:"rates"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &detailed)
	if unknown := detailed.Rates["intl|hy4-preview"]; unknown["refreshHint"] == nil || unknown["explain"] == "" {
		t.Fatalf("未知倍率必须带刷新说明和简短解释: %+v", unknown)
	}
	if known := detailed.Rates["cn|hy3"]; known["refreshHint"] != nil {
		t.Fatalf("已知倍率不应显示刷新说明: %+v", known)
	}
	// 命中缓存后不再重扫；缓存过期后必须重新读取新日志。
	if err := os.WriteFile(path, []byte(`2026/10/13 11:30:00 [Models] 官方倍率 站点=cn 模型=hy3 原始credits="x0.50" 生效倍率=0.50 有倍率=true 促销="" 促销截止="" 促销已过期=false 来源=实时接口 刷新时间=2026-10-13T11:30:00`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", ""); !strings.Contains(w.Body.String(), "0.29x") {
		t.Fatal("短时间内应命中缓存，避免每次点击都重扫日志")
	}
	resetRateDetailCache(t)
	if w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", ""); !strings.Contains(w.Body.String(), "0.50x") {
		t.Fatal("缓存过期后必须读取最新日志")
	}
}

// 生产日志可能很大：接口必须在调用方超时内返回，不能对每一行都做脱敏正则。
func TestRateDetailEndpointStaysFastOnLargeLogs(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	handler := newWebUIHandler()
	path := filepath.Join(logDir, "gateway-2026-10-13.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	unrelated := "2026/10/13 12:00:00 [请求到达] traceId=synthetic 方法=POST 路径=/v1/chat/completions 来源=127.0.0.1:12345 说明=请求已进入网关 authorization=Bearer synthetic-unrelated-token\n"
	for i := 0; i < 40000; i++ {
		if _, err := writer.WriteString(unrelated); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.WriteString(`2026/10/13 12:00:01 [Models] 官方倍率 站点=cn 模型=hy3 原始credits="x0.29" 生效倍率=0.29 有倍率=true 促销="" 促销截止="" 促销已过期=false 来源=实时接口 刷新时间=2026-10-13T12:00:01` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	size, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if size.Size() < 4<<20 {
		t.Fatalf("测试日志过小，未覆盖大文件场景: %d", size.Size())
	}
	start := time.Now()
	w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("大日志下读取失败: %d", w.Code)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("倍率接口在大日志下过慢（%v），部署校验会超时", elapsed)
	}
	var payload struct {
		RecordCount int `json:"recordCount"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RecordCount != 1 {
		t.Fatalf("只应解析价格相关行: %d", payload.RecordCount)
	}
	if strings.Contains(w.Body.String(), "synthetic-unrelated-token") {
		t.Fatal("无关日志正文进入了响应")
	}
	t.Logf("日志 %d 字节，耗时 %v", size.Size(), elapsed)
}

func TestAggregateRateDetailsDeduplicatesAndCapsOtherRecords(t *testing.T) {
	now := time.Now().Unix()
	var records []rateLogRecord
	// 模拟多次重启：同一结论被重复写进日志。
	for i := 0; i < 6; i++ {
		records = append(records,
			rateLogRecord{Site: "intl", Model: "hy4-preview", Source: "官方目录（实时接口）",
				Explain: "官方促销已结束，接口仍返回 0.00x", Value: "-", Unknown: true,
				Priority: ratePriorityPromoStale, UpdatedAt: now - int64(i)},
			rateLogRecord{Site: "intl", Model: "hy4-preview", Source: "价格探测（缓存结论）",
				Explain: "上次真实探测结论：收费（尚未复测）", Value: "收费(倍率未知)", Unknown: true,
				Priority: ratePriorityProbePaid, UpdatedAt: now - int64(i)},
		)
	}
	records = append(records, rateLogRecord{Site: "intl", Model: "hy4-preview", Source: "正常请求实测",
		Explain: "正常客户端请求实测收费，仅作佐证，不改变上方官方倍率", Value: "实测收费",
		Priority: ratePriorityLearnedPaid, UpdatedAt: now})
	rates := aggregateRateDetails(records)
	entry := rates["intl|hy4-preview"]
	if entry["source"] != "价格探测（缓存结论）" {
		t.Fatalf("取值优先级错误: %+v", entry)
	}
	others, ok := entry["others"].([]rateLogRecord)
	if !ok {
		t.Fatalf("others 类型错误")
	}
	if len(others) != 2 {
		t.Fatalf("其他记录应按来源+解释去重为 2 条，实际 %d: %+v", len(others), others)
	}
	if others[0].UpdatedAt < others[1].UpdatedAt {
		t.Fatalf("其他记录应保留较新的一条在前: %+v", others)
	}
	if list, ok := entry["records"].([]rateLogRecord); !ok || len(list) != 13 {
		t.Fatalf("原始记录必须完整保留供折叠查看: %+v", entry["records"])
	}
	// 不同解释超过上限时只保留最新若干条。
	var many []rateLogRecord
	for i := 0; i < 8; i++ {
		many = append(many, rateLogRecord{Site: "cn", Model: "hy3", Source: "官方目录（实时接口）",
			Explain: fmt.Sprintf("解释%d", i), Value: "0.29x", Priority: ratePriorityOfficial, UpdatedAt: now - int64(i)})
	}
	capped := aggregateRateDetails(many)["cn|hy3"]["others"].([]rateLogRecord)
	if len(capped) != rateMaxOtherRecords {
		t.Fatalf("其他记录应限制为 %d 条: %d", rateMaxOtherRecords, len(capped))
	}
}

func TestRateRefreshHintFollowsLoggedProbeSwitch(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	handler := newWebUIHandler()
	path := filepath.Join(logDir, "gateway-2026-10-13.log")
	content := strings.Join([]string{
		`2026/10/13 10:59:32 [ModelPrice] traceId=synthetic 阶段=自动探测调度 allowPriceProbes=true legacyDisablePriceProbes=false 结果=已开启 原因=显式指定-allow-price-probes 费用警告=可能消耗积分`,
		`2026/10/13 10:59:32 [ModelPrice] 载入缓存结论 站点=intl 模型=hy4-preview 判定=paid credit=0.05 tokens=385 探测时间=2026-10-02T00:23:40 说明=usage.credit>0 收费 来源=缓存文件（非本轮新探测）`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	var payload struct {
		RefreshHint      string `json:"refreshHint"`
		AutoProbeEnabled bool   `json:"autoProbeEnabled"`
		AutoProbeKnown   bool   `json:"autoProbeKnown"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.AutoProbeKnown || !payload.AutoProbeEnabled {
		t.Fatalf("探测开关状态应取自普通日志: %+v", payload)
	}
	if !strings.Contains(payload.RefreshHint, "自动复测") {
		t.Fatalf("探测已开启时应说明会自动复测: %q", payload.RefreshHint)
	}
}

func TestRateLogLinesStayInNormalLogNotDebug(t *testing.T) {
	isolateAdminFeatures(t)
	var audit strings.Builder
	oldWriter := log.Writer()
	log.SetOutput(&audit)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	rateLogState.Lock()
	rateLogState.seen = map[string]string{}
	rateLogState.Unlock()
	if err := switchDebugLogging(false); err != nil {
		t.Fatal(err)
	}
	logOfficialRates("cn", []catalogModel{{ID: "hy3", Credits: "x0.29", HasMultiplier: true, Multiplier: 0.29, FromLive: true}}, time.Now())
	if !strings.Contains(audit.String(), "[Models] 官方倍率 站点=cn 模型=hy3") {
		t.Fatal("官方倍率必须写入普通日志")
	}
	if debugLoggingEnabled() {
		t.Fatal("测试应关闭调试日志")
	}
	// 同一签名不重复记录，避免每小时刷屏。
	before := strings.Count(audit.String(), "[Models] 官方倍率 站点=cn 模型=hy3")
	logOfficialRates("cn", []catalogModel{{ID: "hy3", Credits: "x0.29", HasMultiplier: true, Multiplier: 0.29, FromLive: true}}, time.Now())
	if after := strings.Count(audit.String(), "[Models] 官方倍率 站点=cn 模型=hy3"); after != before {
		t.Fatal("倍率未变化时不应重复写日志")
	}
}
