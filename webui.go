package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WebUI 是独立端口的管理台：鉴权设置、账号/模型开关允许受控写入配置。
// 开关通过配置名单影响调度，不删除凭据、不改变历史统计；凭据令牌一律脱敏。
//
//go:embed web/index.html web/app.js web/style.css
var webFS embed.FS

const (
	webUIPrefix      = "/ui"
	webUIPrefixSlash = "/ui/"
)

// webuiEnabled 返回当前进程是否启用管理台。
func webuiEnabled() bool { return cfg.WebUI }

// 管理 Key 可在首次本机访问时初始化，不能再强制开启模型 API 鉴权。
func webUIPreflightError() error {
	if cfg.WebUI && cfg.Port == cfg.WebPort {
		return errors.New("API 与 Web 端口不能相同")
	}
	return nil
}

// webUIIsExemptPath 判断该路径是否属于「无数据的静态外壳」，可免 API 密钥访问。
// 只有 /ui 与 /ui/ 下的静态资源免鉴权；/admin/api/* 一律需要密钥。
func webUIIsExemptPath(p string) bool {
	if !webuiEnabled() {
		return false
	}
	return p == webUIPrefix || p == webUIPrefixSlash || strings.HasPrefix(p, webUIPrefixSlash)
}

// registerWebUIRoutes 注册静态资源、只读数据与受鉴权保护的配置写入路由。
func registerWebUIRoutes(mux *http.ServeMux) {
	mux.HandleFunc(webUIPrefix, handleWebUIIndex)
	mux.HandleFunc(webUIPrefixSlash, handleWebUIIndex)
	mux.HandleFunc("/admin/api/status", handleWebUIStatus)
	mux.HandleFunc("/admin/api/models", handleWebUIModels)
	mux.HandleFunc("/admin/api/logs", handleWebUILogs)
	mux.HandleFunc("/admin/api/config", handleWebUIConfig)
	mux.HandleFunc("/admin/api/config-edit", handleWebUIConfigEdit)
	mux.HandleFunc("/admin/api/credentials", handleWebUICredentials)
	mux.HandleFunc("/admin/api/setup", handleWebUISetup)
	mux.HandleFunc("/admin/api/settings", handleWebUISettings)
	mux.HandleFunc("/admin/api/client-events", handleWebUIClientEvent)
	mux.HandleFunc("/admin/api/account-toggle", handleWebUIAccountToggle)
	mux.HandleFunc("/admin/api/model-toggle", handleWebUIModelToggle)
	mux.HandleFunc("/admin/api/credential-edit", handleWebUICredentialEdit)
	mux.HandleFunc("/admin/api/log-settings", handleWebUILogSettings)
	mux.HandleFunc("/admin/api/log-cleanup", handleWebUILogCleanup)
	mux.HandleFunc("/admin/api/tasks", handleWebUITasks)
	mux.HandleFunc("/admin/api/rate-detail", handleWebUIRateDetail)
	mux.HandleFunc("/admin/api/stats", handleWebUIStats)
	mux.HandleFunc("/admin/api/account-alerts", handleWebUIAccountAlerts)
	mux.HandleFunc("/admin/api/account-add", handleWebUIAccountAdd)
	mux.HandleFunc("/admin/api/account-cancel", handleWebUIAccountCancel)
	mux.HandleFunc("/admin/api/account-delete", handleWebUIAccountDelete)
}

// 只读日志视图：无额外调度器、任务状态文件或内存任务登记表。
type scheduledTaskEvent struct {
	Task      string `json:"task"`
	Phase     string `json:"phase"`
	Status    string `json:"status"`
	Trace     string `json:"trace"`
	PID       int    `json:"pid"`
	At        int64  `json:"at"`
	StartedAt int64  `json:"startedAt"`
	NextAt    int64  `json:"nextAt"`
	Detail    string `json:"detail"`
	File      string `json:"-"`
}

type scheduledTaskView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Schedule        string `json:"schedule"`
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"intervalSeconds,omitempty"`
	OneShot         bool   `json:"oneShot,omitempty"`
	LastRunAt       int64  `json:"lastRunAt,omitempty"`
	LastFinishedAt  int64  `json:"lastFinishedAt,omitempty"`
	NextRunAt       int64  `json:"nextRunAt,omitempty"`
	NextEstimated   bool   `json:"nextEstimated"`
	Status          string `json:"status"`
	Success         *bool  `json:"success"`
	Detail          string `json:"detail,omitempty"`
	LogFile         string `json:"logFile,omitempty"`
	TraceID         string `json:"traceId,omitempty"`
}

var taskLogBootAt = time.Now().UnixMilli()

const taskLogReadBudget int64 = 16 << 20

func taskViewDefinitions() []scheduledTaskView {
	keepaliveEnabled := len(cfg.KeepaliveHours) > 0
	return []scheduledTaskView{
		{ID: "config_reload", Name: "配置热加载", Schedule: "每1秒；高频日志每60秒汇总", Enabled: true, IntervalSeconds: 1},
		{ID: "credential_reload", Name: "凭据文件扫描", Schedule: fmt.Sprintf("每%d秒；高频日志每60秒汇总", cfg.ReloadInterval), Enabled: cfg.ReloadInterval > 0, IntervalSeconds: int64(cfg.ReloadInterval)},
		{ID: "status_snapshot", Name: "状态快照写入", Schedule: "每3秒；高频日志每60秒汇总", Enabled: true, IntervalSeconds: 3},
		{ID: "token_check", Name: "令牌有效期检查", Schedule: "每5分钟；临近过期才续期", Enabled: true, IntervalSeconds: 300},
		{ID: "keepalive", Name: "主动续期保活", Schedule: "每日 " + formatKeepaliveHours(cfg.KeepaliveHours) + "（服务器本地时间）", Enabled: keepaliveEnabled},
		{ID: "keepalive_catchup", Name: "启动续期补跑", Schedule: "启动后90秒；只处理久未续期账号", Enabled: keepaliveEnabled, OneShot: true},
		{ID: "quota_scan", Name: "额度及套餐查询", Schedule: "每5分钟；启动/事件可能提前触发", Enabled: true, IntervalSeconds: 300},
		{ID: "daily_checkin", Name: "每日签到", Schedule: "北京时间09:00；启动/事件可能提前触发", Enabled: true},
		{ID: "model_catalog", Name: "模型目录刷新", Schedule: fmt.Sprintf("每%d分钟；启动/事件可能提前触发", cfg.ModelsRefresh), Enabled: cfg.ModelsRefresh > 0, IntervalSeconds: int64(cfg.ModelsRefresh) * 60},
		{ID: "price_probe", Name: "模型价格探测", Schedule: "仅-allow-price-probes开启；启动后20秒；随后2或30分钟；单模型12小时限制", Enabled: cfg.automaticPriceProbesAllowed()},
		{ID: "log_cleanup", Name: "过期日志清理", Schedule: fmt.Sprintf("每小时；保留%d天，规则变化会提前执行", logRetentionDays.Load()), Enabled: logRetentionDays.Load() > 0, IntervalSeconds: 3600},
	}
}

func aggregateTaskViews(events []scheduledTaskEvent, now time.Time, pid int, bootAt int64) []scheduledTaskView {
	rows := taskViewDefinitions()
	for i := range rows {
		row := &rows[i]
		row.Status = "unknown"
		var execution, schedule scheduledTaskEvent
		for _, event := range events {
			if event.Task != row.ID || event.PID != pid || event.At < bootAt || event.At > now.Add(time.Minute).UnixMilli() {
				continue
			}
			if event.Phase == "scheduled" || event.Phase == "disabled" || event.NextAt > 0 {
				if event.At >= schedule.At {
					schedule = event
				}
			}
			if event.Phase != "started" && event.Phase != "completed" {
				continue
			}
			oldStart := execution.StartedAt
			if oldStart == 0 {
				oldStart = execution.At
			}
			newStart := event.StartedAt
			if newStart == 0 {
				newStart = event.At
			}
			// 旧轮次稍后结束不能覆盖已经启动的新轮次。
			if newStart > oldStart || newStart == oldStart && event.At >= execution.At {
				execution = event
			}
		}
		if execution.At > 0 {
			row.LastRunAt = execution.StartedAt / 1000
			if row.LastRunAt == 0 {
				row.LastRunAt = execution.At / 1000
			}
			row.Status, row.Detail, row.LogFile, row.TraceID = execution.Status, execution.Detail, execution.File, execution.Trace
			if execution.Phase == "completed" {
				row.LastFinishedAt = execution.At / 1000
			}
			switch row.Status {
			case "success":
				value := true
				row.Success = &value
			case "failed", "partial", "cancelled":
				value := false
				row.Success = &value
			}
		} else if !row.Enabled {
			row.Status = "disabled"
			row.Detail = "当前配置关闭该任务"
		}
		if row.Enabled && schedule.NextAt > 0 && schedule.Phase != "disabled" {
			next := schedule.NextAt
			if row.OneShot && execution.At > 0 {
				next = 0
			} else if next <= now.UnixMilli() && row.IntervalSeconds > 0 {
				step := row.IntervalSeconds * 1000
				next += ((now.UnixMilli()-next)/step + 1) * step
				row.NextEstimated = true
			} else if next <= now.UnixMilli() {
				next = 0
				row.Detail += "；计划时间已过，等待新的调度日志"
			}
			row.NextRunAt = next / 1000
		}
	}
	return rows
}

func readTaskEvents() ([]scheduledTaskEvent, int64, bool, error) {
	entries, err := os.ReadDir(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "gateway-") && logFilePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, 0, false, err
	}
	defer root.Close()
	var events []scheduledTaskEvent
	var scanned int64
	truncated := false
	for index, name := range names {
		budget := taskLogReadBudget - scanned
		if budget <= 0 {
			truncated = true
			break
		}
		f, err := root.Open(name)
		if err != nil {
			return nil, scanned, true, err
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			return nil, scanned, true, errors.New("任务日志不是可读普通文件")
		}
		size := min(st.Size(), budget)
		data := make([]byte, int(size))
		start := st.Size() - size
		count, readErr := f.ReadAt(data, start)
		f.Close()
		if readErr != nil && readErr != io.EOF {
			return nil, scanned, true, readErr
		}
		if int64(count) != size {
			return nil, scanned, true, errors.New("任务日志读取期间被截断，请重试")
		}
		scanned += size
		if start > 0 {
			truncated = true
			if end := bytes.IndexByte(data, '\n'); end >= 0 {
				data = data[end+1:]
			} else {
				data = nil
			}
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			marker := bytes.Index(line, []byte("[定时任务] "))
			if marker < 0 {
				continue
			}
			var event scheduledTaskEvent
			if json.Unmarshal(line[marker+len("[定时任务] "):], &event) != nil || event.Task == "" || event.At <= 0 {
				continue
			}
			event.File = name
			events = append(events, event)
		}
		if scanned >= taskLogReadBudget && index < len(names)-1 {
			truncated = true
		}
	}
	return events, scanned, truncated, nil
}

// -----------------------------------------------------------------------------
// 倍率来源（只读日志视图）
//
// 与定时任务页同源：不新增调度器、状态文件或账本，全部从现有普通日志解析。
// 日志里没有任何记录的模型，页面直接显示「没有日志」，不猜测来源。
// -----------------------------------------------------------------------------

type rateLogRecord struct {
	Site      string `json:"site"`
	Model     string `json:"model"`
	Source    string `json:"source"`
	Explain   string `json:"explain"`
	UpdatedAt int64  `json:"updatedAt"`
	Value     string `json:"value"`
	Detail    string `json:"detail"`
	Unknown   bool   `json:"unknown"`
	Priority  int    `json:"priority"`
	LogFile   string `json:"logFile"`
	LogLine   string `json:"logLine"`
}

// 优先级与 modelDisplayMultiplier 的取值顺序一致，保证「感叹号」解释的是控制台真正显示的那个值。
const (
	ratePriorityLearnedFree = 1
	ratePriorityProbeFree   = 2
	ratePriorityProbePaid   = 3
	ratePriorityPromoStale  = 4
	ratePriorityOfficial    = 6
	// 正常请求实测「收费」不参与取值（页面仍展示官方倍率），仅作为佐证列出。
	ratePriorityLearnedPaid = 7
)

var (
	rateOfficialRe   = regexp.MustCompile(`\[Models\] 官方倍率 站点=(\S+) 模型=(\S+) 原始credits="([^"]*)" 生效倍率=(\S+) 有倍率=(\S+) 促销="([^"]*)" 促销截止="([^"]*)" 促销已过期=(\S+) 来源=(\S+) 刷新时间=(\S+)`)
	rateProbeCacheRe = regexp.MustCompile(`\[ModelPrice\] 载入缓存结论 站点=(\S+) 模型=(\S+) 判定=(\S+) credit=(\S+) tokens=(\d+) 探测时间=(\S+) 说明=(.*?) 来源=缓存文件`)
	rateProbeLiveRe  = regexp.MustCompile(`\[ModelPrice\] 站点=(\S+) 账号=(\S+) 模型=(\S+) -> verdict=(\S+) credit=(\S+) tokens=(\d+) \((.*)\)\s*$`)
	rateLearnedRe    = regexp.MustCompile(`\[ModelCost\] requestId=\d+ 账号 (\S+) 站点=(\S+) 模型 (\S+) 实测 usage\.credit=(\S+?)，已学习为(免费|收费)模型`)
	rateProbeModeRe  = regexp.MustCompile(`\[ModelPrice\] .*阶段=自动探测调度 .*结果=(已关闭|已开启)`)
)

const rateLogReadBudget int64 = 16 << 20

// rateMaxOtherRecords 是「其他记录」最多列出的条数，避免同一结论重复刷屏。
const rateMaxOtherRecords = 4

func logLineTime(line string) int64 {
	if len(line) < 19 {
		return 0
	}
	if at, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local); err == nil {
		return at.Unix()
	}
	return 0
}

func probeValue(verdict string) (string, bool, int) {
	switch verdict {
	case "free":
		return "0.00x", false, ratePriorityProbeFree
	case "paid":
		return "收费(倍率未知)", true, ratePriorityProbePaid
	default:
		return "-", true, ratePriorityProbePaid
	}
}

func parseRateLine(line, file string) (rateLogRecord, bool) {
	// 先用三次子串判断挡掉绝大多数无关日志：脱敏正则和时间解析都很贵，
	// 不能对每一行都做，否则大日志下接口会超过调用方超时。
	if !strings.Contains(line, "[Models] 官方倍率") &&
		!strings.Contains(line, "[ModelPrice]") &&
		!strings.Contains(line, "[ModelCost]") {
		return rateLogRecord{}, false
	}
	at := logLineTime(line)
	if at == 0 {
		return rateLogRecord{}, false
	}
	base := rateLogRecord{UpdatedAt: at, LogFile: file}
	if m := rateOfficialRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[1], m[2]
		record.Source = "官方目录（" + m[9] + "）"
		expired := m[8] == "true"
		switch {
		case m[5] != "true":
			record.Value, record.Unknown, record.Priority = "-", true, ratePriorityOfficial
			record.Detail = "官方目录未给出有效倍率，原始 credits=" + m[3]
			record.Explain = "官方接口没有给出有效倍率"
		case expired:
			record.Value, record.Unknown, record.Priority = "-", true, ratePriorityPromoStale
			record.Detail = "官方促销已过期，原始 credits=" + m[3] + "、促销=" + m[6] + "、截止=" + m[7] + "，该数值不可信"
			record.Explain = "官方促销已于 " + m[7] + " 结束，接口仍返回 " + m[4] + "x，该数值不可信"
		default:
			record.Value, record.Priority = m[4]+"x", ratePriorityOfficial
			record.Detail = "官方目录原始 credits=" + m[3]
			record.Explain = "官方接口给出的目录倍率"
			if m[6] != "" {
				record.Detail += "、促销=" + m[6] + "、截止=" + m[7]
				record.Explain = "官方促销 " + m[6] + " 折算后的倍率"
			}
		}
		return record, true
	}
	if m := rateProbeCacheRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[1], m[2]
		record.Source = "价格探测（缓存结论）"
		record.Value, record.Unknown, record.Priority = probeValue(m[3])
		record.Detail = "判定=" + m[3] + " credit=" + m[4] + " tokens=" + m[5] + " 探测时间=" + m[6] + " " + m[7]
		record.Explain = "上次真实探测结论：收费（尚未复测）"
		if m[3] == "free" {
			record.Explain = "上次真实探测结论：免费（尚未复测）"
		}
		return record, true
	}
	if m := rateProbeLiveRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[1], m[3]
		record.Source = "价格探测（本机实测）"
		record.Value, record.Unknown, record.Priority = probeValue(m[4])
		record.Detail = "账号=" + m[2] + " 判定=" + m[4] + " credit=" + m[5] + " tokens=" + m[6] + " " + m[7]
		record.Explain = "网关发起真实请求实测：收费（官方未给出有效倍率）"
		if m[4] == "free" {
			record.Explain = "网关发起真实请求实测：免费"
		}
		return record, true
	}
	if m := rateLearnedRe.FindStringSubmatch(line); m != nil {
		record := base
		record.LogLine = redactSensitiveText(line)
		record.Site, record.Model = m[2], m[3]
		record.Source = "正常请求实测"
		record.Detail = "账号=" + m[1] + " usage.credit=" + m[4] + " 判定=" + m[5]
		if m[5] == "免费" {
			record.Value, record.Priority = "0.00x", ratePriorityLearnedFree
			record.Explain = "正常客户端请求返回 credit=0，判定免费"
		} else {
			record.Value, record.Priority = "实测收费", ratePriorityLearnedPaid
			record.Explain = "正常客户端请求实测收费，仅作佐证，不改变上方官方倍率"
		}
		return record, true
	}
	return rateLogRecord{}, false
}

type rateLogScan struct {
	Records          []rateLogRecord
	AutoProbeEnabled bool
	AutoProbeKnown   bool
	Scanned          int64
	Truncated        bool
}

// readRateEvents 从现有普通日志尾部解析倍率来源记录；不读调试日志、不改动任何文件。
func readRateEvents() (rateLogScan, error) {
	var scan rateLogScan
	entries, err := os.ReadDir(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "gateway-") && logFilePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return scan, err
	}
	defer root.Close()
	for index, name := range names {
		budget := rateLogReadBudget - scan.Scanned
		if budget <= 0 {
			scan.Truncated = true
			break
		}
		f, err := root.Open(name)
		if err != nil {
			scan.Truncated = true
			return scan, err
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			scan.Truncated = true
			return scan, errors.New("倍率日志不是可读普通文件")
		}
		size := min(st.Size(), budget)
		data := make([]byte, int(size))
		start := st.Size() - size
		count, readErr := f.ReadAt(data, start)
		f.Close()
		if readErr != nil && readErr != io.EOF {
			scan.Truncated = true
			return scan, readErr
		}
		if int64(count) != size {
			scan.Truncated = true
			return scan, errors.New("倍率日志读取期间被截断，请重试")
		}
		scan.Scanned += size
		if start > 0 {
			scan.Truncated = true
			if end := bytes.IndexByte(data, '\n'); end >= 0 {
				data = data[end+1:]
			} else {
				data = nil
			}
		}
		for _, raw := range bytes.Split(data, []byte{'\n'}) {
			if len(raw) == 0 {
				continue
			}
			line := string(raw)
			// 探测调度状态：用于告诉用户「未知」时怎么刷新，同样取自普通日志。
			if !scan.AutoProbeKnown && strings.Contains(line, "阶段=自动探测调度") {
				if m := rateProbeModeRe.FindStringSubmatch(line); m != nil {
					scan.AutoProbeEnabled, scan.AutoProbeKnown = m[1] == "已开启", true
				}
			}
			if record, ok := parseRateLine(line, name); ok {
				scan.Records = append(scan.Records, record)
			}
		}
		if scan.Scanned >= rateLogReadBudget && index < len(names)-1 {
			scan.Truncated = true
		}
	}
	return scan, nil
}

func aggregateRateDetails(records []rateLogRecord) map[string]map[string]any {
	grouped := map[string][]rateLogRecord{}
	for _, record := range records {
		key := record.Site + "|" + normalizeModelName(record.Model)
		grouped[key] = append(grouped[key], record)
	}
	out := make(map[string]map[string]any, len(grouped))
	for key, list := range grouped {
		sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
		bestIndex := 0
		for i, record := range list {
			if i == 0 {
				continue
			}
			if record.Priority < list[bestIndex].Priority ||
				record.Priority == list[bestIndex].Priority && record.UpdatedAt > list[bestIndex].UpdatedAt {
				bestIndex = i
			}
		}
		best := list[bestIndex]
		// 「其他记录」按来源+解释去重（多次重启会写出内容相同的记录），
		// 并且只保留最新的若干条，避免把同一句话重复堆给用户。
		others := make([]rateLogRecord, 0, len(list)-1)
		seenOthers := map[string]bool{}
		// 先把胜出记录的签名登记为已见，避免同一结论的旧记录又出现在「其他记录」里。
		seenOthers[best.Source+"|"+best.Explain+"|"+best.Value] = true
		for i, record := range list {
			if i != bestIndex {
				signature := record.Source + "|" + record.Explain + "|" + record.Value
				if seenOthers[signature] {
					continue
				}
				seenOthers[signature] = true
				others = append(others, record)
			}
		}
		if len(others) > rateMaxOtherRecords {
			others = others[:rateMaxOtherRecords]
		}
		out[key] = map[string]any{
			"site": best.Site, "model": best.Model, "source": best.Source,
			"updatedAt": best.UpdatedAt, "value": best.Value, "detail": best.Detail,
			"explain": best.Explain,
			"unknown": best.Unknown, "priority": best.Priority,
			"logFile": best.LogFile, "logLine": best.LogLine,
			"hasLog": true, "records": list, "others": others,
		}
	}
	return out
}

// 短暂缓存扫描结果：点击「感叹号」时按需拉取，避免每次点击都重扫日志。
var rateDetailCache struct {
	sync.Mutex
	at      time.Time
	payload map[string]any
}

const rateDetailCacheTTL = 20 * time.Second

func handleWebUIRateDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "只读数据，仅支持 GET")
		return
	}
	rateDetailCache.Lock()
	if rateDetailCache.payload != nil && time.Since(rateDetailCache.at) < rateDetailCacheTTL {
		payload := rateDetailCache.payload
		rateDetailCache.Unlock()
		log.Printf("[管理倍率来源] traceId=%s 结果=命中缓存 说明=仅读现有普通日志，不修改配置、不触发探测", debugTraceID(r))
		writeAdminData(w, r, payload)
		return
	}
	rateDetailCache.Unlock()
	scan, err := readRateEvents()
	if err != nil {
		log.Printf("[管理倍率来源] traceId=%s 结果=读取失败 原因=%s 说明=页面显示没有日志，不伪造来源", debugTraceID(r), safeDebugError(err))
		writeJSONError(w, http.StatusServiceUnavailable, "日志暂不可读，请稍后重试")
		return
	}
	now := time.Now()
	rates := aggregateRateDetails(scan.Records)
	refreshHint := rateRefreshHint(scan.AutoProbeEnabled)
	for _, entry := range rates {
		if entry["unknown"] == true {
			entry["refreshHint"] = refreshHint
		}
	}
	payload := map[string]any{
		"generatedAt": now.Unix(), "timezone": now.Format("MST -07:00"),
		"rates": rates, "recordCount": len(scan.Records),
		"readBytes": scan.Scanned, "readBudgetBytes": rateLogReadBudget, "truncated": scan.Truncated,
		"autoProbeEnabled": scan.AutoProbeEnabled, "autoProbeKnown": scan.AutoProbeKnown,
		"refreshHint": refreshHint,
		"noLogHint":   "普通日志中没有该模型的记录。用该站点的账号请求一次这个模型，就会产生新的倍率记录。",
		"source":      "gateway 普通日志",
		"note":        "倍率来源与更新时间全部取自普通日志；日志中没有记录的模型显示「没有日志」。目录倍率在首次或变化时记录，探测与实测结论在产生时记录；原始日志默认折叠。",
	}
	rateDetailCache.Lock()
	rateDetailCache.payload, rateDetailCache.at = payload, now
	rateDetailCache.Unlock()
	log.Printf("[管理倍率来源] traceId=%s 结果=读取成功 记录=%d 站点模型=%d 扫描字节=%d 截断=%t 自动探测已开启=%t 说明=仅读现有普通日志，不触发探测或目录刷新", debugTraceID(r), len(scan.Records), len(rates), scan.Scanned, scan.Truncated, scan.AutoProbeEnabled)
	writeAdminData(w, r, payload)
}

// rateRefreshHint 说明「未知」时怎样让倍率更新；探测开关状态同样取自普通日志。
func rateRefreshHint(autoProbeEnabled bool) string {
	if autoProbeEnabled {
		return "后台自动探测已开启，会在下一轮自动复测；也可以让该站点的账号请求一次这个模型，立即按 usage.credit 重新判定。"
	}
	return "后台自动探测已关闭。用该站点的账号请求一次这个模型，网关会按返回的 usage.credit 自动重新判定并写入日志。"
}

func handleWebUITasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "只读页面，仅支持 GET")
		return
	}
	events, scanned, truncated, err := readTaskEvents()
	if err != nil {
		log.Printf("[管理定时任务] traceId=%s 结果=读取失败 原因=%s 说明=保留页面上次数据，不伪装成任务成功", debugTraceID(r), safeDebugError(err))
		writeJSONError(w, http.StatusServiceUnavailable, "任务日志暂不可读，请稍后重试")
		return
	}
	now := time.Now()
	rows := aggregateTaskViews(events, now, os.Getpid(), taskLogBootAt)
	log.Printf("[管理定时任务] traceId=%s 结果=读取成功 任务数=%d 日志记录=%d 扫描字节=%d 截断=%t 说明=仅读现有日志，不触发任务或修改调度", debugTraceID(r), len(rows), len(events), scanned, truncated)
	writeAdminData(w, r, map[string]any{"tasks": rows, "generatedAt": now.Unix(), "timezone": now.Format("MST -07:00"), "readBytes": scanned, "readBudgetBytes": taskLogReadBudget, "truncated": truncated, "source": "gateway 日志；仅当前服务进程", "note": "高频本地任务每60秒记录最近一次执行，状态变化即时记录；下次时间按日志中的定时计划推算，事件触发可能提前。无日志时不判定成功。"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleWebUIIndex 提供管理台静态外壳与静态资源。
// 访问 /ui 时跳到 /ui/，保证相对资源路径正确；/ui/<file> 按名读取嵌入资源。
func handleWebUIIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/HEAD")
		return
	}
	if r.URL.Path == webUIPrefix {
		http.Redirect(w, r, webUIPrefixSlash, http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, webUIPrefixSlash)
	if name == "" {
		name = "index.html"
	}
	serveWebAsset(w, name)
}

// serveWebAsset 从嵌入资源读取单个静态文件，拒绝任何路径穿越。
func serveWebAsset(w http.ResponseWriter, name string) {
	name = path.Clean(name)
	if name == "." || strings.HasPrefix(name, "..") || strings.Contains(name, "/") {
		http.NotFound(w, nil)
		return
	}
	data, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	_, _ = w.Write(data)
}

// webGatewayMeta 是管理台展示的网关运行元信息（不含任何密钥）。
type webGatewayMeta struct {
	Version       string `json:"version"`
	Listen        string `json:"listen"`
	APIKeyEnabled bool   `json:"apiKeyEnabled"`
	WebUIEnabled  bool   `json:"webuiEnabled"`
	ModelSource   string `json:"modelSource,omitempty"`
	WebListen     string `json:"webListen,omitempty"`
}

// webStatusResponse 是 /admin/api/status 的响应体。
type webStatusResponse struct {
	Gateway     webGatewayMeta            `json:"gateway"`
	UpdatedAt   int64                     `json:"updatedAt"`
	Summary     map[string]int            `json:"summary"`
	SiteSummary map[string]map[string]int `json:"siteSummary"`
	Accounts    []accountSnapshot         `json:"accounts"`
	Models      []modelStatSnapshot       `json:"models"`
	Stale       bool                      `json:"stale"`
	Health      accountHealth             `json:"health"`
}

// readStatusSnapshot 读取 serve 周期写入的状态快照（与 monitor 命令同一数据源）。
func readStatusSnapshot() (statusSnapshot, error) {
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		return statusSnapshot{}, err
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return statusSnapshot{}, err
	}
	if snap.UpdatedAt <= 0 {
		return statusSnapshot{}, errors.New("状态快照没有有效更新时间")
	}
	return snap, nil
}

func summarizeAccounts(accs []accountSnapshot) map[string]int {
	summary := map[string]int{
		"total": len(accs), "active": 0, "cooldown": 0,
		"paidExhausted": 0, "expired": 0, "disabled": 0, "paused": 0,
	}
	for _, a := range accs {
		if a.SchedulingEnabled != nil && !*a.SchedulingEnabled {
			summary["paused"]++
			continue
		}
		switch a.State {
		case "active":
			summary["active"]++
		case "cooldown":
			summary["cooldown"]++
		case "quota_exhausted", "paid_exhausted":
			summary["paidExhausted"]++
		case "expired":
			summary["expired"]++
		case "disabled":
			summary["disabled"]++
		}
	}
	return summary
}

// summarizeAccountsBySite 按站点（cn/intl）分别汇总账号数量，供总览页分站点展示。
// 站点标识缺失时按国内站处理，与凭据默认站点保持一致。
func summarizeAccountsBySite(accs []accountSnapshot) map[string]map[string]int {
	sites := map[string][]accountSnapshot{}
	for _, a := range accs {
		key := a.Edition
		if key != "intl" {
			key = "cn"
		}
		sites[key] = append(sites[key], a)
	}
	return map[string]map[string]int{
		"cn":   summarizeAccounts(sites["cn"]),
		"intl": summarizeAccounts(sites["intl"]),
	}
}

func handleWebUIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	resp := webStatusResponse{
		Gateway: webGatewayMeta{
			Version:       version,
			Listen:        net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.Port)),
			WebListen:     net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.WebPort)),
			APIKeyEnabled: currentAPIKey() != "",
			WebUIEnabled:  webuiEnabled(),
			ModelSource:   modelSourceLabel(modelSourceFromSnapshots(snap.Models)),
		},
		UpdatedAt: snap.UpdatedAt,
		Accounts:  snap.Accounts,
		Models:    snap.Models,
		Stale:     time.Now().Unix()-snap.UpdatedAt > 15,
	}
	if resp.Accounts == nil {
		resp.Accounts = []accountSnapshot{}
	}
	annotateAccountPolicies(resp.Accounts)
	if resp.Models == nil {
		resp.Models = []modelStatSnapshot{}
	}
	resp.Summary = summarizeAccounts(resp.Accounts)
	resp.SiteSummary = summarizeAccountsBySite(resp.Accounts)
	resp.Health = summarizeAccountHealth(resp.Accounts, resp.Stale)
	log.Printf("[管理状态] traceId=%s 结果=读取成功 账号数=%d 模型数=%d 快照过旧=%t 说明=只展示快照，不调用上游或改变账本", debugTraceID(r), len(resp.Accounts), len(resp.Models), resp.Stale)
	writeAdminData(w, r, resp)
}

func handleWebUIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	models := snap.ModelCatalog
	if models == nil {
		models = snap.Models // 兼容旧版本状态快照。
	}
	if models == nil {
		models = []modelStatSnapshot{}
	}
	for i := range models {
		disabled, reason := modelDisabled(models[i].ID)
		enabled := !disabled
		models[i].Enabled, models[i].PolicyReason = &enabled, reason
		if disabled {
			models[i].AvailableAccounts = 0
		}
	}
	log.Printf("[管理模型] traceId=%s 结果=读取成功 模型数=%d", debugTraceID(r), len(models))
	writeAdminData(w, r, map[string]any{
		"source":     modelSourceLabel(modelSourceFromSnapshots(models)),
		"models":     models,
		"stale":      time.Now().Unix()-snap.UpdatedAt > 15,
		"policyMode": currentModelPolicyMode(),
	})
}

func webSnapshotUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[管理状态] traceId=%s 阶段=读取快照 结果=失败 原因=%v 状态码=503 业务影响=前端保留上次数据，不伪装成零账号", debugTraceID(r), err)
	writeJSONError(w, http.StatusServiceUnavailable, "状态快照暂不可用，请稍后重试；已有页面数据不是新的零账号结果")
}

var logFilePattern = managedLogPattern

type logFileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

// listLogFiles 仅列出 logs/ 下受限命名格式的日志文件，最新在前。
func listLogFiles() []logFileInfo {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return []logFileInfo{}
	}
	files := make([]logFileInfo, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || !logFilePattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, logFileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files
}

func handleWebUILogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	files := listLogFiles()
	name := r.URL.Query().Get("file")
	if name != "" && (baseNameSafe(name) != name || !logFilePattern.MatchString(name)) {
		log.Printf("[管理日志] traceId=%s 结果=拒绝 原因=日志文件名不合法 状态码=400", debugTraceID(r))
		writeJSONError(w, http.StatusBadRequest, "非法的日志文件名")
		return
	}
	if name == "" && len(files) > 0 {
		name = files[0].Name
	}

	linesN := 200
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > 2000 {
				n = 2000
			}
			linesN = n
		}
	}

	resp := map[string]any{"files": files, "file": name, "lines": []string{}, "truncated": false, "readBudgetBytes": webUILogReadBudget}
	if name != "" {
		log.Printf("[管理日志] traceId=%s 阶段=读取 文件=%s 请求行数=%d 扫描字节预算=%d", debugTraceID(r), name, linesN, webUILogReadBudget)
		lines, truncated, err := readWebLog(name, linesN)
		if err != nil {
			log.Printf("[管理日志] traceId=%s 结果=失败 文件=%s 原因=%v 状态码=503", debugTraceID(r), name, err)
			writeJSONError(w, http.StatusServiceUnavailable, "日志暂不可读，请刷新文件列表后重试")
			return
		}
		// 脱敏完整行副本，覆盖旧版本写出的日志；不改动文件或业务错误。
		if len(lines) > 0 {
			// 一批日志只建立一次秘密值替换器，避免逐行重复构建。
			lines = strings.Split(redactSensitiveText(strings.Join(lines, "\n")), "\n")
		}
		resp["lines"], resp["truncated"] = lines, truncated
		log.Printf("[管理日志] traceId=%s 结果=成功 返回行数=%d 预算截断=%t 秘密值已隐藏=true", debugTraceID(r), len(lines), truncated)
	}
	writeJSON(w, http.StatusOK, resp)
}

func readWebLog(name string, lines int) ([]string, bool, error) {
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	return readTailLines(f, lines, webUILogReadBudget)
}

// baseNameSafe 只取基名，避免任何目录穿越意图进入后续校验。
func baseNameSafe(name string) string {
	name = strings.TrimSpace(name)
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	return name
}

func handleWebUIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	runtimeConfigMu.Lock()
	data, err := os.ReadFile(runtimeConfigFile)
	runtimeConfigMu.Unlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"exists": false, "path": runtimeConfigFile, "content": ""})
		} else {
			log.Printf("[管理配置读取] traceId=%s 结果=失败 原因=%v", debugTraceID(r), err)
			writeJSONError(w, http.StatusServiceUnavailable, "配置文件暂不可读")
		}
		return
	}
	var parsed any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil || ensureJSONEOF(decoder) != nil {
		log.Printf("[管理配置读取] traceId=%s 结果=失败 原因=配置文件不是有效JSON，拒绝返回未脱敏原文", debugTraceID(r))
		writeJSONError(w, http.StatusServiceUnavailable, "配置文件暂不可解析")
		return
	}
	safe, err := json.MarshalIndent(redactDebugValue(parsed), "", "  ")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "配置展示编码失败")
		return
	}
	log.Printf("[管理配置读取] traceId=%s 结果=成功 管理Key与APIKey已隐藏=true 说明=磁盘配置仍按用户要求明文保存", debugTraceID(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":  true,
		"path":    runtimeConfigFile,
		"content": string(safe),
	})
}

// credentialView 是脱敏后的凭据视图：只保留非敏感字段，令牌仅显示前缀。
type credentialView struct {
	Path               string `json:"path"`
	Exists             bool   `json:"exists"`
	Error              string `json:"error,omitempty"`
	Edition            string `json:"edition,omitempty"`
	Nickname           string `json:"nickname,omitempty"`
	UID                string `json:"uid,omitempty"`
	EnterpriseID       string `json:"enterpriseId,omitempty"`
	Domain             string `json:"domain,omitempty"`
	ExpiresAt          int64  `json:"expiresAt,omitempty"`
	RefreshExpiresAt   int64  `json:"refreshExpiresAt,omitempty"`
	LastRefreshTime    int64  `json:"lastRefreshTime,omitempty"`
	AccessTokenMasked  string `json:"accessTokenMasked,omitempty"`
	RefreshTokenMasked string `json:"refreshTokenMasked,omitempty"`
}

// maskToken 仅保留令牌前 6 位用于识别，其余以星号替代；空值保持为空。
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 6 {
		return "****"
	}
	return token[:6] + "****"
}

func handleWebUICredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	views := make([]credentialView, 0)
	for _, p := range collectConfiguredAuthPaths() {
		view := credentialView{Path: p, Exists: false}
		data, err := os.ReadFile(p)
		if err != nil {
			views = append(views, view)
			continue
		}
		var sa StoredAuth
		if err := json.Unmarshal(data, &sa); err != nil {
			view.Exists = true
			view.Error = "凭据文件解析失败"
			views = append(views, view)
			continue
		}
		view.Exists = true
		view.Edition = sa.Edition
		view.Nickname = sa.Account.Nickname
		view.UID = sa.Account.UID
		view.EnterpriseID = sa.Account.EnterpriseID
		view.Domain = sa.Auth.Domain
		view.ExpiresAt = sa.Auth.ExpiresAt
		view.RefreshExpiresAt = sa.Auth.RefreshExpiresAt
		view.LastRefreshTime = sa.Auth.LastRefreshTime
		view.AccessTokenMasked = maskToken(sa.Auth.AccessToken)
		view.RefreshTokenMasked = maskToken(sa.Auth.RefreshToken)
		views = append(views, view)
	}
	log.Printf("[管理凭据] traceId=%s 结果=读取完成 文件数=%d 说明=只返回脱敏视图，不触发刷新或写入", debugTraceID(r), len(views))
	writeAdminData(w, r, map[string]any{
		"hint":  "列表令牌已脱敏；点击编辑可由管理员读取和保存完整凭据。完整令牌不会写入日志或浏览器持久存储。",
		"files": views,
		// 保留字段名便于前端展示生成时间
		"generatedAt": time.Now().Unix(),
	})
}
