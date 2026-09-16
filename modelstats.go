package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 模型级统计（/v1/models 附表）
//
// 记录每个模型被请求的次数、成功/失败、首字响应时间（TTFT）、平均耗时、
// 最近请求时间、最近状态，以及该模型当前可服务账号数。
// 统计只服务于 monitor 展示，不参与调度决策。
// -----------------------------------------------------------------------------

type modelStat struct {
	Requests       int64
	Success        int64
	Failed         int64
	TTFTSum        time.Duration
	TTFTSamples    int64
	LatencySum     time.Duration
	LatencySamples int64
	LastRequestAt  time.Time
	LastStatus     string
	// 站点维度的免费/收费观测：同一个模型名在 cn 与 intl 可能一个免费一个收费。
	CNFreeSeen   bool
	CNPaidSeen   bool
	IntlFreeSeen bool
	IntlPaidSeen bool
}

var (
	modelStatsMu sync.Mutex
	modelStats   = map[string]*modelStat{}
)

func modelStatLocked(model string) *modelStat {
	model = normalizeModelName(model)
	if model == "" {
		model = "(默认)"
	}
	stat := modelStats[model]
	if stat == nil {
		stat = &modelStat{}
		modelStats[model] = stat
	}
	return stat
}

func recordModelRequest(model string) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.Requests++
	stat.LastRequestAt = time.Now()
	modelStatsMu.Unlock()
}

func recordModelSuccess(model string) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.Success++
	stat.LastStatus = "成功"
	modelStatsMu.Unlock()
}

func recordModelFailure(model, reason string) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.Failed++
	stat.LastStatus = "失败:" + reason
	modelStatsMu.Unlock()
}

func recordModelTTFT(model string, d time.Duration) {
	if d <= 0 {
		return
	}
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.TTFTSum += d
	stat.TTFTSamples++
	modelStatsMu.Unlock()
}

func recordModelLatency(model string, d time.Duration) {
	if d <= 0 {
		return
	}
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	stat.LatencySum += d
	stat.LatencySamples++
	modelStatsMu.Unlock()
}

// recordModelCostClass 记录一次免费/收费观测，按站点分别累计。
func recordModelCostClass(model, edition string, free bool) {
	modelStatsMu.Lock()
	stat := modelStatLocked(model)
	if edition == "intl" {
		if free {
			stat.IntlFreeSeen = true
		} else {
			stat.IntlPaidSeen = true
		}
	} else {
		if free {
			stat.CNFreeSeen = true
		} else {
			stat.CNPaidSeen = true
		}
	}
	modelStatsMu.Unlock()
}

// ttftReader 记录上游响应体首个非空读取的时间，作为首字响应时间近似值。
type ttftReader struct {
	r     io.Reader
	start time.Time
	once  sync.Once
	d     time.Duration
}

func newTTFTReader(r io.Reader, start time.Time) *ttftReader {
	return &ttftReader{r: r, start: start}
}

func (t *ttftReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.once.Do(func() { t.d = time.Since(t.start) })
	}
	return n, err
}

func (t *ttftReader) duration() time.Duration { return t.d }

// modelStatusFromError 从错误响应中提取简短状态标签（业务码优先，其次 HTTP 状态码）。
func modelStatusFromError(statusCode int, body string) string {
	if code := extractBusinessCode(body); code != "" {
		return code
	}
	if statusCode > 0 {
		return strconv.Itoa(statusCode)
	}
	return "error"
}

// extractBusinessCode 从响应体提取 "code":NNNN 形式的业务码。
func extractBusinessCode(body string) string {
	const marker = `"code":`
	idx := strings.Index(body, marker)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return ""
	}
	return rest[:end]
}

// modelStatSnapshot 是写入状态快照的单个模型统计。
type modelStatSnapshot struct {
	ID       string `json:"id"`
	Source   string `json:"source,omitempty"`
	CNFree   string `json:"cnFree,omitempty"`   // 国内站：是 | 否 | 混合 | -
	IntlFree string `json:"intlFree,omitempty"` // 国际站：是 | 否 | 混合 | -
	// AvailableAccounts 为两个站点合计的可服务账号数。
	AvailableAccounts int    `json:"availableAccounts"`
	Requests          int64  `json:"requests,omitempty"`
	Success           int64  `json:"success,omitempty"`
	Failed            int64  `json:"failed,omitempty"`
	AvgTTFTMs         int64  `json:"avgTtftMs,omitempty"`
	HasTTFT           bool   `json:"hasTtft,omitempty"`
	AvgLatencyMs      int64  `json:"avgLatencyMs,omitempty"`
	HasLatency        bool   `json:"hasLatency,omitempty"`
	LastRequestAt     int64  `json:"lastRequestAt,omitempty"`
	LastStatus        string `json:"lastStatus,omitempty"`
}

// modelServableLocked 只读判断账号当前能否服务该模型（不改变任何状态）。
// 与 nextAccountForModel 的可用性规则保持一致。
func modelServableLocked(acc *Account, model string, now time.Time) bool {
	if acc.Disabled || acc.CooldownUntil.After(now) {
		return false
	}
	state := acc.ModelStates[normalizeModelName(model)]
	if state == nil {
		if acc.QuotaExhausted {
			return true // 未知模型允许一次受控探测
		}
		return true
	}
	if state.CooldownUntil.After(now) || state.QuotaBlocked {
		return false
	}
	if !acc.QuotaExhausted {
		return true
	}
	switch state.CostClass {
	case modelCostFree:
		return true
	case modelCostPaid:
		return false
	default:
		return !now.Before(state.NextProbeAt)
	}
}

// modelSourceFromSnapshots 从模型统计行中取出目录来源版本（用于 monitor 标题）。
func modelSourceFromSnapshots(rows []modelStatSnapshot) string {
	for _, row := range rows {
		if row.Source != "" {
			return row.Source
		}
	}
	return ""
}

func freeLabel(freeSeen, paidSeen bool) string {
	switch {
	case freeSeen && paidSeen:
		return "混合"
	case freeSeen:
		return "是"
	case paidSeen:
		return "否"
	default:
		return "-"
	}
}

// buildModelStatSnapshots 汇总「官方/静态模型目录 ∪ 有统计的模型」的统计行。
func buildModelStatSnapshots(now time.Time, accs []*Account) []modelStatSnapshot {
	ids, source := mergedModelIDs()
	seen := make(map[string]bool, len(ids))
	rows := make([]modelStatSnapshot, 0, len(ids))

	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()

	appendRow := func(id string) {
		id = normalizeModelName(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		row := modelStatSnapshot{ID: id, Source: source}
		// 免费/收费按站点分别聚合：同一模型名在 cn 与 intl 结论可能不同，
		// 若混在一起会显示成无意义的“混合”。
		var cnFree, cnPaid, intlFree, intlPaid bool
		for _, acc := range accs {
			if modelServableLocked(acc, id, now) {
				row.AvailableAccounts++
			}
			state := acc.ModelStates[id]
			if state == nil {
				continue
			}
			isIntl := acc.Profile().Key == "intl"
			switch state.CostClass {
			case modelCostFree:
				if isIntl {
					intlFree = true
				} else {
					cnFree = true
				}
			case modelCostPaid:
				if isIntl {
					intlPaid = true
				} else {
					cnPaid = true
				}
			}
		}
		if stat := modelStats[id]; stat != nil {
			row.Requests = stat.Requests
			row.Success = stat.Success
			row.Failed = stat.Failed
			cnFree = cnFree || stat.CNFreeSeen
			cnPaid = cnPaid || stat.CNPaidSeen
			intlFree = intlFree || stat.IntlFreeSeen
			intlPaid = intlPaid || stat.IntlPaidSeen
			row.LastStatus = stat.LastStatus
			row.LastRequestAt = unixOrZero(stat.LastRequestAt)
			if stat.TTFTSamples > 0 {
				row.AvgTTFTMs = (stat.TTFTSum / time.Duration(stat.TTFTSamples)).Milliseconds()
				row.HasTTFT = true
			}
			if stat.LatencySamples > 0 {
				row.AvgLatencyMs = (stat.LatencySum / time.Duration(stat.LatencySamples)).Milliseconds()
				row.HasLatency = true
			}
		}
		row.CNFree = freeLabel(cnFree, cnPaid)
		row.IntlFree = freeLabel(intlFree, intlPaid)
		rows = append(rows, row)
	}

	for _, id := range ids {
		appendRow(id)
	}
	for id := range modelStats {
		appendRow(id)
	}
	return rows
}

func formatMilliseconds(ms int64, has bool) string {
	if !has {
		return "-"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// renderModelTable 渲染 /v1/models 统计附表。
func renderModelTable(rows []modelStatSnapshot) string {
	widths := []int{26, 21, 8, 8, 8, 8, 11, 9, 9, 12, 15}
	headers := []string{"模型", "来源", "国内免费", "国际免费", "可用账号", "请求", "成功/失败", "首字", "平均", "最近状态", "最近请求"}
	border := func() string {
		var b strings.Builder
		b.WriteByte('+')
		for _, width := range widths {
			b.WriteString(strings.Repeat("-", width+2))
			b.WriteByte('+')
		}
		return b.String()
	}
	row := func(cells []string) string {
		var b strings.Builder
		b.WriteByte('|')
		for i, cell := range cells {
			b.WriteByte(' ')
			b.WriteString(fitCell(cell, widths[i]))
			b.WriteString(" |")
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString(border() + "\n")
	b.WriteString(row(headers) + "\n")
	b.WriteString(border() + "\n")
	for _, r := range rows {
		last := "-"
		if r.LastRequestAt > 0 {
			last = time.Unix(r.LastRequestAt, 0).Format("01-02 15:04:05")
		}
		status := r.LastStatus
		if status == "" {
			status = "-"
		}
		b.WriteString(row([]string{
			r.ID, modelSourceLabel(r.Source), r.CNFree, r.IntlFree, strconv.Itoa(r.AvailableAccounts),
			strconv.FormatInt(r.Requests, 10),
			fmt.Sprintf("%d/%d", r.Success, r.Failed),
			formatMilliseconds(r.AvgTTFTMs, r.HasTTFT),
			formatMilliseconds(r.AvgLatencyMs, r.HasLatency),
			status, last,
		}) + "\n")
	}
	b.WriteString(border())
	return b.String()
}
