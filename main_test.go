package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// 验证 429 消息中的重置时间解析
func TestParseResetTime(t *testing.T) {
	msg := `{"code":429,"message":"upstream 429: {\"code\":6004,\"msg\":\"您的使用量已超出频率限制，将在 2026-09-04 07:48:15 UTC+8 重置，您也可以切换其他模型继续使用。\",\"requestId\":\"149e3299-ddf3-4ad1-aaa8-cc752c37630a\"}","type":"upstream_error"}`
	got, ok := parseResetTime(msg)
	if !ok {
		t.Fatal("parseResetTime should succeed")
	}
	want := time.Date(2026, 9, 4, 7, 48, 15, 0, time.FixedZone("UTC+8", 8*3600))
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	t.Logf("parsed reset time: %v", got)
}

// 验证无法解析时返回 false
func TestParseResetTimeInvalid(t *testing.T) {
	if _, ok := parseResetTime("some random error"); ok {
		t.Fatal("should not parse random string")
	}
}

// 验证限流识别（429 状态码 / code 6004 / 频率限制关键词）
func TestIsRateLimited(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, "{}", true},
		{400, `{"code":6004,"msg":"x"}`, true},
		{400, "频率限制", true},
		{400, "frequency limit", true},
		{400, "some other error", false},
		{500, "{}", false},
	}
	for _, c := range cases {
		if got := isRateLimited(c.status, c.body); got != c.want {
			t.Errorf("isRateLimited(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// 验证轮询选择：两个账号交替返回
func TestNextAccountRoundRobin(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a1, _ := nextAccount()
	a2, _ := nextAccount()
	a3, _ := nextAccount()
	if a1.Path != "a.json" || a2.Path != "b.json" || a3.Path != "a.json" {
		t.Fatalf("round robin failed: %s %s %s", a1.Path, a2.Path, a3.Path)
	}
	t.Logf("round-robin order: %s %s %s", a1.Path, a2.Path, a3.Path)
}

// 验证冷却屏蔽：冷却中的账号被跳过，由另一账号代偿
func TestNextAccountSkipsCooldown(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "b.json" {
		t.Fatalf("expected b.json (only non-cooldown), got %s", a.Path)
	}
	t.Logf("cooldown skip works, selected %s", a.Path)
}

// 验证全部冷却时返回错误
func TestNextAccountAllCooldown(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
		{Path: "b.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(2 * time.Hour)},
	}
	rrIndex = 0
	accountMu.Unlock()

	_, err := nextAccount()
	if err == nil {
		t.Fatal("expected error when all accounts in cooldown")
	}
	t.Logf("all-cooldown error: %v", err)
}

// 验证冷却到期后自动恢复
func TestNextAccountCooldownExpiry(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(-time.Minute)},
		{Path: "b.json", Auth: &StoredAuth{}, CooldownUntil: time.Now().Add(time.Hour)},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "a.json" {
		t.Fatalf("expected a.json (cooldown expired), got %s", a.Path)
	}
	t.Logf("expired cooldown recovers, selected %s", a.Path)
}

// 验证授权失效识别（401/403 / invalid token / 登录过期等）
func TestIsAuthFailure(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, "{}", true},
		{403, "{}", true},
		{400, `{"msg":"invalid token"}`, true},
		{400, "unauthorized", true},
		{400, "登录已过期", true},
		{400, "登录失效，请重新登录", true},
		{400, "some other error", false},
		{429, "频率限制", false},
		{500, "{}", false},
	}
	for _, c := range cases {
		if got := isAuthFailure(c.status, c.body); got != c.want {
			t.Errorf("isAuthFailure(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// 验证轮询跳过已失效（Disabled）账号
func TestNextAccountSkipsDisabled(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "revoked"},
		{Path: "b.json", Auth: &StoredAuth{}},
	}
	rrIndex = 0
	accountMu.Unlock()

	a, _ := nextAccount()
	if a.Path != "b.json" {
		t.Fatalf("expected b.json (only non-disabled), got %s", a.Path)
	}
	t.Logf("disabled skip works, selected %s", a.Path)
}

// 验证全部失效时返回错误并提示重新登录
func TestNextAccountAllDisabled(t *testing.T) {
	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "expired"},
		{Path: "b.json", Auth: &StoredAuth{}, Disabled: true, DisabledReason: "revoked"},
	}
	rrIndex = 0
	accountMu.Unlock()

	_, err := nextAccount()
	if err == nil {
		t.Fatal("expected error when all accounts disabled")
	}
	t.Logf("all-disabled error: %v", err)
}

// 验证 disableAccount：标记失效、删除凭据文件、写入失效标记文件
func TestDisableAccount(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"
	// 写一个假的凭据文件
	if err := os.WriteFile(authPath, []byte(`{"auth":{"accessToken":"x"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	acc := &Account{Path: authPath, Auth: &StoredAuth{
		Auth: StoredTokens{AccessToken: "x"},
		Account: StoredAccount{Nickname: "tester", UID: "uid-1"},
	}}

	disableAccount(acc, "令牌刷新失败 (HTTP 401): invalid token")

	accountMu.Lock()
	disabled := acc.Disabled
	reason := acc.DisabledReason
	accountMu.Unlock()
	if !disabled {
		t.Fatal("account should be marked disabled")
	}
	if reason == "" {
		t.Fatal("disabled reason should be recorded")
	}
	// 凭据文件应被删除
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("credential file should be deleted, stat err=%v", err)
	}
	// 失效标记文件应存在
	if _, err := os.Stat(markerPath(authPath)); err != nil {
		t.Fatalf("marker file should exist: %v", err)
	}
	t.Logf("disableAccount works: disabled=%v reason=%q", disabled, reason)
}

// 验证失效标记文件可被恢复为失效账号（Auth 为 nil），并提示重新登录
func TestLoadDisabledMarkers(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"

	// 构造失效标记文件
	marker := disabledMarker{
		Path:       authPath,
		Reason:     "授权失效（令牌刷新失败 HTTP 401）",
		DisabledAt: time.Now().Unix(),
		Nickname:   "tester",
		UID:        "uid-1",
	}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(authPath), data, 0600); err != nil {
		t.Fatal(err)
	}

	// 模拟 -auth 显式指定该路径
	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = authPath
	cfg.AuthDir = ""
	cfg.AuthExplicit = true

	accounts = nil
	loadDisabledMarkers()

	if len(accounts) != 1 {
		t.Fatalf("expected 1 disabled account, got %d", len(accounts))
	}
	acc := accounts[0]
	if !acc.Disabled || acc.Auth != nil {
		t.Fatalf("expected disabled account with nil Auth, got disabled=%v authNil=%v", acc.Disabled, acc.Auth == nil)
	}
	if acc.Nickname != "tester" || acc.UID != "uid-1" {
		t.Fatalf("marker nickname/uid not restored: %+v", acc)
	}
	t.Logf("loadDisabledMarkers restores: %s (nickname=%s)", acc.Path, acc.Nickname)
}

// 验证登录成功后清除失效标记
func TestClearDisabledMarker(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/workbuddy-test.json"
	marker := disabledMarker{Path: authPath, Reason: "revoked"}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(authPath), data, 0600); err != nil {
		t.Fatal(err)
	}

	clearDisabledMarker(authPath)
	if _, err := os.Stat(markerPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("marker file should be removed, stat err=%v", err)
	}
	t.Log("clearDisabledMarker works")
}

// 验证自动发现模式：未指定 -auth/-auth-dir 时，扫描当前目录下所有 workbuddy*.json
func TestCollectConfiguredAuthPathsAutoDiscover(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 模拟目录内多个凭据文件 + 非凭据文件 + 失效标记
	for _, name := range []string{"workbuddy.json", "workbuddy2.json", "workbuddy-3.json", "other.json", "workbuddy.json.disabled"} {
		if err := os.WriteFile(name, []byte(`{"auth":{"accessToken":"x"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = ""
	cfg.AuthExplicit = false

	paths := collectConfiguredAuthPaths()
	if len(paths) != 3 {
		t.Fatalf("expected 3 workbuddy json files, got %d: %v", len(paths), paths)
	}
	want := []string{"workbuddy-3.json", "workbuddy.json", "workbuddy2.json"} // sort.Strings 排序结果
	for i, p := range want {
		if paths[i] != p {
			t.Fatalf("paths[%d] = %s, want %s (all: %v)", i, paths[i], p, paths)
		}
	}
	t.Logf("auto-discover paths: %v", paths)
}

// 验证自动发现模式：目录内没有任何凭据文件时回退到默认路径
func TestCollectConfiguredAuthPathsAutoDiscoverFallback(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 目录为空

	oldAuthFile := cfg.AuthFile
	oldAuthDir := cfg.AuthDir
	oldAuthExplicit := cfg.AuthExplicit
	defer func() {
		cfg.AuthFile = oldAuthFile
		cfg.AuthDir = oldAuthDir
		cfg.AuthExplicit = oldAuthExplicit
	}()
	cfg.AuthFile = "workbuddy.json"
	cfg.AuthDir = ""
	cfg.AuthExplicit = false

	paths := collectConfiguredAuthPaths()
	if len(paths) != 1 || paths[0] != "workbuddy.json" {
		t.Fatalf("expected fallback to default workbuddy.json, got %v", paths)
	}
	t.Logf("fallback path: %v", paths)
}

// 验证 tailLines 读取文件末尾 N 行
func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/test.log"
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(f, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "line4" || lines[1] != "line5" {
		t.Fatalf("tailLines got %v", lines)
	}
	t.Logf("tailLines(2) = %v", lines)
}

// 验证状态快照写入：含 active / cooldown / disabled 三种状态
func TestWriteStatusSnapshot(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{
			Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: StoredAccount{Nickname: "alice", UID: "u1"},
		}},
		{Path: "b.json", Auth: &StoredAuth{
			Auth:    StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: StoredAccount{Nickname: "bob", UID: "u2"},
		}, CooldownUntil: time.Now().Add(30 * time.Minute), CooldownMsg: "频率限制"},
		{Path: "c.json", Disabled: true, DisabledReason: "revoked", Nickname: "carol", UID: "u3"},
	}
	accountMu.Unlock()

	writeStatusSnapshot()

	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 3 {
		t.Fatalf("expected 3 accounts in snapshot, got %d", len(snap.Accounts))
	}
	states := map[string]bool{}
	for _, a := range snap.Accounts {
		states[a.State] = true
	}
	if !states["active"] || !states["cooldown"] || !states["disabled"] {
		t.Fatalf("expected all three states in snapshot, got %v", states)
	}
	t.Logf("snapshot states: %v", states)
}
