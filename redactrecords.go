package main

// 脱敏记录（参照 magpie 观测 › 脱敏）：每个被脱敏的模型请求一条记录。
//
// 落盘（logs/redact-YYYY-MM-DD.jsonl，随日志保留天数清理）的只有：时间、接口、模型、
// 账号文件名、每个值的类型、占位符、开头几个字符与长度。原值从不落盘。
// 上游实际收到的文字（只含占位符）仅保存在内存，网关重启后消失；
// 原值同样只在内存中，管理台在记录详情里按需查看。

import (
	"bufio"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

type redactRecordHit struct {
	Kind        string `json:"kind"`
	Placeholder string `json:"placeholder"`
	Head        string `json:"head"`
	Len         int    `json:"len"`
	Count       int    `json:"count"`
}

type redactRecord struct {
	ID        int64             `json:"id"`
	At        int64             `json:"at"` // 毫秒时间戳
	TraceID   string            `json:"traceId,omitempty"`
	RequestID uint64            `json:"requestId,omitempty"`
	Endpoint  string            `json:"endpoint,omitempty"`
	Model     string            `json:"model,omitempty"`
	Account   string            `json:"account,omitempty"`
	Values    int               `json:"values"`
	Hits      []redactRecordHit `json:"hits"`
	Attempts  int               `json:"attempts"`
	Restored  int               `json:"restored"`
	Failed    bool              `json:"failed,omitempty"` // 脱敏失败，请求未发送
	segments  []redactSegment   // 仅内存
}

const (
	redactRecordsMax      = 5000 // 内存中保留的记录条数
	redactRecordsWithText = 300  // 其中保留“上游收到的文字”的最近条数
	redactRecordsDays     = 30
)

var redactRecords = struct {
	sync.Mutex
	list   []*redactRecord // 按时间升序
	byKey  map[string]*redactRecord
	lastID int64
	writer *dailyLogWriter
	warned bool
	loaded bool
}{byKey: map[string]*redactRecord{}}

var redactRecordFilePattern = regexp.MustCompile(`^redact-(\d{4}-\d{2}-\d{2})\.jsonl$`)

func redactRecordKey(trace string, reqID uint64) string {
	return trace + "#" + strconv.FormatUint(reqID, 10)
}

func nextRedactRecordIDLocked(now time.Time) int64 {
	id := now.UnixMilli() * 1000
	if id <= redactRecords.lastID {
		id = redactRecords.lastID + 1
	}
	redactRecords.lastID = id
	return id
}

// persistRedactLineLocked 追加一行记录；写入失败只告警一次，不影响请求。
func persistRedactLineLocked(v any) {
	if redactRecords.writer == nil {
		w, err := newDailyLogWriter("redact", ".jsonl")
		if err != nil {
			if !redactRecords.warned {
				redactRecords.warned = true
				log.Printf("[脱敏记录] 结果=写入失败 原因=%v 说明=记录仅保存在内存，请求不受影响", err)
			}
			return
		}
		redactRecords.writer = w
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	if _, err := redactRecords.writer.Write(append(raw, '\n')); err != nil && !redactRecords.warned {
		redactRecords.warned = true
		log.Printf("[脱敏记录] 结果=写入失败 原因=%v 说明=记录仅保存在内存，请求不受影响", err)
	}
}

func appendRedactRecordLocked(rec *redactRecord) {
	redactRecords.list = append(redactRecords.list, rec)
	if rec.TraceID != "" || rec.RequestID != 0 {
		redactRecords.byKey[redactRecordKey(rec.TraceID, rec.RequestID)] = rec
	}
	if n := len(redactRecords.list); n > redactRecordsMax {
		for _, old := range redactRecords.list[:n-redactRecordsMax] {
			delete(redactRecords.byKey, redactRecordKey(old.TraceID, old.RequestID))
		}
		redactRecords.list = append([]*redactRecord(nil), redactRecords.list[n-redactRecordsMax:]...)
	}
	// 只为最近的记录保留文字，控制内存
	if n := len(redactRecords.list); n > redactRecordsWithText {
		redactRecords.list[n-redactRecordsWithText-1].segments = nil
	}
}

func redactAccountName(acc *Account) string {
	if acc == nil {
		return ""
	}
	return filepath.Base(acc.Path)
}

// noteRedactRecord 记录一次出站脱敏。同一请求换账号或重放时合并为一条，只累加尝试次数。
func noteRedactRecord(r *http.Request, reqID uint64, traceID string, acc *Account, res *redactResult) *redactRecord {
	redactRecords.Lock()
	defer redactRecords.Unlock()
	now := time.Now()
	key := redactRecordKey(traceID, reqID)
	if rec := redactRecords.byKey[key]; rec != nil && now.UnixMilli()-rec.At < int64(10*time.Minute/time.Millisecond) {
		rec.Attempts++
		rec.Account = redactAccountName(acc)
		return rec
	}
	rec := &redactRecord{ID: nextRedactRecordIDLocked(now), At: now.UnixMilli(), TraceID: traceID, RequestID: reqID,
		Model: res.model, Account: redactAccountName(acc), Values: len(res.hits), Attempts: 1}
	if r != nil && r.URL != nil {
		rec.Endpoint = r.URL.Path
	}
	byPlaceholder := map[string]int{}
	for _, h := range res.hits {
		if i, ok := byPlaceholder[h.Placeholder]; ok {
			rec.Hits[i].Count++
			continue
		}
		byPlaceholder[h.Placeholder] = len(rec.Hits)
		rec.Hits = append(rec.Hits, redactRecordHit{Kind: h.Kind, Placeholder: h.Placeholder, Head: h.Head, Len: h.Len, Count: 1})
	}
	total := 0
	for _, s := range res.segments {
		if total+len(s.Text) > redactRecordTextLimit {
			break
		}
		total += len(s.Text)
		rec.segments = append(rec.segments, s)
	}
	appendRedactRecordLocked(rec)
	persistRedactLineLocked(rec)
	return rec
}

// noteRedactFailure 记录脱敏失败（请求未发送）。
func noteRedactFailure(r *http.Request, reqID uint64, traceID string, acc *Account) {
	redactRecords.Lock()
	defer redactRecords.Unlock()
	now := time.Now()
	rec := &redactRecord{ID: nextRedactRecordIDLocked(now), At: now.UnixMilli(), TraceID: traceID, RequestID: reqID,
		Account: redactAccountName(acc), Attempts: 1, Failed: true, Hits: []redactRecordHit{}}
	if r != nil && r.URL != nil {
		rec.Endpoint = r.URL.Path
	}
	appendRedactRecordLocked(rec)
	persistRedactLineLocked(rec)
}

type redactRestoredLine struct {
	RestoredFor int64 `json:"restoredFor"`
	Restored    int   `json:"restored"`
}

// noteRedactRestored 在回复读完后记下还原了多少个占位符。
func noteRedactRestored(rec *redactRecord, n int) {
	if rec == nil || n == 0 {
		return
	}
	redactRecords.Lock()
	defer redactRecords.Unlock()
	rec.Restored += n
	persistRedactLineLocked(redactRestoredLine{RestoredFor: rec.ID, Restored: n})
}

// loadRedactRecords 启动时读回最近 30 天的记录（不含文字与原值）。
func loadRedactRecords() {
	redactRecords.Lock()
	defer redactRecords.Unlock()
	if redactRecords.loaded {
		return
	}
	redactRecords.loaded = true
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -redactRecordsDays).Format("2006-01-02")
	var names []string
	for _, e := range entries {
		m := redactRecordFilePattern.FindStringSubmatch(e.Name())
		if e.Type().IsRegular() && m != nil && m[1] >= cutoff {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	byID := map[int64]*redactRecord{}
	var list []*redactRecord
	for _, name := range names {
		f, err := os.Open(filepath.Join(logDir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			var restored redactRestoredLine
			if json.Unmarshal(line, &restored) == nil && restored.RestoredFor != 0 {
				if rec := byID[restored.RestoredFor]; rec != nil {
					rec.Restored += restored.Restored
				}
				continue
			}
			var rec redactRecord
			if json.Unmarshal(line, &rec) != nil || rec.ID == 0 {
				continue
			}
			if rec.Hits == nil {
				rec.Hits = []redactRecordHit{}
			}
			r := rec
			byID[r.ID] = &r
			list = append(list, &r)
		}
		f.Close()
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if len(list) > redactRecordsMax {
		list = list[len(list)-redactRecordsMax:]
	}
	// 读回的旧记录排在本次运行的新记录之前
	merged := append(list, redactRecords.list...)
	redactRecords.list = merged
	for _, rec := range list {
		if rec.ID > redactRecords.lastID {
			redactRecords.lastID = rec.ID
		}
	}
	if len(list) > 0 {
		log.Printf("[脱敏记录] 启动读回=%d条 文件数=%d 说明=只含类型、占位符、开头字符与长度；原值与上游文字不落盘", len(list), len(names))
	}
}

// clearRedactRecords 删除全部脱敏记录（内存与 logs/redact-*.jsonl）。脱敏设置不变。
func clearRedactRecords(trace string) (int, error) {
	redactRecords.Lock()
	defer redactRecords.Unlock()
	if redactRecords.writer != nil {
		_ = redactRecords.writer.Close()
		redactRecords.writer = nil
	}
	redactRecords.list = nil
	redactRecords.byKey = map[string]*redactRecord{}
	entries, err := os.ReadDir(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, e := range entries {
		if !e.Type().IsRegular() || !redactRecordFilePattern.MatchString(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(logDir, e.Name())); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	log.Printf("[脱敏记录] traceId=%s 操作=清空 删除文件=%d 结果=%s 说明=脱敏设置不变，后续请求重新记录", trace, removed, map[bool]string{true: "成功", false: "部分失败"}[firstErr == nil])
	return removed, firstErr
}

// ---- 查询 -----------------------------------------------------------------------

type redactRecordFilter struct {
	days        int
	kind, model string
	before      int64
	limit       int
}

func (f redactRecordFilter) match(rec *redactRecord, since int64) bool {
	if rec.At < since || f.model != "" && rec.Model != f.model {
		return false
	}
	if f.kind == "" {
		return true
	}
	if f.kind == "FAILED" {
		return rec.Failed
	}
	for _, h := range rec.Hits {
		if h.Kind == f.kind {
			return true
		}
	}
	return false
}

func redactRecordView(rec *redactRecord) map[string]any {
	return map[string]any{
		"id": strconv.FormatInt(rec.ID, 10), "at": rec.At, "traceId": rec.TraceID, "requestId": rec.RequestID,
		"endpoint": rec.Endpoint, "model": rec.Model, "account": rec.Account, "values": rec.Values,
		"hits": rec.Hits, "attempts": rec.Attempts, "restored": rec.Restored, "failed": rec.Failed,
		"hasText": len(rec.segments) > 0,
	}
}

func redactSince(days int) int64 {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	return start.UnixMilli()
}

func redactRecordsQuery(f redactRecordFilter) map[string]any {
	since := redactSince(f.days)
	redactRecords.Lock()
	defer redactRecords.Unlock()
	type valueAgg struct {
		Kind, Placeholder, Head string
		Len, Count, Requests    int
	}
	summary := struct {
		requests, values, restored, failed, retried int
		kinds                                       map[string]int
		models, accounts                            map[string]int
		days                                        map[string][2]int
		valuesBy                                    map[string]*valueAgg
	}{kinds: map[string]int{}, models: map[string]int{}, accounts: map[string]int{}, days: map[string][2]int{}, valuesBy: map[string]*valueAgg{}}
	var out []map[string]any
	more := false
	for i := len(redactRecords.list) - 1; i >= 0; i-- {
		rec := redactRecords.list[i]
		if rec.At < since {
			continue
		}
		summary.requests++
		summary.values += rec.Values
		summary.restored += rec.Restored
		if rec.Failed {
			summary.failed++
		}
		if rec.Attempts > 1 {
			summary.retried++
		}
		if rec.Model != "" {
			summary.models[rec.Model]++
		}
		if rec.Account != "" {
			summary.accounts[rec.Account]++
		}
		day := time.UnixMilli(rec.At).Format("2006-01-02")
		d := summary.days[day]
		summary.days[day] = [2]int{d[0] + 1, d[1] + rec.Values}
		for _, h := range rec.Hits {
			summary.kinds[h.Kind] += h.Count
			a := summary.valuesBy[h.Placeholder]
			if a == nil {
				a = &valueAgg{Kind: h.Kind, Placeholder: h.Placeholder, Head: h.Head, Len: h.Len}
				summary.valuesBy[h.Placeholder] = a
			}
			a.Count += h.Count
			a.Requests++
		}
		if !f.match(rec, since) || f.before != 0 && rec.ID >= f.before {
			continue
		}
		if len(out) >= f.limit {
			more = true
			continue
		}
		out = append(out, redactRecordView(rec))
	}
	if out == nil {
		out = []map[string]any{}
	}
	var days []map[string]any
	start := time.UnixMilli(since)
	for i := 0; i < f.days; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		d := summary.days[day]
		days = append(days, map[string]any{"date": day, "requests": d[0], "values": d[1]})
	}
	top := make([]*valueAgg, 0, len(summary.valuesBy))
	for _, a := range summary.valuesBy {
		top = append(top, a)
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Requests != top[j].Requests {
			return top[i].Requests > top[j].Requests
		}
		return top[i].Count > top[j].Count
	})
	if len(top) > 10 {
		top = top[:10]
	}
	topView := make([]map[string]any, len(top))
	for i, a := range top {
		topView[i] = map[string]any{"kind": a.Kind, "placeholder": a.Placeholder, "head": a.Head, "len": a.Len, "count": a.Count, "requests": a.Requests}
	}
	return map[string]any{
		"records": out, "more": more, "days": f.days,
		"summary": map[string]any{
			"requests": summary.requests, "values": summary.values, "restored": summary.restored,
			"failed": summary.failed, "retried": summary.retried, "kinds": summary.kinds,
			"models": summary.models, "accounts": summary.accounts, "byDay": days, "topValues": topView,
		},
	}
}

func findRedactRecordLocked(id int64) *redactRecord {
	list := redactRecords.list
	i := sort.Search(len(list), func(i int) bool { return list[i].ID >= id })
	if i < len(list) && list[i].ID == id {
		return list[i]
	}
	return nil
}

func redactValuesInMemory(rec *redactRecord) map[string]string {
	values := map[string]string{}
	redactStore.RLock()
	defer redactStore.RUnlock()
	for _, h := range rec.Hits {
		if v, ok := redactStore.values[h.Placeholder]; ok {
			values[h.Placeholder] = v
		}
	}
	return values
}

// ---- 管理接口 -------------------------------------------------------------------

func queryInt(r *http.Request, name string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

func handleWebUIRedactRecords(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	q := r.URL.Query()
	f := redactRecordFilter{days: queryInt(r, "days", 7, 1, redactRecordsDays), kind: q.Get("kind"), model: q.Get("model"),
		limit: queryInt(r, "limit", 50, 1, 200)}
	if b := q.Get("before"); b != "" {
		f.before, _ = strconv.ParseInt(b, 10, 64)
	}
	writeJSON(w, http.StatusOK, redactRecordsQuery(f))
}

func handleWebUIRedactRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "id 无效")
		return
	}
	redactRecords.Lock()
	rec := findRedactRecordLocked(id)
	var view map[string]any
	var segments []redactSegment
	if rec != nil {
		view = redactRecordView(rec)
		segments = append([]redactSegment{}, rec.segments...)
	}
	redactRecords.Unlock()
	if rec == nil {
		writeJSONError(w, http.StatusNotFound, "记录不存在或已清空")
		return
	}
	view["segments"] = segments
	view["valuesInMemory"] = len(redactValuesInMemory(rec))
	writeJSON(w, http.StatusOK, view)
}

// handleWebUIRedactReveal 返回这条记录的原值（仅限仍在网关内存中的）。只在管理员点击时调用，日志不含原值。
func handleWebUIRedactReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "必须提供记录 id")
		return
	}
	id, err := strconv.ParseInt(input.ID, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "id 无效")
		return
	}
	redactRecords.Lock()
	rec := findRedactRecordLocked(id)
	redactRecords.Unlock()
	if rec == nil {
		writeJSONError(w, http.StatusNotFound, "记录不存在或已清空")
		return
	}
	values := redactValuesInMemory(rec)
	log.Printf("[脱敏记录] traceId=%s 操作=查看原值 记录=%d 可查看=%d/%d 说明=原值仅返回给管理台，不写日志", debugTraceID(r), id, len(values), len(rec.Hits))
	writeJSON(w, http.StatusOK, map[string]any{"values": values})
}

func handleWebUIRedactRecordsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input map[string]json.RawMessage
	if err := decodeAdminJSON(w, r, &input); err != nil || input == nil || len(input) != 0 {
		writeJSONError(w, http.StatusBadRequest, "清空请求必须为空 JSON 对象")
		return
	}
	removed, err := clearRedactRecords(debugTraceID(r))
	if err != nil {
		writeAdminOperationError(w, r, "清空脱敏记录", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true, "files": removed})
}

// redactBriefSummary 供设置接口显示一行概况。
func redactBriefSummary() map[string]any {
	since := redactSince(7)
	redactRecords.Lock()
	defer redactRecords.Unlock()
	requests, values := 0, 0
	for i := len(redactRecords.list) - 1; i >= 0 && redactRecords.list[i].At >= since; i-- {
		requests++
		values += redactRecords.list[i].Values
	}
	return map[string]any{"requests": requests, "values": values, "days": 7}
}
