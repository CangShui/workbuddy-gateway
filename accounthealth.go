package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type accountHealth struct {
	Percent *float64 `json:"percent"`
	State   string   `json:"state"` // healthy / degraded / unavailable / empty / unknown
}

// 保持账号表的可用口径；全局主动停用也在分母内，不能被排除后虚报100%。
// 这不是某个指定模型的可调度率，不把单模型冷却等同于整个账号失效。
func summarizeAccountHealth(accs []accountSnapshot, stale bool) accountHealth {
	if stale {
		return accountHealth{State: "unknown"}
	}
	if len(accs) == 0 {
		return accountHealth{State: "empty"}
	}
	healthy := 0
	for _, acc := range accs {
		if acc.State == "active" && (acc.SchedulingEnabled == nil || *acc.SchedulingEnabled) {
			healthy++
		}
	}
	// 向下保留一位，避免999/1000四舍五入成100%。
	percent := math.Floor(float64(healthy)*1000/float64(len(accs))) / 10
	state := "degraded"
	if healthy == len(accs) {
		state = "healthy"
	} else if healthy == 0 {
		state = "unavailable"
	}
	return accountHealth{Percent: &percent, State: state}
}

type accountAlertRule struct {
	Label    string
	Severity string
	Message  string
}

var accountAlertRules = map[string]accountAlertRule{
	"disabled":       {"账号失效", "urgent", "登录授权已失效，账号已停止调度，请重新登录。"},
	"expired":        {"账号过期", "important", "访问令牌已过期，请检查自动续期结果或重新登录。"},
	"paid_exhausted": {"积分用尽", "important", "已确认账号额度耗尽，收费模型暂不可用；等待额度恢复。"},
	"cooldown":       {"账号冷却", "warning", "账号触发频率限制，冷却期间暂不调度，请等待自动恢复。"},
	"paused":         {"账号停用", "notice", "该账号被全局调度规则停用；这不是授权失效，可在账号池恢复。"},
}

func accountAlertKinds(acc accountSnapshot) []string {
	kinds := []string{}
	if acc.SchedulingEnabled != nil && !*acc.SchedulingEnabled {
		kinds = append(kinds, "paused")
	}
	state := acc.State
	if state == "quota_exhausted" {
		state = "paid_exhausted"
	}
	if _, ok := accountAlertRules[state]; ok {
		kinds = append(kinds, state)
	}
	return kinds
}

type accountLogEvent struct {
	At      int64  `json:"at"`
	Account string `json:"account"`
	State   string `json:"state"`
	Trace   string `json:"trace,omitempty"`
	File    string `json:"-"`
}

// 仅用于压低重复日志：不建立新的状态文件。启动及状态转换写一条普通日志，
// 让过期/停用（包括直接编辑配置）也能有真实的可追溯日志来源。
var accountStateLog = struct {
	sync.Mutex
	last map[string]string
}{last: map[string]string{}}

func logAccountStateChanges(accs []accountSnapshot, now int64) {
	accountStateLog.Lock()
	defer accountStateLog.Unlock()
	next := make(map[string]string, len(accs))
	trace := newTraceID()
	for _, acc := range accs {
		kinds := accountAlertKinds(acc)
		signature := acc.UID + "\x00" + acc.State + "\x00" + strings.Join(kinds, ",")
		next[acc.Path] = signature
		if accountStateLog.last[acc.Path] == signature {
			continue
		}
		if len(kinds) == 0 {
			kinds = []string{acc.State}
		}
		for _, kind := range kinds {
			event := accountLogEvent{At: now, Account: acc.Path, State: kind, Trace: trace}
			raw, err := json.Marshal(event)
			if err == nil {
				log.Printf("[账号状态] %s", raw)
			}
		}
	}
	accountStateLog.last = next
}

// 兼容已有普通日志。仅匹配已知日志标签/措辞，不将任意包含“失效”的正文当告警。
func parseAccountLogEvent(line, file string) (accountLogEvent, bool) {
	event := accountLogEvent{At: logLineTime(line), File: file}
	if event.At <= 0 || len(line) < 21 {
		return event, false
	}
	body := line[20:]
	if strings.HasPrefix(body, "[账号状态] ") {
		if json.Unmarshal([]byte(strings.TrimPrefix(body, "[账号状态] ")), &event) != nil {
			return accountLogEvent{}, false
		}
		event.File = file
		_, known := accountAlertRules[event.State]
		return event, known && event.At > 0 && event.Account != ""
	}
	accountBetween := func(suffix string) string {
		_, rest, ok := strings.Cut(body, "账号 ")
		if !ok {
			return ""
		}
		name, _, ok := strings.Cut(rest, suffix)
		if !ok {
			return ""
		}
		return strings.TrimSpace(name)
	}
	switch {
	case strings.HasPrefix(body, "[Cooldown] "):
		event.State, event.Account = "cooldown", accountBetween(" 触发频率限制")
	case strings.HasPrefix(body, "[Quota] "):
		if strings.Contains(body, "账号已冻结调度") || strings.Contains(body, "分配额度已用完") {
			event.State = "paid_exhausted"
			event.Account = accountBetween(" 额度查询成功")
			if event.Account == "" {
				event.Account = accountBetween(" 企业额度计算=")
			}
		}
	case strings.HasPrefix(body, "[Auth] "):
		for _, suffix := range []string{" 判定登录态失效", " 授权失效，已禁止调度", " 已停止调度；"} {
			if name := accountBetween(suffix); name != "" {
				event.Account, event.State = name, "disabled"
				break
			}
		}
	case strings.HasPrefix(body, "[管理开关] ") && strings.Contains(body, "结果=成功 类型=account ") && strings.Contains(body, " 启用=false "):
		_, rest, ok := strings.Cut(body, "目标=")
		if ok {
			quoted, _, ok := strings.Cut(rest, " 启用=")
			if ok {
				if name, err := strconv.Unquote(quoted); err == nil {
					event.Account, event.State = name, "paused"
				}
			}
		}
	}
	_, known := accountAlertRules[event.State]
	return event, known && event.Account != ""
}

const accountAlertReadBudget int64 = 4 << 20

type accountAlertScan struct {
	Events    map[string]accountLogEvent
	Scanned   int64
	Truncated bool
}

// 只扫描普通日志，尾部总预算4MiB；读取失败不影响健康度接口或模型请求。
func readAccountAlertLogs() (accountAlertScan, error) {
	scan := accountAlertScan{Events: map[string]accountLogEvent{}}
	info, err := os.Lstat(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	if !info.IsDir() {
		return scan, errors.New("日志路径不是目录")
	}
	entries, err := os.ReadDir(logDir)
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
	for _, name := range names {
		budget := accountAlertReadBudget - scan.Scanned
		if budget <= 0 {
			scan.Truncated = true
			break
		}
		f, err := root.Open(name)
		if err != nil {
			return scan, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return scan, errors.New("账号告警日志不可读")
		}
		size := min(info.Size(), budget)
		start := info.Size() - size
		data := make([]byte, size)
		n, readErr := f.ReadAt(data, start)
		f.Close()
		if (readErr != nil && readErr != io.EOF) || int64(n) != size {
			return scan, errors.New("账号告警日志读取中变化，请重试")
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
		// 忽略最后尚未写完整的一行。
		lines := bytes.Split(data, []byte{'\n'})
		for _, line := range lines[:len(lines)-1] {
			if event, ok := parseAccountLogEvent(string(line), name); ok {
				key := event.Account + "\x00" + event.State
				if old, exists := scan.Events[key]; !exists || event.At > old.At {
					scan.Events[key] = event
				}
			}
		}
	}
	return scan, nil
}

var accountAlertCache struct {
	sync.Mutex
	at   time.Time
	scan accountAlertScan
}

type accountAlertView struct {
	Account  string `json:"account"`
	State    string `json:"state"`
	Label    string `json:"label"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	At       int64  `json:"at"`
	LogFile  string `json:"logFile"`
}

func currentAccountAlerts(accs []accountSnapshot, scan accountAlertScan) ([]accountAlertView, int) {
	out := []accountAlertView{}
	missing := 0
	for _, acc := range accs {
		for _, kind := range accountAlertKinds(acc) {
			event, ok := scan.Events[acc.Path+"\x00"+kind]
			if !ok {
				missing++
				continue // 无日志不编造告警“记录”。
			}
			rule := accountAlertRules[kind]
			out = append(out, accountAlertView{Account: acc.Path, State: kind, Label: rule.Label,
				Severity: rule.Severity, Message: rule.Message, At: event.At, LogFile: event.File})
		}
	}
	// 最新日志在上；等级只决定颜色和标签，不改变排序。
	// 秒级时间相同则按账号、类别稳定排序，避免每次轮询跳动。
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].State < out[j].State
	})
	return out, missing
}

func handleWebUIAccountAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "账号告警只读，仅支持 GET")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	annotateAccountPolicies(snap.Accounts)
	accountAlertCache.Lock()
	if accountAlertCache.at.IsZero() || time.Since(accountAlertCache.at) >= 10*time.Second {
		scan, readErr := readAccountAlertLogs()
		if readErr != nil {
			accountAlertCache.Unlock()
			log.Printf("[账号告警] traceId=%s 阶段=读取普通日志 结果=失败 说明=不展示伪造空列表，不影响健康度与模型调用", debugTraceID(r))
			writeJSONError(w, http.StatusServiceUnavailable, "账号告警日志暂不可读，保留已有结果")
			return
		}
		accountAlertCache.scan, accountAlertCache.at = scan, time.Now()
	}
	scan := accountAlertCache.scan
	accountAlertCache.Unlock()
	alerts, missing := currentAccountAlerts(snap.Accounts, scan)
	limited := len(alerts) > 100
	if limited {
		alerts = alerts[:100]
	}
	log.Printf("[账号告警] traceId=%s 结果=成功 条目=%d 缺少日志=%d 扫描字节=%d 扫描截断=%t 排序=最新日志优先 说明=先剔除已恢复项，再按时间倒序取前100条；等级只用于颜色和标签，不含令牌或上游错误正文", debugTraceID(r), len(alerts), missing, scan.Scanned, scan.Truncated)
	writeAdminData(w, r, map[string]any{
		"alerts": alerts, "updatedAt": snap.UpdatedAt, "stale": time.Now().Unix()-snap.UpdatedAt > 15,
		"source": "gateway 普通日志", "missingLogCount": missing,
		"scanTruncated": scan.Truncated, "listTruncated": limited,
	})
}
