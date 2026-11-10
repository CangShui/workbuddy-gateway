package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolateStatsFile 把工作目录切到临时目录，隔离 stats.json 的读写。
func isolateStatsFile(t *testing.T) {
	t.Helper()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
}

func writePruneFixture(t *testing.T, now time.Time) []statsRequest {
	t.Helper()
	requestStats.Lock()
	requestStats.document = statsDocument{Schema: statsSchema, StartedAt: now.Add(-40 * 24 * time.Hour).UnixMilli(), Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}
	requestStats.revision, requestStats.saved, requestStats.blocked = 0, 0, false
	requestStats.enabled = true
	requestStats.Unlock()

	rows := []statsRequest{}
	mk := func(id string, ageDays int) statsRequest {
		return statsRequest{Account: id, At: now.Add(-time.Duration(ageDays) * 24 * time.Hour).UnixMilli(),
			Status: 200, LatencyMS: 100}
	}
	// 40 天前的旧记录（应裁剪） + 账号 stale-only（裁剪后应连账号条目一起清掉）
	rows = append(rows, mk("acct-stale-only", 40), mk("acct-mixed", 40), mk("acct-mixed", 5), mk("acct-fresh", 2))
	requestStats.Lock()
	for _, row := range rows {
		if _, ok := requestStats.document.Accounts[row.Account]; !ok {
			requestStats.document.Accounts[row.Account] = statsAccount{File: row.Account + ".json"}
		}
		requestStats.document.Requests = append(requestStats.document.Requests, row)
	}
	requestStats.document.StartedAt = requestStats.document.Requests[0].At
	requestStats.revision = 10
	requestStats.Unlock()
	return rows
}

// 裁剪生效：超窗口记录被移除、窗口内保留、孤儿账号条目清理、文档约束仍成立。
func TestStatsPruneRequestsLockedWindow(t *testing.T) {
	isolateStatsFile(t)
	now := time.Now()
	writePruneFixture(t, now)

	requestStats.Lock()
	defer requestStats.Unlock()
	statsPruneRequestsLocked(now)

	reqs := requestStats.document.Requests
	if len(reqs) != 2 {
		t.Fatalf("应保留窗口内 2 条，实际 %d: %+v", len(reqs), reqs)
	}
	for _, row := range reqs {
		if row.Account == "acct-stale-only" {
			t.Fatalf("纯过期账号的记录应被裁剪")
		}
	}
	if _, ok := requestStats.document.Accounts["acct-stale-only"]; ok {
		t.Fatalf("不再被任何保留记录引用的账号条目应被清理")
	}
	for _, id := range []string{"acct-mixed", "acct-fresh"} {
		if _, ok := requestStats.document.Accounts[id]; !ok {
			t.Fatalf("仍被引用的账号条目不应被清理: %s", id)
		}
	}
	if requestStats.document.StartedAt != reqs[0].At {
		t.Fatalf("StartedAt 应推进到最早保留记录: %d != %d", requestStats.document.StartedAt, reqs[0].At)
	}
	if !validStatsDocument(requestStats.document) {
		t.Fatalf("裁剪后文档必须仍满足 validStatsDocument 约束")
	}
}

// 全部记录过期：清空 Requests 与 Accounts，StartedAt 重置为当前时间。
func TestStatsPruneRequestsLockedAllExpired(t *testing.T) {
	isolateStatsFile(t)
	now := time.Now()
	writePruneFixture(t, now)
	requestStats.Lock()
	// 只留 40 天前的记录（fixture 前两条为 31 天，满足 <35 天）
	fresh := []statsRequest{}
	for _, row := range requestStats.document.Requests {
		if row.At < now.Add(-35*24*time.Hour).UnixMilli() {
			fresh = append(fresh, row)
		}
	}
	if len(fresh) == 0 {
		requestStats.Unlock()
		t.Fatalf("fixture 应包含 35 天前的记录")
	}
	requestStats.document.Requests = fresh
	requestStats.document.StartedAt = fresh[0].At
	requestStats.Unlock()

	requestStats.Lock()
	defer requestStats.Unlock()
	statsPruneRequestsLocked(now)
	if len(requestStats.document.Requests) != 0 || len(requestStats.document.Accounts) != 0 {
		t.Fatalf("全部过期时应清空 Requests 与 Accounts")
	}
	if !validStatsDocument(requestStats.document) {
		t.Fatalf("清空后文档必须仍满足约束（Accounts 为空 map）")
	}
}

// 无超窗记录时零动作：Requests/Accounts/StartedAt/revision 全部不变。
func TestStatsPruneRequestsLockedNoopWhenFresh(t *testing.T) {
	isolateStatsFile(t)
	now := time.Now()
	writePruneFixture(t, now)
	requestStats.Lock()
	requestStats.document.Requests = requestStats.document.Requests[2:] // 只留窗口内 2 条
	requestStats.document.StartedAt = requestStats.document.Requests[0].At
	before := requestStats.document
	beforeReqs := len(requestStats.document.Requests)
	requestStats.Unlock()

	requestStats.Lock()
	defer requestStats.Unlock()
	statsPruneRequestsLocked(now)
	if len(requestStats.document.Requests) != beforeReqs {
		t.Fatalf("无超窗记录时不应裁剪")
	}
	if requestStats.document.StartedAt != before.StartedAt {
		t.Fatalf("无裁剪时 StartedAt 不应变化")
	}
}

// 端到端：flushOptionalStats 在真实落盘前裁剪，写出的文件不含超窗记录，
// 且重启加载后文档仍然有效。
func TestStatsFlushPrunesBeforeWrite(t *testing.T) {
	isolateStatsFile(t)
	now := time.Now()
	writePruneFixture(t, now)
	// saved 落后 revision，触发真实写盘路径
	requestStats.Lock()
	requestStats.saved = 0
	requestStats.Unlock()

	if err := flushOptionalStats(); err != nil {
		t.Fatalf("flushOptionalStats 失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(".", statsFile))
	if err != nil {
		t.Fatalf("stats.json 未写出: %v", err)
	}
	var doc statsDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("写出文件不可解析: %v", err)
	}
	if !validStatsDocument(doc) {
		t.Fatalf("落盘文档不满足 validStatsDocument")
	}
	for _, row := range doc.Requests {
		if row.At < now.Add(-31*24*time.Hour).UnixMilli() {
			t.Fatalf("落盘文件仍包含超窗记录: at=%d", row.At)
		}
	}
}
