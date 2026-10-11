package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

var logRetentionDays atomic.Int64
var activeLogsMu sync.Mutex
var activeLogs = map[string]int{}
var logCleanupMu sync.Mutex
var cleanupScheduleMu sync.Mutex
var lastLogCleanup struct {
	time time.Time
	days int
}

// 写入时按本地日期切换文件。与清理共用打开文件登记，不能删除正在写入的旧日志。
type dailyLogWriter struct {
	mu                sync.Mutex
	prefix, extension string
	file              *os.File
	path, day         string
	closed            bool
}

func newDailyLogWriter(prefix, extension string) (*dailyLogWriter, error) {
	w := &dailyLogWriter{prefix: prefix, extension: extension}
	if err := w.rotate(time.Now()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyLogWriter) rotate(now time.Time) error {
	day := now.Format("2006-01-02")
	if w.file != nil && day == w.day {
		return nil
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}
	path, err := filepath.Abs(filepath.Join(logDir, w.prefix+"-"+day+w.extension))
	if err != nil {
		return err
	}
	activeLogsMu.Lock()
	defer activeLogsMu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	activeLogs[path]++
	if w.file != nil {
		_ = w.file.Close()
		activeLogs[w.path]--
		if activeLogs[w.path] == 0 {
			delete(activeLogs, w.path)
		}
	}
	w.file, w.path, w.day = f, path, day
	return nil
}

func (w *dailyLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if err := w.rotate(time.Now()); err != nil {
		return 0, err
	}
	return w.file.Write(data)
}

func (w *dailyLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	activeLogsMu.Lock()
	defer activeLogsMu.Unlock()
	activeLogs[w.path]--
	if activeLogs[w.path] == 0 {
		delete(activeLogs, w.path)
	}
	return w.file.Close()
}

var managedLogPattern = regexp.MustCompile(`^(gateway-\d{4}-\d{2}-\d{2}\.log|debug-\d{4}-\d{2}-\d{2}\.jsonl|deployment-\d{4}-\d{2}-\d{2}\.jsonl|frontend-\d{4}-\d{2}-\d{2}\.log)$`)
var logDatePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

type logCleanupResult struct {
	RetentionDays int      `json:"retentionDays"`
	Deleted       int      `json:"deleted"`
	DeletedBytes  int64    `json:"deletedBytes"`
	SkippedActive int      `json:"skippedActive"`
	Failed        int      `json:"failed"`
	Files         []string `json:"files"`
}

func cleanupLogs(now time.Time, days int, trace string, schedule ...time.Time) (result logCleanupResult, resultErr error) {
	next := time.Time{}
	if len(schedule) > 0 {
		next = schedule[0]
	} else {
		cleanupScheduleMu.Lock()
		if !lastLogCleanup.time.IsZero() {
			next = lastLogCleanup.time.Add(time.Hour)
		}
		cleanupScheduleMu.Unlock()
	}
	started := time.Now()
	logScheduledTask("log_cleanup", "started", "running", trace, started, next, "")
	result = logCleanupResult{RetentionDays: days, Files: []string{}}
	defer func() {
		status := "success"
		if resultErr != nil {
			status = "failed"
		} else if days == 0 {
			status = "skipped"
		} else if result.Failed > 0 {
			status = "partial"
		}
		detail := fmt.Sprintf("保留天数=%d，删除=%d，失败=%d，跳过正在写入=%d", days, result.Deleted, result.Failed, result.SkippedActive)
		if resultErr != nil {
			detail = safeDebugError(resultErr)
		}
		logScheduledTask("log_cleanup", "completed", status, trace, started, next, detail)
	}()
	if days < 0 || days > 36500 {
		return result, gatewayValidationError("日志保留天数无效")
	}
	if days == 0 {
		log.Printf("[日志清理] traceId=%s 结果=跳过 保留天数=0 说明=未删除任何日志，0表示保留全部", trace)
		return result, nil
	}
	logCleanupMu.Lock()
	defer logCleanupMu.Unlock()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	directoryInfo, err := os.Lstat(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("日志目录不是普通目录或是符号链接，拒绝清理")
	}
	entries, err := os.ReadDir(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	log.Printf("[日志清理] traceId=%s 阶段=扫描 保留天数=%d 最早保留日期=%s 文件数=%d 说明=仅清理命名受限的历史日志，不跟随符号链接，不触及配置和凭据",
		trace, days, cutoff.Format("2006-01-02"), len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !managedLogPattern.MatchString(name) {
			continue
		}
		date, parseErr := time.ParseInLocation("2006-01-02", logDatePattern.FindString(name), now.Location())
		if parseErr != nil || !date.Before(cutoff) {
			continue
		}
		fullPath, _ := filepath.Abs(filepath.Join(logDir, name))
		activeLogsMu.Lock()
		if activeLogs[fullPath] > 0 {
			activeLogsMu.Unlock()
			result.SkippedActive++
			continue
		}
		info, statErr := entry.Info()
		if statErr == nil && !info.Mode().IsRegular() {
			activeLogsMu.Unlock()
			continue
		}
		removeErr := statErr
		if statErr == nil {
			removeErr = root.Remove(name)
		}
		activeLogsMu.Unlock()
		if removeErr != nil {
			result.Failed++
			log.Printf("[日志清理] traceId=%s 文件=%q 结果=删除失败 原因=%s", trace, name, safeDebugError(removeErr))
			continue
		}
		result.Deleted++
		result.DeletedBytes += info.Size()
		result.Files = append(result.Files, name)
		log.Printf("[日志清理] traceId=%s 文件=%q 文件日期=%s 字节=%d 结果=已删除 原因=超出保留天数", trace, name, date.Format("2006-01-02"), info.Size())
	}
	log.Printf("[日志清理] traceId=%s 结果=完成 已删除=%d 回收字节=%d 跳过正在写入=%d 失败=%d", trace, result.Deleted, result.DeletedBytes, result.SkippedActive, result.Failed)
	return result, nil
}

func scheduledLogCleanup(now time.Time) {
	days := int(logRetentionDays.Load())
	cleanupScheduleMu.Lock()
	previousDays := lastLogCleanup.days
	due := days > 0 && (lastLogCleanup.days != days || now.Sub(lastLogCleanup.time) >= time.Hour)
	lastLogCleanup.days = days
	if due {
		lastLogCleanup.time = now
	}
	cleanupScheduleMu.Unlock()
	if days == 0 && previousDays != 0 {
		logScheduledTask("log_cleanup", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "保留天数已改为0，自动清理关闭")
	}
	if !due {
		return
	}
	trace := newTraceID()
	if _, err := cleanupLogs(now, days, trace, now.Add(time.Hour)); err != nil {
		log.Printf("[日志清理] traceId=%s 阶段=自动任务 结果=失败 原因=%s 说明=保留现有日志，下个周期重试", trace, safeDebugError(err))
	}
}

func saveLogRetention(days int, trace string) error {
	if days < 0 || days > 36500 {
		return gatewayValidationError("日志保留天数必须为 0～36500，0 表示不自动清理")
	}
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	doc, gateway, old, err := readGatewayDocumentLocked()
	if err == nil && (cfg.AdminKey == "" || gateway.AdminKey != cfg.AdminKey) {
		err = errConfigChange
	}
	gatewayConfigMu.Unlock()
	if err != nil {
		return err
	}
	doc["logs"], _ = json.Marshal(map[string]int{"retentionDays": days})
	log.Printf("[日志设置] traceId=%s 阶段=配置写入 旧保留天数=%d 新保留天数=%d 说明=其他配置段原样保留", trace, logRetentionDays.Load(), days)
	return persistRuntimeDocument(doc, old, trace)
}

func handleWebUILogSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		log.Printf("[日志设置] traceId=%s 阶段=读取 保留天数=%d 结果=成功", debugTraceID(r), logRetentionDays.Load())
		writeJSON(w, http.StatusOK, map[string]any{"retentionDays": logRetentionDays.Load()})
	case http.MethodPost:
		var input struct {
			Days *int `json:"retentionDays"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil || input.Days == nil {
			log.Printf("[日志设置] traceId=%s 阶段=参数校验 结果=拒绝 原因=保留天数缺失或字段格式无效 状态码=400", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "必须提供 retentionDays 整数")
			return
		}
		if err := saveLogRetention(*input.Days, debugTraceID(r)); err != nil {
			writeAdminOperationError(w, r, "日志设置", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"retentionDays": *input.Days, "saved": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

func handleWebUILogCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input map[string]json.RawMessage
	if err := decodeAdminJSON(w, r, &input); err != nil || input == nil || len(input) != 0 {
		log.Printf("[日志清理] traceId=%s 阶段=参数校验 结果=拒绝 原因=请求JSON无效 状态码=400", debugTraceID(r))
		writeJSONError(w, http.StatusBadRequest, "清理请求必须为空 JSON 对象")
		return
	}
	result, err := cleanupLogs(time.Now(), int(logRetentionDays.Load()), debugTraceID(r))
	if err != nil {
		writeAdminOperationError(w, r, "日志清理", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
