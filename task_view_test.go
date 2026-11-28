package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func taskByID(t *testing.T, rows []scheduledTaskView, id string) scheduledTaskView {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing task %s", id)
	return scheduledTaskView{}
}

func TestTaskLogAggregationNoFalseSuccessOrCrossProcess(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.ReloadInterval, cfg.ModelsRefresh, cfg.KeepaliveHours = 5, 60, []int{22}
	cfg.DisablePriceProbes = true
	now := time.Now()
	at := now.UnixMilli()
	pid := os.Getpid()
	events := []scheduledTaskEvent{
		{Task: "quota_scan", Phase: "started", Status: "running", Trace: "new", PID: pid, At: at - 1000, StartedAt: at - 1000, NextAt: at + 300000},
		{Task: "quota_scan", Phase: "completed", Status: "cancelled", Trace: "old", PID: pid, At: at, StartedAt: at - 10000, NextAt: at + 300000},
		{Task: "token_check", Phase: "completed", Status: "failed", PID: pid + 1, At: at, StartedAt: at - 1000},
		{Task: "config_reload", Phase: "completed", Status: "success", PID: pid, At: at - 5000, StartedAt: at - 5010, NextAt: at - 4000},
		{Task: "keepalive_catchup", Phase: "scheduled", PID: pid, At: at - 30000, NextAt: at - 10000},
		{Task: "keepalive_catchup", Phase: "completed", Status: "skipped", PID: pid, At: at - 9000, StartedAt: at - 9500},
		{Task: "daily_checkin", Phase: "scheduled", PID: pid, At: at - 30000, NextAt: at - 1000},
	}
	rows := aggregateTaskViews(events, now, pid, at-60000, scheduledTaskEvent{})
	if len(rows) != 11 {
		t.Fatalf("任务数=%d", len(rows))
	}
	quota := taskByID(t, rows, "quota_scan")
	if quota.Status != "running" || quota.Success != nil || quota.TraceID != "new" {
		t.Fatalf("旧轮次结束覆盖了新轮次: %+v", quota)
	}
	if row := taskByID(t, rows, "token_check"); row.Status != "unknown" || row.Success != nil || row.LastRunAt != 0 {
		t.Fatal("旧进程失败记录污染当前状态")
	}
	if row := taskByID(t, rows, "config_reload"); row.Success == nil || !*row.Success || row.NextRunAt <= now.Unix() || !row.NextEstimated {
		t.Fatalf("周期预计时间错误: %+v", row)
	}
	if row := taskByID(t, rows, "keepalive_catchup"); row.NextRunAt != 0 || row.Status != "skipped" || row.Success != nil {
		t.Fatal("一次性补跑完成后不应重复安排")
	}
	if row := taskByID(t, rows, "daily_checkin"); row.NextRunAt != 0 || row.Success != nil {
		t.Fatal("过去的计划不能伪造新的执行或成功")
	}
	if row := taskByID(t, rows, "price_probe"); row.Enabled || row.Status != "disabled" || row.NextRunAt != 0 {
		t.Fatal("关闭的价格探测不应显示计划")
	}
	beforeBoot := aggregateTaskViews(events, now, pid, at+1, scheduledTaskEvent{})
	if taskByID(t, beforeBoot, "quota_scan").LastRunAt != 0 {
		t.Fatal("服务启动前记录不能充当当前执行")
	}
}

func TestKeepaliveHistoryDoesNotOverrideLiveRunOrSchedule(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.KeepaliveHours = []int{22}
	now := time.Now().Truncate(time.Second)
	at := now.UnixMilli()
	pid := os.Getpid()
	saved := scheduledTaskEvent{Task: "keepalive", Phase: "completed", Status: "partial",
		PID: pid - 1, Trace: "previous", At: at - 120000, StartedAt: at - 121000, NextAt: at + 35000}
	events := []scheduledTaskEvent{
		{Task: "keepalive", Phase: "scheduled", PID: pid, At: at - 20000, NextAt: at + 60000},
		{Task: "keepalive", Phase: "started", Status: "running", PID: pid - 1, At: at - 100000},
		{Task: "keepalive_catchup", Phase: "completed", Status: "success", PID: pid, At: at - 15000},
	}
	get := func() scheduledTaskView {
		return taskByID(t, aggregateTaskViews(events, now, pid, at-30000, saved), "keepalive")
	}
	if row := get(); row.Status != "partial" || !row.PreviousProcess || row.LastFinishedAt != saved.At/1000 ||
		row.Success == nil || *row.Success || row.NextRunAt != now.Unix()+60 || row.NextEstimated {
		t.Fatalf("历史结果、启动补跑、当前计划不可混用：%+v", row)
	}
	events = append(events, scheduledTaskEvent{Task: "keepalive", Phase: "started", Status: "running",
		PID: pid, Trace: "current", At: at - 1000, StartedAt: at - 1000})
	if row := get(); row.Status != "running" || row.PreviousProcess || row.Success != nil {
		t.Fatalf("历史结果不应覆盖当前运行：%+v", row)
	}
	saved = scheduledTaskEvent{Task: "keepalive", Phase: "completed", Status: "success",
		PID: pid, Trace: "current", At: at - 1000, StartedAt: at - 1000}
	if row := get(); row.Status != "success" || row.PreviousProcess || row.Success == nil || !*row.Success {
		t.Fatalf("同一轮已落盘结果不应仍显示执行中：%+v", row)
	}
}

func TestKeepaliveRestoresLegacyCompletionOutsideScanBudget(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.KeepaliveHours = []int{22}
	now := time.Now().Truncate(time.Second)
	old := now.AddDate(0, 0, -1)
	event := scheduledTaskEvent{Task: "keepalive", Phase: "completed", Status: "partial",
		PID: os.Getpid() - 1, Trace: "legacy-completion", At: old.UnixMilli()}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	oldLog := "gateway-" + old.Format("2006-01-02") + ".log"
	if err := os.WriteFile(filepath.Join(logDir, oldLog), append(append([]byte("[定时任务] "), raw...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	todayLog := filepath.Join(logDir, "gateway-"+now.Format("2006-01-02")+".log")
	f, err := os.OpenFile(todayLog, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	block := strings.Repeat(strings.Repeat("x", 1023)+"\n", 1024)
	for i := int64(0); i <= taskLogReadBudget/int64(len(block)); i++ {
		if _, err := f.WriteString(block); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	restoreKeepaliveResult()
	handler := newWebUIHandler()
	get := func() scheduledTaskView {
		t.Helper()
		w := adminRequest(handler, "GET", "/admin/api/tasks", "feature-admin-key", "")
		var response struct {
			Tasks     []scheduledTaskView `json:"tasks"`
			Truncated bool                `json:"truncated"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); w.Code != 200 || err != nil || !response.Truncated {
			t.Fatalf("应返回超出普通扫描窗口的测试数据：HTTP %d，%v", w.Code, err)
		}
		return taskByID(t, response.Tasks, "keepalive")
	}
	if row := get(); row.Status != "partial" || !row.PreviousProcess || row.TraceID != event.Trace || row.LogFile != oldLog ||
		!row.NextEstimated || row.NextRunAt <= now.Unix() {
		t.Fatalf("应从仍存在的历史日志恢复结果，不沿用旧计划：%+v", row)
	}
	logScheduledTask("keepalive", "completed", "success", "new-completion", now, time.Time{}, "定时成功")
	logScheduledTask("keepalive_catchup", "completed", "failed", "catchup", now, time.Time{}, "补跑失败")
	saved, err := readKeepaliveResult()
	if err != nil || saved.Trace != "new-completion" || saved.Status != "success" {
		t.Fatalf("仅定时完成结果应更新落盘记录：%+v %v", saved, err)
	}
	// 模拟同一进程的普通日志已滚出扫描窗口。
	if row := taskByID(t, aggregateTaskViews(nil, time.Now(), os.Getpid(), taskLogBootAt, saved), "keepalive"); row.Status != "success" || row.PreviousProcess {
		t.Fatalf("日志超出预算不应丢掉本进程的结果：%+v", row)
	}
}

func TestReadOnlyTasksEndpointAndCredentialPageRemoved(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.ReloadInterval, cfg.ModelsRefresh = 5, 60
	now := time.Now()
	logScheduledTask("quota_scan", "completed", "partial", "task-fixture", now.Add(-time.Second), now.Add(5*time.Minute), "两个账号查询失败")
	logScheduledTask("config_reload", "completed", "success", "check-fixture", now, now.Add(time.Second), "检查完成")
	handler := newWebUIHandler()
	if w := adminRequest(handler, "GET", "/admin/api/tasks", "feature-api-key", ""); w.Code != 401 {
		t.Fatal("任务日志必须需要管理Key")
	}
	before, _ := os.ReadFile(runtimeConfigFile)
	w := adminRequest(handler, "GET", "/admin/api/tasks", "feature-admin-key", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var response struct {
		Tasks  []scheduledTaskView `json:"tasks"`
		Source string              `json:"source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	row := taskByID(t, response.Tasks, "quota_scan")
	if row.Status != "partial" || row.Success == nil || *row.Success || row.LastFinishedAt == 0 || row.LogFile == "" {
		t.Fatalf("执行结果没有来自日志: %+v", row)
	}
	if w := adminRequest(handler, "POST", "/admin/api/tasks", "feature-admin-key", `{}`); w.Code != 405 {
		t.Fatal("只读任务接口不应接收写操作")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if string(before) != string(after) {
		t.Fatal("读取任务修改了配置")
	}
	html, _ := webFS.ReadFile("web/index.html")
	js, _ := webFS.ReadFile("web/app.js")
	if strings.Contains(string(html), `data-tab="credentials"`) || strings.Contains(string(html), "credentialContent") || strings.Contains(string(js), "loadCredentials") {
		t.Fatal("网页仍包含凭据页或编辑器")
	}
	if !strings.Contains(string(html), "tasksTable") {
		t.Fatal("任务页面没有嵌入")
	}
}

func TestTaskLogMissingCorruptAndBudget(t *testing.T) {
	isolateAdminFeatures(t)
	path := filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("2026/10/11 00:00:00 [定时任务] broken json\n")
	_ = file.Close()
	if _, _, _, err := readTaskEvents(false); err != nil {
		t.Fatal("坏行应跳过而非报成功或整个失败")
	}
	large := filepath.Join(logDir, "gateway-2099-01-01.log")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(taskLogReadBudget + 100); err != nil {
		t.Fatal(err)
	}
	f.Close()
	events, size, truncated, err := readTaskEvents(false)
	if err != nil || size > taskLogReadBudget || !truncated || len(events) != 0 {
		t.Fatalf("预算或缺失处理错误: records=%d size=%d truncated=%v err=%v", len(events), size, truncated, err)
	}
}

func TestTaskLoggingWorksWithoutDebugAndIsSampled(t *testing.T) {
	isolateAdminFeatures(t)
	if debugLoggingEnabled() {
		t.Fatal("测试应关闭debug")
	}
	var last time.Time
	var previous string
	now := time.Now()
	logFrequentTask("credential_reload", "success", "扫描成功", now, now.Add(5*time.Second), &last, &previous, false)
	logFrequentTask("credential_reload", "success", "扫描成功", now, now.Add(5*time.Second), &last, &previous, false)
	logFrequentTask("credential_reload", "failed", "扫描失败", now, now.Add(5*time.Second), &last, &previous, false)
	events, _, _, err := readTaskEvents(false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Task == "credential_reload" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("高频成功应合并、失败即时记录，实际=%d", count)
	}
}

func TestConfigTaskRecordsFailedValidation(t *testing.T) {
	isolateAdminFeatures(t)
	writeRuntimeFixture(t, `{`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtimeConfigLoop(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		events, _, _, _ := readTaskEvents(false)
		for _, event := range events {
			if event.Task == "config_reload" && event.Phase == "completed" && event.Status == "failed" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("配置解析失败没有日志任务失败事件")
}

func TestCredentialScanAndSnapshotErrorsReported(t *testing.T) {
	isolateAdminFeatures(t)
	cfg.AuthDir = filepath.Join(t.TempDir(), "missing")
	accounts = []*Account{{Path: "keep.json"}}
	if changed, err := reloadAccountsResult(); err == nil || changed || len(accounts) != 1 {
		t.Fatal("目录失败应保留账号并返回错误")
	}
	accounts = nil
	if err := os.Mkdir(statusSnapshotFile+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusSnapshot(); err == nil {
		t.Fatal("快照写入错误不能作为成功")
	}
}
