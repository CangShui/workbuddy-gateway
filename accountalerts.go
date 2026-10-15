package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// accountAlertMark 是写进状态快照的精简告警标记：只记状态、首次时间与日志位置，
// 不复制任何日志正文。页面据此渲染告警，因此不再依赖后台的日志扫描。
type accountAlertMark struct {
	State     string `json:"state"`
	At        int64  `json:"at"`
	LogFile   string `json:"logFile,omitempty"`
	LogOffset int64  `json:"logOffset,omitempty"`
}

// alertSourceSnapshot 写入快照，用来区分「新版内联告警」与「旧版快照需要回退扫日志」。
const alertSourceSnapshot = "snapshot"

var accountAlertMarks = struct {
	sync.Mutex
	current map[string]accountAlertMark
	last    map[string]string
}{current: map[string]accountAlertMark{}, last: map[string]string{}}

// logWriteOffset 返回当天日志文件当前长度，也就是下一条日志的字节偏移。
// 日志是追加写，所以这个偏移正好指向即将写入的那一行。
func logWriteOffset() (string, int64) {
	name := "gateway-" + time.Now().Format("2006-01-02") + ".log"
	info, err := os.Stat(filepath.Join(logDir, name))
	if err != nil {
		return name, -1
	}
	return name, info.Size()
}

// recordAccountAlertMarks 在状态变化时写一条普通日志，并把当前异常记成快照用的精简标记。
// 返回按凭据路径分组的标记，供 writeStatusSnapshot 一并落盘。
func recordAccountAlertMarks(accs []accountSnapshot, now int64) map[string][]accountAlertMark {
	accountAlertMarks.Lock()
	defer accountAlertMarks.Unlock()
	next := make(map[string]string, len(accs))
	current := map[string]accountAlertMark{}
	trace := newTraceID()
	for _, acc := range accs {
		kinds := accountAlertKinds(acc)
		signature := acc.UID + "\x00" + acc.State + "\x00" + strings.Join(kinds, ",")
		next[acc.Path] = signature
		changed := accountAlertMarks.last[acc.Path] != signature
		if len(kinds) == 0 {
			// 恢复正常也要留一条审计日志，但不再生成告警标记。
			if changed {
				if raw, err := json.Marshal(accountLogEvent{At: now, Account: acc.Path, State: acc.State, Trace: trace}); err == nil {
					log.Printf("[账号状态] %s", raw)
				}
			}
			continue
		}
		for _, kind := range kinds {
			key := acc.Path + "\x00" + kind
			mark, seen := accountAlertMarks.current[key]
			if !seen || mark.At <= 0 {
				mark = accountAlertMark{State: kind, At: now}
			}
			// 只有状态真的变化时才写日志，并记下那一行的文件与偏移。
			if changed && !seen {
				file, offset := logWriteOffset()
				if raw, err := json.Marshal(accountLogEvent{At: now, Account: acc.Path, State: kind, Trace: trace}); err == nil {
					log.Printf("[账号状态] %s", raw)
				}
				mark.LogFile, mark.LogOffset = file, offset
			}
			mark.State = kind
			current[key] = mark
		}
	}
	accountAlertMarks.last = next
	// 必须写回：否则标记每轮都会重建，定位符丢失、首次时间也会被刷新。
	accountAlertMarks.current = current
	out := make(map[string][]accountAlertMark, len(accs))
	for key, mark := range current {
		path := key[:strings.Index(key, "\x00")]
		out[path] = append(out[path], mark)
	}
	return out
}

// sortAccountAlerts 统一排序：时间倒序（最新在上），同秒按账号、类别稳定排序。
func sortAccountAlerts(out []accountAlertView) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].State < out[j].State
	})
}

// currentAlertsFromSnapshot 直接按快照里的标记生成告警视图，不再扫描日志文件。
// 快照标记与账号当前状态不一致时（例如刚恢复）不展示，保证「恢复即消除」。
func currentAlertsFromSnapshot(accs []accountSnapshot) []accountAlertView {
	out := []accountAlertView{}
	for _, acc := range accs {
		want := map[string]bool{}
		for _, kind := range accountAlertKinds(acc) {
			want[kind] = true
		}
		for _, mark := range acc.Alerts {
			rule, known := accountAlertRules[mark.State]
			if !known || !want[mark.State] {
				continue
			}
			out = append(out, accountAlertView{Account: acc.Path, State: mark.State, Label: rule.Label,
				Severity: rule.Severity, Message: rule.Message, At: mark.At, LogFile: mark.LogFile,
				LogOffset: mark.LogOffset})
		}
	}
	sortAccountAlerts(out)
	return out
}
