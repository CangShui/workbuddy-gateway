package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func resetModelStats() {
	modelStatsMu.Lock()
	modelStats = map[string]*modelStat{}
	modelStatsMu.Unlock()
}

func TestExtractBusinessCode(t *testing.T) {
	cases := map[string]string{
		`{"error":{"data":{"code":14018,"msg":"x"}}}`: "14018",
		`{"code":6004,"msg":"rate"}`:                  "6004",
		`{"msg":"no code"}`:                           "",
	}
	for body, want := range cases {
		if got := extractBusinessCode(body); got != want {
			t.Errorf("extractBusinessCode(%q)=%q want %q", body, got, want)
		}
	}
	if got := modelStatusFromError(http.StatusTooManyRequests, `{"code":14018}`); got != "14018" {
		t.Errorf("modelStatusFromError business code = %q", got)
	}
	if got := modelStatusFromError(http.StatusBadGateway, "boom"); got != "502" {
		t.Errorf("modelStatusFromError http code = %q", got)
	}
}

func TestModelStatsRecordingAndSnapshot(t *testing.T) {
	resetModelStats()
	recordModelRequest("glm-5.2")
	recordModelRequest("glm-5.2")
	recordModelSuccess("glm-5.2")
	recordModelFailure("glm-5.2", "14018")
	recordModelTTFT("glm-5.2", 400*time.Millisecond)
	recordModelTTFT("glm-5.2", 600*time.Millisecond)
	recordModelLatency("glm-5.2", 2*time.Second)
	recordModelCostClass("glm-5.2", "cn", false)
	recordModelCostClass("glm-5.2", "intl", true)

	acc := &Account{Path: "a.json", Auth: &StoredAuth{}}
	rows := buildModelStatSnapshots(time.Now(), []*Account{acc})
	var target *modelStatSnapshot
	for i := range rows {
		if rows[i].ID == "glm-5.2" {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("glm-5.2 not found in model snapshots")
	}
	if target.Requests != 2 || target.Success != 1 || target.Failed != 1 {
		t.Fatalf("unexpected counters: %+v", target)
	}
	if target.CNFree != "否" || target.IntlFree != "是" || target.LastStatus != "失败:14018" {
		t.Fatalf("unexpected cost/status: cn=%s intl=%s status=%s", target.CNFree, target.IntlFree, target.LastStatus)
	}
	if !target.HasTTFT || target.AvgTTFTMs != 500 {
		t.Fatalf("unexpected ttft: %+v", target)
	}
	if !target.HasLatency || target.AvgLatencyMs != 2000 {
		t.Fatalf("unexpected latency: %+v", target)
	}
}

// 同一模型名在两个站点结论不同时必须分列显示，不能合并成“混合”。
func TestModelFreeSplitBySite(t *testing.T) {
	resetModelStats()
	recordModelCostClass("hy3", "cn", true)
	recordModelCostClass("hy3", "intl", false)

	cnAcc := &Account{Path: "cn.json", Auth: &StoredAuth{Edition: "cn"}, QuotaKnown: true, QuotaRemaining: 100}
	intlAcc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}, QuotaKnown: true, QuotaRemaining: 100}
	rows := buildModelStatSnapshots(time.Now(), []*Account{cnAcc, intlAcc})
	var target *modelStatSnapshot
	for i := range rows {
		if rows[i].ID == "hy3" {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		t.Fatal("hy3 not found")
	}
	if target.CNFree != "是" || target.IntlFree != "否" {
		t.Fatalf("expected cn=是 intl=否, got cn=%s intl=%s", target.CNFree, target.IntlFree)
	}
	// 账号账本维度：国内站 free 只影响国内站账号
	if !modelServableLocked(cnAcc, "hy3", time.Now()) {
		t.Fatal("cn account should serve cn-free model")
	}
}

func TestMultiplierCellUsesFreeThenOfficial(t *testing.T) {
	resetModelStats()
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn":   {{ID: "hy3", Multiplier: 0.20, HasMultiplier: true}},
		"intl": {{ID: "hy3", Multiplier: 0.51, HasMultiplier: true}},
	}
	modelsMu.Unlock()
	defer resetModelsStateT(t)

	if got := modelDisplayMultiplier("cn", "hy3", "是"); got != "0.00x" {
		t.Fatalf("free model should show 0.00x, got %s", got)
	}
	if got := modelDisplayMultiplier("cn", "hy3", "否"); got != "0.20x" {
		t.Fatalf("paid cn should show official multiplier, got %s", got)
	}
	if got := modelDisplayMultiplier("intl", "hy3", "-"); got != "0.51x" {
		t.Fatalf("unknown intl should show official multiplier, got %s", got)
	}
	if got := modelDisplayMultiplier("_", "missing", "-"); got != "-" {
		t.Fatalf("unknown model should show -, got %s", got)
	}
}

func TestModelTableHasEqualDisplayWidth(t *testing.T) {
	resetModelStats()
	recordModelRequest("glm-5.2")
	recordModelSuccess("glm-5.2")
	rows := buildModelStatSnapshots(time.Now(), []*Account{{Path: "a.json", Auth: &StoredAuth{}}})
	table := renderModelTable(rows)
	lines := strings.Split(table, "\n")
	want := displayWidth(lines[0])
	for i, line := range lines {
		if got := displayWidth(line); got != want {
			t.Fatalf("line %d width=%d want=%d:\n%s", i, got, want, table)
		}
	}
	for _, h := range []string{"模型", "国内倍率", "国际倍率", "可用账号", "请求", "首字", "平均"} {
		if !strings.Contains(table, h) {
			t.Fatalf("table missing header %q", h)
		}
	}
	if !strings.Contains(table, "glm-5.2") {
		t.Fatalf("table missing model row")
	}
}

func TestModelServableLocked(t *testing.T) {
	now := time.Now()
	base := &Account{Path: "a.json", Auth: &StoredAuth{}}
	if !modelServableLocked(base, "any", now) {
		t.Fatal("normal account should serve")
	}
	emptyFree := &Account{Path: "b.json", Auth: &StoredAuth{}, QuotaExhausted: true, ModelStates: map[string]*modelRuntimeState{
		"free": {CostClass: modelCostFree},
		"paid": {CostClass: modelCostPaid},
		"cool": {CostClass: modelCostFree, CooldownUntil: now.Add(time.Hour)},
	}}
	if !modelServableLocked(emptyFree, "free", now) {
		t.Fatal("zero-quota account should serve known free model")
	}
	if modelServableLocked(emptyFree, "paid", now) {
		t.Fatal("zero-quota account must not serve paid model")
	}
	if modelServableLocked(emptyFree, "cool", now) {
		t.Fatal("model cooldown must block")
	}
}

func TestTTFTReader(t *testing.T) {
	start := time.Now().Add(-50 * time.Millisecond)
	r := newTTFTReader(strings.NewReader("hello"), start)
	buf := make([]byte, 2)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	if r.duration() <= 0 {
		t.Fatal("ttft should be recorded on first read")
	}
}

// 平均首字/平均总耗时必须是最近 5 小时的滚动窗口，旧样本要被淘汰。
func TestWindowAveragesRollingFiveHours(t *testing.T) {
	resetModelStats()
	now := time.Now()

	modelStatsMu.Lock()
	stat := modelStatLocked("m")
	// 窗口内：1 小时前，TTFT 200ms，耗时 1s
	cur := stat.bucketLocked(now)
	cur.TTFTSum, cur.TTFTSamples = 200*time.Millisecond, 1
	cur.LatencySum, cur.LatencySamples = 1*time.Second, 1
	// 窗口外：10 小时前，TTFT 10s，耗时 20s（应被淘汰）
	stat.Buckets = append(stat.Buckets, hourlyBucket{
		Hour:    hourStart(now.Add(-10 * time.Hour)),
		TTFTSum: 10 * time.Second, TTFTSamples: 1,
		LatencySum: 20 * time.Second, LatencySamples: 1,
	})
	ttft, okT, latency, okL := stat.windowAveragesLocked(now)
	modelStatsMu.Unlock()

	if !okT || ttft != 200*time.Millisecond {
		t.Fatalf("stale TTFT sample must be excluded, got %v ok=%v", ttft, okT)
	}
	if !okL || latency != 1*time.Second {
		t.Fatalf("stale latency sample must be excluded, got %v ok=%v", latency, okL)
	}
}

func TestModelTableHeadersMentionWindow(t *testing.T) {
	resetModelStats()
	rows := buildModelStatSnapshots(time.Now(), []*Account{{Path: "a.json", Auth: &StoredAuth{}}})
	table := renderModelTable(rows)
	for _, h := range []string{"平均首字(5h)", "平均总耗时(5h)"} {
		if !strings.Contains(table, h) {
			t.Fatalf("table missing header %q", h)
		}
	}
	lines := strings.Split(table, "\n")
	want := displayWidth(lines[0])
	for i, line := range lines {
		if got := displayWidth(line); got != want {
			t.Fatalf("line %d width=%d want=%d:\n%s", i, got, want, table)
		}
	}
}
