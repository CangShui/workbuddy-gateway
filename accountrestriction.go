package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errUpstreamAccountRestricted = errors.New("上游确认账号风控限制，本次请求已停止换号")

// restrictionEnvelope 只读取错误信封，绝不在 choices 的正文或工具参数里寻找风控文案。
type restrictionEnvelope struct {
	Code    json.RawMessage      `json:"code"`
	Msg     string               `json:"msg"`
	Message string               `json:"message"`
	Error   *restrictionEnvelope `json:"error"`
}

func (e *restrictionEnvelope) hasCode() bool {
	return e != nil && (string(e.Code) == "11140" || string(e.Code) == `"11140"`)
}

func (e *restrictionEnvelope) usesCode() bool {
	return e != nil && (e.hasCode() || e.Error.usesCode())
}

func (e *restrictionEnvelope) restricted() bool {
	if e == nil {
		return false
	}
	if e.hasCode() {
		text := strings.ToLower(e.Msg + "\n" + e.Message)
		for _, marker := range []string{"账号存在异常行为", "unusual activity", "request illegal"} {
			if strings.Contains(text, marker) {
				return true
			}
		}
	}
	return e.Error.restricted()
}

func parseRestrictionEnvelope(raw []byte) *restrictionEnvelope {
	if !strings.Contains(string(raw), "11140") {
		return nil
	}
	var envelope restrictionEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	return &envelope
}

func isAccountRestrictionPayload(raw []byte) bool {
	return parseRestrictionEnvelope(raw).restricted()
}

// markAccountRestricted 只停止这个账号的调度，保留凭据供用户处理。
// 不做删除前探测、不继续换号，也不暂停账号池或其他站点。
func markAccountRestricted(r *http.Request, acc *Account, reqID uint64, model string) {
	accountMu.Lock()
	acc.Disabled = true
	acc.DisabledReason = errUpstreamAccountRestricted.Error()
	acc.CooldownUntil = time.Time{}
	acc.CooldownMsg = ""
	marker := disabledMarker{
		Path: acc.Path, Reason: acc.DisabledReason, DisabledAt: time.Now().Unix(),
		Nickname: acc.Nickname, UID: acc.UID, Edition: acc.Edition,
	}
	if acc.Auth != nil {
		marker.Nickname = acc.Auth.Account.Nickname
		marker.UID = acc.Auth.Account.UID
		marker.Edition = profileForEdition(acc.Auth.Edition).Key
	}
	accountMu.Unlock()
	if raw, err := json.MarshalIndent(marker, "", "  "); err == nil {
		if err := os.WriteFile(markerPath(marker.Path), raw, 0o600); err != nil {
			log.Printf("[账号风控] traceId=%s requestId=%d 阶段=保存失效标记 结果=失败 错误=%v 业务影响=内存仍停止调度该账号，凭据保留",
				debugTraceID(r), reqID, err)
		}
	}
	recordModelFailure(model, "account_restricted")
	debugEvent(r, "warn", "account_restricted", map[string]any{
		"account_file": filepath.Base(marker.Path), "business_code": 11140,
		"business_impact": "仅停止当前账号调度和本请求换号，保留凭据，不暂停其他账号或站点",
	})
	log.Printf("[账号风控] traceId=%s requestId=%d 账号=%s 业务码=11140 结果=停止本请求换号并保留凭据 业务影响=仅当前账号停止调度，其他账号和站点不变",
		debugTraceID(r), reqID, filepath.Base(marker.Path))
	writeStatusSnapshot()
}

func accountRestrictionError() map[string]any {
	return map[string]any{
		"type": "permission_error", "code": "account_restricted",
		"message": errUpstreamAccountRestricted.Error(),
	}
}
