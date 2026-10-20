package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveCredentialPathRejectsEscapes(t *testing.T) {
	chdirTemp(t)
	path, err := resolveCredentialPath("workbuddy4.json")
	if err != nil {
		t.Fatalf("合法文件名被拒绝: %v", err)
	}
	if filepath.Base(path) != "workbuddy4.json" || !filepath.IsAbs(path) {
		t.Fatalf("应解析成运行目录下的绝对路径: %s", path)
	}
	for _, bad := range []string{"", "  ", "other.json", "../workbuddy1.json", "sub/workbuddy1.json",
		`sub\workbuddy1.json`, "workbuddy1.json/../workbuddy2.json", "workbuddy*.json"} {
		if _, err := resolveCredentialPath(bad); err == nil {
			t.Fatalf("应拒绝越界或非法的凭据名: %q", bad)
		}
	}
}

func TestSuggestCredentialFileNameSkipsUsed(t *testing.T) {
	chdirTemp(t)
	for _, name := range []string{"workbuddy1.json", "workbuddy3.json", "workbuddy3.json.disabled", "other.json"} {
		if err := os.WriteFile(name, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := suggestCredentialFileName(); got != "workbuddy2.json" {
		t.Fatalf("应跳过已占用的 1 与 3，实际 %s", got)
	}
}

func TestDeleteAccountCredentialGuardsAndRemoval(t *testing.T) {
	isolatedGatewayConfig(t)
	chdirTemp(t)
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "workbuddy7.json")
	if err := os.WriteFile(target, []byte(`{"auth":{"accessToken":"synthetic-not-a-token"},"account":{"uid":"u"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	old := accounts
	accounts = []*Account{{Path: target, Auth: &StoredAuth{}}}
	accountMu.Unlock()
	t.Cleanup(func() { accountMu.Lock(); accounts = old; accountMu.Unlock() })

	// 目录不能被当成凭据删除。
	if err := os.Mkdir(filepath.Join(dir, "workbuddy9.json"), 0700); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	accounts = append(accounts, &Account{Path: filepath.Join(dir, "workbuddy9.json"), Auth: &StoredAuth{}})
	accountMu.Unlock()
	if _, err := deleteAccountCredential("workbuddy9.json"); err == nil {
		t.Fatal("目录必须被拒绝，不能递归删除")
	}

	for _, bad := range []string{"", "../x.json", "other.json", "missing.json"} {
		if _, err := deleteAccountCredential(bad); err == nil {
			t.Fatalf("应拒绝删除: %q", bad)
		}
	}
	if _, err := deleteAccountCredential("workbuddy7.json"); err != nil {
		t.Fatalf("正常删除失败: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("凭据文件没有被删除")
	}
}

// 授权失效的账号会以 Disabled 条目留在账号池中（凭据文件可能已被删除，
// 只剩失效标记用于提示重新登录）。用户从网页显式删除时必须连账号条目一起移除，
// 否则网页和 CLI 都会一直显示已经删掉的 workbuddyN.json。
func TestDeleteAccountCredentialRemovesDisabledPhantom(t *testing.T) {
	isolatedGatewayConfig(t)
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "workbuddy9.json")
	marker := disabledMarker{
		Path: target, Reason: "上游鉴权失败 (HTTP 403)", Nickname: "synthetic-user",
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(target), raw, 0600); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	old := accounts
	accounts = []*Account{{Path: target, Disabled: true, DisabledReason: marker.Reason, Nickname: marker.Nickname}}
	accountMu.Unlock()
	t.Cleanup(func() { accountMu.Lock(); accounts = old; accountMu.Unlock() })
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}

	if _, err := deleteAccountCredential("workbuddy9.json"); err != nil {
		t.Fatalf("删除失效账号失败: %v", err)
	}
	accountMu.Lock()
	remaining := len(accounts)
	accountMu.Unlock()
	if remaining != 0 {
		t.Fatalf("失效账号应从账号池移除，剩余 %d 个", remaining)
	}
	if _, err := os.Stat(markerPath(target)); !os.IsNotExist(err) {
		t.Fatal("失效标记应被清除")
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, acc := range snap.Accounts {
		if filepath.Base(acc.Path) == "workbuddy9.json" {
			t.Fatal("状态快照仍包含已删除的账号，网页会继续显示它")
		}
	}
}

func TestAccountAddEndpointValidatesInputWithoutUpstream(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-account-admin"}); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t)
	handler := newWebUIHandler()
	if got := adminRequest(handler, "GET", "/admin/api/account-add", "", ""); got.Code != 401 {
		t.Fatalf("未鉴权必须拒绝: %d", got.Code)
	}
	got := adminRequest(handler, "GET", "/admin/api/account-add", "synthetic-account-admin", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), "defaultFile") {
		t.Fatalf("读取默认名失败: %d %s", got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), "/") {
		t.Fatal("默认名不能回传目录信息")
	}
	for _, body := range []string{`{"site":"mars","file":"workbuddy1.json"}`,
		`{"site":"cn","file":"../workbuddy1.json"}`, `{"site":"cn","file":"other.json"}`} {
		got = adminRequest(handler, "POST", "/admin/api/account-add", "synthetic-account-admin", body)
		if got.Code != 400 {
			t.Fatalf("非法参数必须 400: %s -> %d %s", body, got.Code, got.Body.String())
		}
	}
	if err := os.WriteFile("workbuddy1.json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got = adminRequest(handler, "POST", "/admin/api/account-add", "synthetic-account-admin",
		`{"site":"cn","file":"workbuddy1.json"}`)
	if got.Code != 409 {
		t.Fatalf("同名凭据必须拒绝覆盖: %d %s", got.Code, got.Body.String())
	}
	got = adminRequest(handler, "POST", "/admin/api/account-delete", "synthetic-account-admin", `{"file":"../x.json"}`)
	if got.Code != 400 {
		t.Fatalf("删除越界路径必须 400: %d", got.Code)
	}
	got = adminRequest(handler, "DELETE", "/admin/api/account-delete", "synthetic-account-admin", "")
	// 管理写操作被同源 JSON POST 策略保护，DELETE 必须在进入业务前就被挡下。
	if got.Code == 200 {
		t.Fatalf("删除接口不应接受 DELETE: %d %s", got.Code, got.Body.String())
	}
}

func TestLoginSessionSnapshotReportsRemainingTime(t *testing.T) {
	s := &loginSession{
		ID: "login-snapshot-test", Site: "cn", File: "workbuddy1.json",
		AuthURL: "https://example.test/login", status: "pending",
		deadline: time.Now().Add(90 * time.Second),
	}
	snap := s.snapshot()
	if snap["status"] != "pending" {
		t.Fatalf("初始状态应为 pending: %#v", snap)
	}
	remaining, ok := snap["remainingSeconds"].(int)
	if !ok || remaining < 85 || remaining > 90 {
		t.Fatalf("剩余时间异常: %#v", snap["remainingSeconds"])
	}

	s.finish("expired", "登录已超时，请重新获取登录接口", nil, "")
	snap = s.snapshot()
	if snap["status"] != "expired" {
		t.Fatalf("结束状态应为 expired: %#v", snap)
	}
	if got := snap["remainingSeconds"]; got != 0 {
		t.Fatalf("结束会话不应再显示剩余时间: %#v", got)
	}
}

func TestFinishedLoginSessionRetainedForWebPolling(t *testing.T) {
	oldRetention := loginSessionRetention
	loginSessionRetention = 40 * time.Millisecond
	t.Cleanup(func() { loginSessionRetention = oldRetention })

	s := &loginSession{
		ID: "login-retention-test", Site: "cn", File: "workbuddy1.json",
		status: "pending", deadline: time.Now().Add(-time.Second),
	}
	loginSessionsMu.Lock()
	loginSessions[s.ID] = s
	loginSessionsMu.Unlock()
	t.Cleanup(func() { forgetLoginSession(s.ID) })

	s.finish("expired", "登录已超时，请重新获取登录接口", nil, "")
	scheduleForgetLoginSession(s.ID)
	if findLoginSession(s.ID) == nil {
		t.Fatal("结束后的会话应立即仍可被网页轮询到终态")
	}

	time.Sleep(150 * time.Millisecond)
	if findLoginSession(s.ID) != nil {
		t.Fatal("超过保留时间后会话应被清理")
	}
}

func TestAccountAddEndpointReturnsExpiredStatus(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{AdminKey: "synthetic-account-admin"}); err != nil {
		t.Fatal(err)
	}
	s := &loginSession{
		ID: "login-expired-endpoint", Site: "cn", File: "workbuddy1.json",
		status: "expired", deadline: time.Now().Add(-time.Minute),
	}
	loginSessionsMu.Lock()
	loginSessions[s.ID] = s
	loginSessionsMu.Unlock()
	t.Cleanup(func() { forgetLoginSession(s.ID) })

	handler := newWebUIHandler()
	got := adminRequest(handler, "GET", "/admin/api/account-add?id="+s.ID, "synthetic-account-admin", "")
	if got.Code != 200 {
		t.Fatalf("查询已结束会话应返回 200: %d %s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"status":"expired"`) {
		t.Fatalf("网页轮询应能读到 expired 终态: %s", got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"remainingSeconds":0`) {
		t.Fatalf("已结束会话不应返回剩余时间: %s", got.Body.String())
	}
}
