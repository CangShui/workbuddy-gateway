package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

const statsFile = "stats.json"
const statsSchema = 1
const statsMaxFileBytes = 64 << 20

// 只记录统计事实，不记录凭据、请求/响应正文、提示词或令牌。
// identity 是账号身份摘要，避免同一文件换成另一个账号后继承旧模型阻断。
type statsAccount struct {
	File        string                        `json:"file"`
	Edition     string                        `json:"edition"`
	ModelStates map[string]modelStateSnapshot `json:"modelStates,omitempty"`
}

type statsRequest struct {
	Account      string   `json:"account"`
	Model        string   `json:"model"`
	At           int64    `json:"at"` // 完成时间，Unix 毫秒，按该时刻归桶
	Status       int      `json:"status"`
	Complete     bool     `json:"complete"`
	TotalTokens  *int64   `json:"totalTokens,omitempty"`
	InputTokens  *int64   `json:"inputTokens,omitempty"`
	OutputTokens *int64   `json:"outputTokens,omitempty"`
	CachedTokens *int64   `json:"cachedTokens,omitempty"` // 已包含在输入里，不再加到总Token
	Credit       *float64 `json:"credit,omitempty"`       // usage.credit，而不是余额差
	LatencyMS    float64  `json:"latencyMs"`
	TTFTMS       *float64 `json:"ttftMs,omitempty"`
	TTFTMetric   string   `json:"ttftMetric,omitempty"` // 空=旧首响应体；v2=有效内容增量，仅v2参与新均值
}

type statsDocument struct {
	Schema    int                     `json:"schema"`
	StartedAt int64                   `json:"startedAt"`
	Accounts  map[string]statsAccount `json:"accounts"`
	Requests  []statsRequest          `json:"requests"`
}

var requestStats = struct {
	sync.Mutex
	document  statsDocument
	revision  uint64
	saved     uint64
	lastSaved int64
	mode      string // pending / file / memory
	problem   string // 安全的白话原因，不回传底层错误或用户文件内容
	blocked   bool   // 坏文件或符号链接不得被自动覆盖
	enabled   bool
}{document: statsDocument{Schema: statsSchema, Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}, mode: "pending"}

var statsWriteMu sync.Mutex

// 调用方持有 accountMu。摘要不含 Token，续期不改变归属，企业/站点切换改变归属。
func statsAccountIDLocked(acc *Account) (string, statsAccount) {
	if acc == nil || acc.Auth == nil {
		return "", statsAccount{}
	}
	auth := acc.Auth
	site := acc.Profile().Key
	key := filepath.Clean(acc.Path) + "\x00" + site + "\x00" + auth.Account.UID + "\x00" + strings.TrimSpace(auth.Account.EnterpriseID)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), statsAccount{File: acc.Path, Edition: site}
}

func statsModelStatesLocked(acc *Account) map[string]modelStateSnapshot {
	out := make(map[string]modelStateSnapshot, len(acc.ModelStates))
	for model, state := range acc.ModelStates {
		if state == nil {
			continue
		}
		out[model] = modelStateSnapshot{
			CostClass: state.CostClass, CooldownUntil: unixOrZero(state.CooldownUntil),
			QuotaBlocked: state.QuotaBlocked, NextProbeAt: unixOrZero(state.NextProbeAt),
			LastReason: truncate(redactSensitiveText(state.LastReason), 400), ObservedAt: unixOrZero(state.ObservedAt),
		}
	}
	return out
}

// 和既有状态快照一起捕获账本；这里不做文件I/O，持锁顺序始终 accountMu→requestStats。
func captureAccountStatsLocked() {
	requestStats.Lock()
	defer requestStats.Unlock()
	if !requestStats.enabled {
		return
	}
	for _, acc := range accounts {
		id, view := statsAccountIDLocked(acc)
		if id == "" {
			continue
		}
		view.ModelStates = statsModelStatesLocked(acc)
		if !reflect.DeepEqual(requestStats.document.Accounts[id], view) {
			requestStats.document.Accounts[id] = view
			requestStats.revision++
		}
	}
}

func restoreStatsAccountStatesLocked() {
	requestStats.Lock()
	defer requestStats.Unlock()
	if !requestStats.enabled {
		return
	}
	for _, acc := range accounts {
		id, _ := statsAccountIDLocked(acc)
		saved, ok := requestStats.document.Accounts[id]
		if !ok || acc.Disabled {
			continue
		}
		acc.ModelStates = make(map[string]*modelRuntimeState, len(saved.ModelStates))
		for model, row := range saved.ModelStates {
			state := &modelRuntimeState{CostClass: row.CostClass, CooldownUntil: timeFromUnix(row.CooldownUntil),
				QuotaBlocked: row.QuotaBlocked, NextProbeAt: timeFromUnix(row.NextProbeAt),
				LastReason: row.LastReason, ObservedAt: timeFromUnix(row.ObservedAt)}
			sanitizeRestoredModelState(state)
			acc.ModelStates[normalizeModelName(model)] = state
		}
	}
}

func loadOptionalStats() {
	trace := newTraceID()
	requestStats.Lock()
	defer requestStats.Unlock()
	requestStats.document = statsDocument{Schema: statsSchema, StartedAt: time.Now().UnixMilli(), Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}
	requestStats.revision, requestStats.saved, requestStats.lastSaved = 0, 0, 0
	requestStats.blocked, requestStats.mode, requestStats.problem = false, "pending", ""
	requestStats.enabled = true
	info, err := os.Lstat(statsFile)
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("[统计文件] traceId=%s 阶段=启动读取 结果=文件不存在 业务影响=从空统计启动，不影响网关；首次有数据后尝试落盘", trace)
		return
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > statsMaxFileBytes {
		requestStats.mode, requestStats.problem, requestStats.blocked = "memory", "统计文件不可安全读取，保留原文件，仅使用内存", true
		log.Printf("[统计文件] traceId=%s 阶段=启动读取 结果=跳过 业务影响=%s", trace, requestStats.problem)
		return
	}
	f, err := os.Open(statsFile)
	if err != nil {
		requestStats.mode, requestStats.problem, requestStats.blocked = "memory", "统计文件无法读取，保留原文件，仅使用内存", true
		log.Printf("[统计文件] traceId=%s 阶段=启动读取 结果=失败 业务影响=%s", trace, requestStats.problem)
		return
	}
	defer f.Close()
	var doc statsDocument
	decoder := json.NewDecoder(io.LimitReader(f, statsMaxFileBytes+1))
	err = decoder.Decode(&doc)
	var trailing any
	if err != nil || decoder.Decode(&trailing) != io.EOF || !validStatsDocument(doc) {
		requestStats.mode, requestStats.problem, requestStats.blocked = "memory", "统计文件格式无效，保留原文件，仅使用内存", true
		log.Printf("[统计文件] traceId=%s 阶段=启动读取 结果=失败 业务影响=%s", trace, requestStats.problem)
		return
	}
	if doc.Requests == nil {
		doc.Requests = []statsRequest{}
	}
	requestStats.document, requestStats.mode = doc, "file"
	requestStats.lastSaved = info.ModTime().UnixMilli()
	log.Printf("[统计文件] traceId=%s 阶段=启动读取 结果=成功 账号=%d 请求记录=%d 说明=不含凭据或正文", trace, len(doc.Accounts), len(doc.Requests))
}

func validStatsDocument(doc statsDocument) bool {
	if doc.Schema != statsSchema || doc.StartedAt <= 0 || doc.Accounts == nil {
		return false
	}
	for _, row := range doc.Requests {
		if _, ok := doc.Accounts[row.Account]; !ok || row.At <= 0 || row.LatencyMS < 0 ||
			row.Status < 100 || row.Status > 599 || math.IsNaN(row.LatencyMS) || math.IsInf(row.LatencyMS, 0) {
			return false
		}
		for _, value := range []*int64{row.TotalTokens, row.InputTokens, row.OutputTokens, row.CachedTokens} {
			if value != nil && *value < 0 {
				return false
			}
		}
		for _, value := range []*float64{row.Credit, row.TTFTMS} {
			if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
				return false
			}
		}
	}
	return true
}

func flushOptionalStats() error {
	statsWriteMu.Lock()
	defer statsWriteMu.Unlock()
	accountMu.Lock()
	captureAccountStatsLocked()
	accountMu.Unlock()
	requestStats.Lock()
	if requestStats.blocked || requestStats.revision == requestStats.saved {
		requestStats.Unlock()
		return nil
	}
	revision := requestStats.revision
	raw, err := json.Marshal(requestStats.document)
	requestStats.Unlock()
	if err == nil && len(raw) > statsMaxFileBytes {
		err = errors.New("统计文件达到安全大小上限")
	}
	if err == nil {
		err = writeOptionalStatsFile(raw)
	}
	requestStats.Lock()
	previous := requestStats.mode
	if err != nil {
		requestStats.mode, requestStats.problem = "memory", "统计暂时无法写入，仍在内存统计，稍后自动重试"
	} else {
		requestStats.saved, requestStats.mode, requestStats.problem = revision, "file", ""
		requestStats.lastSaved = time.Now().UnixMilli()
	}
	requestStats.Unlock()
	if err != nil && previous != "memory" {
		log.Printf("[统计文件] traceId=%s 阶段=原子落盘 结果=失败 业务影响=继续内存统计，不阻塞模型请求，旧文件保留", newTraceID())
	} else if err == nil && previous != "file" {
		log.Printf("[统计文件] traceId=%s 阶段=原子落盘 结果=成功 字节=%d 业务影响=统计已保存或已恢复写入", newTraceID(), len(raw))
	}
	return err
}

func writeOptionalStatsFile(raw []byte) error {
	if info, err := os.Lstat(statsFile); err == nil && !info.Mode().IsRegular() {
		return errors.New("统计路径不是普通文件")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// 临时文件在同一工作目录，0600；完整写入、sync、关闭后才原子替换旧文件。
	f, err := os.CreateTemp(".", ".stats-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, statsFile)
}

func statsPersistenceLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = flushOptionalStats()
			return
		case <-ticker.C:
			_ = flushOptionalStats()
		}
	}
}

type statsRequestContextKey struct{}
type statsAttempt struct {
	id       string
	account  statsAccount
	model    string
	started  time.Time
	usage    map[string]any
	ttft     *float64
	complete bool
}

func statsAttemptFor(r *http.Request) *statsAttempt {
	if r == nil {
		return nil
	}
	attempt, _ := r.Context().Value(statsRequestContextKey{}).(*statsAttempt)
	return attempt
}

func statsSelectAccount(r *http.Request, acc *Account, model string) {
	p := statsAttemptFor(r)
	if p == nil {
		return
	}
	accountMu.Lock()
	id, account := statsAccountIDLocked(acc)
	accountMu.Unlock()
	*p = statsAttempt{id: id, account: account, model: normalizeModelName(model), started: requestStartFor(r)}
}

func statsObserveUsage(r *http.Request, usage map[string]any, complete bool) {
	if p := statsAttemptFor(r); p != nil {
		p.usage, p.complete = usage, complete
	}
}

func statsTokenValue(usage map[string]any, field string) *int64 {
	if value, ok := usageNumber(usage, field); ok && value >= 0 {
		return &value
	}
	return nil
}

func statsCachedValue(usage map[string]any) *int64 {
	var fallback *int64
	for _, field := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens"} {
		if v := statsTokenValue(usage, field); v != nil {
			if *v > 0 {
				return v
			}
			fallback = v
		}
	}
	for _, field := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if nested, ok := usage[field].(map[string]any); ok {
			if v := statsTokenValue(nested, "cached_tokens"); v != nil {
				if *v > 0 {
					return v
				}
				fallback = v
			}
		}
	}
	return fallback
}

// 每次实际账号上游调用结束记录一次，失败重试的账号也单独记；未选号的鉴权拒绝不算账号消耗。
func statsFinishAttempt(r *http.Request, status int) {
	p := statsAttemptFor(r)
	if p == nil || p.id == "" {
		return
	}
	row := statsRequest{Account: p.id, Model: p.model, At: time.Now().UnixMilli(), Status: status,
		Complete: p.complete && status < 400, LatencyMS: durationMilliseconds(time.Since(p.started)), TTFTMS: p.ttft, TTFTMetric: ttftMetric,
		InputTokens: statsTokenValue(p.usage, "prompt_tokens"), OutputTokens: statsTokenValue(p.usage, "completion_tokens"),
		CachedTokens: statsCachedValue(p.usage)}
	if n, ok := usageTotalTokens(p.usage); ok && n >= 0 {
		row.TotalTokens = &n
	}
	if credit, ok := usageCredit(p.usage); ok && credit >= 0 && !math.IsNaN(credit) && !math.IsInf(credit, 0) {
		row.Credit = &credit
	}
	requestStats.Lock()
	if !requestStats.enabled {
		requestStats.Unlock()
		*p = statsAttempt{}
		return
	}
	if requestStats.document.StartedAt == 0 {
		requestStats.document.StartedAt = row.At
	}
	if _, ok := requestStats.document.Accounts[p.id]; !ok {
		requestStats.document.Accounts[p.id] = p.account
	}
	requestStats.document.Requests = append(requestStats.document.Requests, row)
	requestStats.revision++
	requestStats.Unlock()
	log.Printf("[请求统计] traceId=%s 账号=%s 模型=%s 状态=%d 完整=%t 有Token=%t 有积分=%t 总耗时ms=%.2f TTFT口径=%s 有首Token=%t 说明=只记录实测usage，不记录正文；文件写入由后台执行",
		debugTraceID(r), p.account.File, p.model, row.Status, row.Complete, row.TotalTokens != nil, row.Credit != nil, row.LatencyMS, row.TTFTMetric, row.TTFTMS != nil)
	*p = statsAttempt{}
}

// 指标中每种未知都有样本数，0费用与缺失费用、未知首字与真正0均可区分。
type statsTotals struct {
	Requests          int64    `json:"requests"`
	Success           int64    `json:"success"`
	Failed            int64    `json:"failed"`
	Tokens            int64    `json:"tokens"`
	InputTokens       int64    `json:"inputTokens"`
	OutputTokens      int64    `json:"outputTokens"`
	CachedTokens      int64    `json:"cachedTokens"`
	TokenSamples      int64    `json:"tokenSamples"`
	Credits           float64  `json:"credits"`
	CreditSamples     int64    `json:"creditSamples"`
	TTFTSamples       int64    `json:"ttftSamples"`
	LegacyTTFTSamples int64    `json:"legacyTtftSamples"` // 保留历史，但不当作新口径Token样本
	AvgLatencyMS      *float64 `json:"avgLatencyMs"`
	AvgTTFTMS         *float64 `json:"avgTtftMs"`
	latencySum        float64
	ttftSum           float64
}

func (s *statsTotals) add(r statsRequest) {
	s.Requests++
	if r.Complete {
		s.Success++
	} else {
		s.Failed++
	}
	if r.TotalTokens != nil {
		s.Tokens += *r.TotalTokens
		s.TokenSamples++
	}
	if r.InputTokens != nil {
		s.InputTokens += *r.InputTokens
	}
	if r.OutputTokens != nil {
		s.OutputTokens += *r.OutputTokens
	}
	if r.CachedTokens != nil {
		s.CachedTokens += *r.CachedTokens
	}
	if r.Credit != nil {
		s.Credits += *r.Credit
		s.CreditSamples++
	}
	s.latencySum += r.LatencyMS
	if r.TTFTMS != nil && r.TTFTMetric != ttftMetric {
		s.LegacyTTFTSamples++
	}
	if r.TTFTMS != nil && r.TTFTMetric == ttftMetric && r.Complete && r.Status < 400 {
		s.ttftSum += *r.TTFTMS
		s.TTFTSamples++
	}
}

func (s *statsTotals) average() {
	if s.Requests > 0 {
		v := s.latencySum / float64(s.Requests)
		s.AvgLatencyMS = &v
	}
	if s.TTFTSamples > 0 {
		v := s.ttftSum / float64(s.TTFTSamples)
		s.AvgTTFTMS = &v
	}
}

type statsBucket struct {
	Start int64  `json:"start"`
	Label string `json:"label"`
	statsTotals
}

// statsModelRow 是明细表的按模型聚合：同一时间范围与账号过滤下按用量排名。
type statsModelRow struct {
	Model string `json:"model"`
	statsTotals
}

// statsModelLatency 与时间桶一一对应。没有调用的桶保持 nil，前端断线，不画成 0。
type statsModelLatency struct {
	Model        string     `json:"model"`
	AvgLatencyMS []*float64 `json:"avgLatencyMs"`
	AvgTTFTMS    []*float64 `json:"avgTtftMs"`
}

type statsLatencyCell struct {
	sum     float64
	n       int
	ttftSum float64
	ttftN   int
}

var statsZone = time.FixedZone("UTC+8", 8*3600)

func statsBucketStart(at time.Time, period string) time.Time {
	t := at.In(statsZone)
	switch period {
	case "hour":
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, statsZone)
	case "week":
		d := (int(t.Weekday()) + 6) % 7
		return time.Date(t.Year(), t.Month(), t.Day()-d, 0, 0, 0, 0, statsZone)
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, statsZone)
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, statsZone)
	}
}

func statsNextBucket(at time.Time, period string) time.Time {
	switch period {
	case "hour":
		return at.Add(time.Hour)
	case "week":
		return at.AddDate(0, 0, 7)
	case "month":
		return at.AddDate(0, 1, 0)
	default:
		return at.AddDate(0, 0, 1)
	}
}

// 只读且走既有管理鉴权。UTC+8 日历日，周从周一开始；end 为包含的结束日。
func handleWebUIStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "统计接口只读，仅支持 GET")
		return
	}
	q := r.URL.Query()
	period := q.Get("period")
	if period == "" {
		period = "day"
	}
	if period != "hour" && period != "day" && period != "week" && period != "month" {
		writeJSONError(w, http.StatusBadRequest, "时间维度只能是 hour/day/week/month")
		return
	}
	now := time.Now().In(statsZone)
	to := statsBucketStart(now, "day").AddDate(0, 0, 1)
	from := to.AddDate(0, 0, -30)
	switch period {
	case "hour":
		from = to.AddDate(0, 0, -1)
	case "week":
		from = to.AddDate(0, 0, -84)
	case "month":
		from = statsBucketStart(now, "month").AddDate(0, -11, 0)
	}
	if q.Get("start") != "" {
		t, err := time.ParseInLocation("2006-01-02", q.Get("start"), statsZone)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "开始日期格式应为 YYYY-MM-DD")
			return
		}
		from = t
	}
	if q.Get("end") != "" {
		t, err := time.ParseInLocation("2006-01-02", q.Get("end"), statsZone)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "结束日期格式应为 YYYY-MM-DD")
			return
		}
		to = t.AddDate(0, 0, 1)
	}
	if !from.Before(to) || to.Sub(from) > 10*366*24*time.Hour {
		writeJSONError(w, http.StatusBadRequest, "日期范围无效或超过10年")
		return
	}
	buckets := []statsBucket{}
	index := map[int64]int{}
	for at := statsBucketStart(from, period); at.Before(to); at = statsNextBucket(at, period) {
		if len(buckets) >= 800 {
			writeJSONError(w, http.StatusBadRequest, "时间桶超过800，请缩小范围或选择日/周/月")
			return
		}
		label := at.Format("2006-01-02")
		if period == "hour" {
			label = at.Format("01-02 15:00")
		} else if period == "month" {
			label = at.Format("2006-01")
		}
		index[at.UnixMilli()] = len(buckets)
		buckets = append(buckets, statsBucket{Start: at.UnixMilli(), Label: label})
	}
	requestStats.Lock()
	account := q.Get("account")
	if account != "" {
		if _, ok := requestStats.document.Accounts[account]; !ok {
			requestStats.Unlock()
			writeJSONError(w, http.StatusBadRequest, "账号统计标识不存在")
			return
		}
	}
	choices := []map[string]string{}
	for id, a := range requestStats.document.Accounts {
		choices = append(choices, map[string]string{"id": id, "file": a.File, "edition": a.Edition})
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i]["file"] == choices[j]["file"] {
			return choices[i]["id"] < choices[j]["id"]
		}
		return choices[i]["file"] < choices[j]["file"]
	})
	var summary statsTotals
	byModel := map[string]*statsModelRow{}
	latencyCells := map[string][]statsLatencyCell{}
	for _, row := range requestStats.document.Requests {
		if (account != "" && row.Account != account) || row.At < from.UnixMilli() || row.At >= to.UnixMilli() {
			continue
		}
		summary.add(row)
		m := byModel[row.Model]
		if m == nil {
			m = &statsModelRow{Model: row.Model}
			byModel[row.Model] = m
		}
		m.add(row)
		i := index[statsBucketStart(time.UnixMilli(row.At), period).UnixMilli()]
		buckets[i].add(row)
		cells := latencyCells[row.Model]
		if cells == nil {
			cells = make([]statsLatencyCell, len(buckets))
			latencyCells[row.Model] = cells
		}
		cells[i].sum += row.LatencyMS
		cells[i].n++
		// 首字与总耗时同一时间桶、同一模型分别平均，口径与总览摘要一致。
		if row.TTFTMS != nil && row.TTFTMetric == ttftMetric && row.Complete && row.Status < 400 {
			cells[i].ttftSum += *row.TTFTMS
			cells[i].ttftN++
		}
	}
	startedAt, mode, problem, savedAt := requestStats.document.StartedAt, requestStats.mode, requestStats.problem, requestStats.lastSaved
	requestStats.Unlock()
	summary.average()
	for i := range buckets {
		buckets[i].average()
	}
	// 明细按模型排名：有调用的才列出，用量（Token）多的在前，Token 未知时用请求数兜底。
	models := make([]*statsModelRow, 0, len(byModel))
	for _, m := range byModel {
		if m.Requests <= 0 {
			continue
		}
		m.average()
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Tokens != models[j].Tokens {
			return models[i].Tokens > models[j].Tokens
		}
		if models[i].Requests != models[j].Requests {
			return models[i].Requests > models[j].Requests
		}
		return models[i].Model < models[j].Model
	})
	// 耗时线与排名使用同一批模型、同一时间和账号范围，每条线只平均该模型自己的调用。
	modelLatency := make([]statsModelLatency, 0, len(models))
	for _, m := range models {
		points := make([]*float64, len(buckets))
		ttftPoints := make([]*float64, len(buckets))
		if cells := latencyCells[m.Model]; cells != nil {
			for i, cell := range cells {
				if cell.n > 0 {
					v := cell.sum / float64(cell.n)
					points[i] = &v
				}
				if cell.ttftN > 0 {
					v := cell.ttftSum / float64(cell.ttftN)
					ttftPoints[i] = &v
				}
			}
		}
		modelLatency = append(modelLatency, statsModelLatency{Model: m.Model, AvgLatencyMS: points, AvgTTFTMS: ttftPoints})
	}
	log.Printf("[统计查询] traceId=%s 维度=%s 单账号=%t 时间桶=%d 请求记录=%d 耗时线条=%d 存储=%s 结果=成功 说明=耗时按模型分别平均，不把各模型合成一条；UTC+8，周一开周；仅读内存聚合，不请求上游", debugTraceID(r), period, account != "", len(buckets), summary.Requests, len(modelLatency), mode)
	writeAdminData(w, r, map[string]any{
		"period": period, "account": account, "timezone": "Asia/Shanghai",
		"start": from.Format("2006-01-02"), "end": to.AddDate(0, 0, -1).Format("2006-01-02"),
		"startedAt": startedAt, "accounts": choices, "summary": summary, "buckets": buckets, "models": models, "modelLatency": modelLatency,
		"ttftMetric": ttftMetric,
		"storage":    map[string]any{"mode": mode, "message": problem, "lastSavedAt": savedAt},
	})
}
