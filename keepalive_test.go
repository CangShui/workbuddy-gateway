package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHourListParsing(t *testing.T) {
	for _, tc := range []struct {
		input   string
		want    []int
		wantErr bool
	}{
		{"22", []int{22}, false},
		{"22,10", []int{10, 22}, false},
		{" 3 , 7 ", []int{3, 7}, false},
		{"", nil, false},
		{"0", []int{0}, false},
		{"24", nil, true},
		{"-1", nil, true},
		{"abc", nil, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			h := hourList{22}
			err := h.Set(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if len(h) != len(tc.want) {
				t.Fatalf("got=%v want=%v", []int(h), tc.want)
			}
			for i := range tc.want {
				if h[i] != tc.want[i] {
					t.Fatalf("got=%v want=%v", []int(h), tc.want)
				}
			}
		})
	}
}

func TestNextKeepaliveTimePicksEarliestFutureHour(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 10, 2, 23, 30, 0, 0, loc)
	// 23:30 已过 22:00，下一个是次日 10:00。
	got := nextKeepaliveTime(now, []int{10, 22})
	want := time.Date(2026, 10, 3, 10, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got=%s want=%s", got, want)
	}
	// 未到 22:00 时当天触发。
	now = time.Date(2026, 10, 2, 21, 0, 0, 0, loc)
	if got := nextKeepaliveTime(now, []int{22}); !got.Equal(time.Date(2026, 10, 2, 22, 0, 0, 0, loc)) {
		t.Fatalf("当天应触发: %s", got)
	}
	// 正好等于配置时刻应顺延到次日，避免刚启动就重复触发。
	now = time.Date(2026, 10, 2, 22, 0, 0, 0, loc)
	if got := nextKeepaliveTime(now, []int{22}); !got.Equal(time.Date(2026, 10, 3, 22, 0, 0, 0, loc)) {
		t.Fatalf("整点应顺延: %s", got)
	}
}

func TestClassifyRefreshFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       refreshFailureKind
	}{
		{"上游会话失效码", `{"code":12153,"msg":"Offline user session not found"}`, 401, refreshFailureSessionDead},
		{"invalid_grant", `{"code":12153,"msg":"invalid_grant: Token is not active"}`, 401, refreshFailureSessionDead},
		{"普通401", `{"code":10001,"msg":"unauthorized"}`, 401, refreshFailureTransient},
		{"权限拒绝403", `{"code":10002,"msg":"permission denied"}`, 403, refreshFailureTransient},
		{"服务端5xx", `{"code":500,"msg":"internal error"}`, 500, refreshFailureTransient},
		{"网络错误无响应体", "", 0, refreshFailureTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRefreshFailure(tc.status, tc.body); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestJWTExpiryAndRefreshTokenExpiryFallback(t *testing.T) {
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa(int(exp.Unix())) + `,"typ":"Offline"}`))
	token := "header." + payload + ".signature"
	got, ok := jwtExpiry(token)
	if !ok || !got.Equal(exp) {
		t.Fatalf("jwtExpiry got=%v ok=%v want=%v", got, ok, exp)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b", "header." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".sig"} {
		if _, ok := jwtExpiry(bad); ok {
			t.Fatalf("应解析失败: %q", bad)
		}
	}

	// 优先使用续期接口记录的时间。
	recorded := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	if v, source, ok := refreshTokenExpiry(StoredTokens{RefreshToken: token, RefreshExpiresAt: recorded.Unix()}); !ok || !v.Equal(recorded) || source != "续期接口返回" {
		t.Fatalf("应优先使用续期接口时间: %v %s %v", v, source, ok)
	}
	// 没有记录时回退到未验签的令牌声明。
	if v, source, ok := refreshTokenExpiry(StoredTokens{RefreshToken: token}); !ok || !v.Equal(exp) || source != "令牌声明" {
		t.Fatalf("应回退到令牌声明: %v %s %v", v, source, ok)
	}
	// 都没有则未知。
	if _, _, ok := refreshTokenExpiry(StoredTokens{RefreshToken: "opaque"}); ok {
		t.Fatal("不可解析的令牌应视为未知")
	}
}

func TestSanitizeUpstreamTextHidesIdentifiers(t *testing.T) {
	raw := `upstream 401: {"msg":"userID:1191c369-95c2-4f51-9eda-1da71ae17784","token":"Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"}`
	got := sanitizeUpstreamText(raw)
	for _, secret := range []string{"1191c369-95c2-4f51-9eda-1da71ae17784", "eyJhbGciOiJSUzI1NiJ9"} {
		if strings.Contains(got, secret) {
			t.Fatalf("敏感片段未隐去: %s", got)
		}
	}
	if !strings.Contains(got, "已隐去") {
		t.Fatalf("应包含隐去标记: %s", got)
	}
}

// refreshTestServer 起一个假的续期接口，返回可配置的响应体。
func refreshTestServer(t *testing.T, handler func(call int) (int, string)) *httptest.Server {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 覆盖前的只读校验会打到同一个站点，返回「可用」让测试聚焦刷新逻辑本身。
		if r.URL.Path == "/billing/meter/get-user-resource-summary" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[]}}`))
			return
		}
		calls++
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Errorf("非预期路径: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("非预期方法: %s", r.Method)
		}
		if r.Header.Get("X-Refresh-Token") == "" {
			t.Error("缺少 X-Refresh-Token")
		}
		if got := r.Header.Get("X-Auth-Refresh-Source"); got != "workbuddy" {
			t.Errorf("X-Auth-Refresh-Source=%q", got)
		}
		status, body := handler(calls)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	oldBase, oldOrigin, oldClient := profileCN.Base, profileCN.Origin, cfg.HttpClient
	profileCN.Base, profileCN.Origin, cfg.HttpClient = server.URL, server.URL, server.Client()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin, cfg.HttpClient = oldBase, oldOrigin, oldClient
	})
	return server
}

func newTestAccount(t *testing.T, access, refresh string) *Account {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-test.json")
	sa := &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}
	if err := saveAuthTo(path, sa); err != nil {
		t.Fatal(err)
	}
	return &Account{Path: path, Auth: sa, Edition: "cn"}
}

func TestRefreshPersistsRenewalMetadata(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 200, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600,"refreshExpiresIn":7200,"domain":"example.test"}}`
	})
	acc := newTestAccount(t, "old-access", "old-refresh")
	before := time.Now()
	if err := refreshAccountToken(acc, "单元测试"); err != nil {
		t.Fatal(err)
	}
	got := acc.Auth.Auth
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.Domain != "example.test" {
		t.Fatalf("令牌未更新: %+v", got)
	}
	if got.ExpiresAt < before.Add(3500*time.Second).Unix() {
		t.Fatalf("访问令牌过期时间未顺延: %d", got.ExpiresAt)
	}
	if got.RefreshExpiresAt < before.Add(7100*time.Second).Unix() {
		t.Fatalf("刷新令牌到期时间未记录: %d", got.RefreshExpiresAt)
	}
	if got.LastRefreshTime < before.Unix() {
		t.Fatalf("最近续期时间未记录: %d", got.LastRefreshTime)
	}
	if acc.RefreshFailCount != 0 || acc.LastRefreshError != "" {
		t.Fatal("成功续期后应清零失败计数")
	}

	// 元数据必须落盘，重启后仍能看到。
	raw, err := os.ReadFile(acc.Path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk StoredAuth
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Auth.LastRefreshTime == 0 || onDisk.Auth.RefreshExpiresAt == 0 || onDisk.Auth.AccessToken != "new-access" {
		t.Fatalf("元数据未写回磁盘: %+v", onDisk.Auth)
	}
}

func TestRefreshPreservesExpiryWhenUpstreamOmitsExpiresIn(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 200, `{"code":0,"data":{"accessToken":"second-access"}}`
	})
	acc := newTestAccount(t, "first-access", "refresh-token")
	originalExpires := acc.Auth.Auth.ExpiresAt
	if err := refreshAccountToken(acc, "单元测试"); err != nil {
		t.Fatal(err)
	}
	if acc.Auth.Auth.ExpiresAt != originalExpires {
		t.Fatalf("接口未给 expiresIn 时应保留旧过期时间: %d -> %d", originalExpires, acc.Auth.Auth.ExpiresAt)
	}
	if acc.Auth.Auth.AccessToken != "second-access" {
		t.Fatal("访问令牌仍应更新")
	}
	if acc.Auth.Auth.RefreshToken != "refresh-token" {
		t.Fatal("接口未下发新刷新令牌时应保留旧值")
	}
}

func TestRefreshDisablesOnlyAfterConsecutiveFailures(t *testing.T) {
	chdirTemp(t)
	// 普通 401（不含会话失效标志）：属于临时失败，不能一次就删凭据。
	refreshTestServer(t, func(int) (int, string) {
		return 401, `{"code":10001,"msg":"unauthorized"}`
	})
	acc := newTestAccount(t, "access", "refresh")
	for i := 1; i < refreshDisableThreshold; i++ {
		if err := refreshAccountToken(acc, "单元测试"); err == nil {
			t.Fatal("应返回错误")
		}
		if acc.Disabled {
			t.Fatalf("第 %d 次失败就禁用账号，过于激进", i)
		}
		if acc.RefreshFailCount != i {
			t.Fatalf("失败计数=%d want=%d", acc.RefreshFailCount, i)
		}
		if _, err := os.Stat(acc.Path); err != nil {
			t.Fatalf("凭据文件不应被删除: %v", err)
		}
	}
	if err := refreshAccountToken(acc, "单元测试"); err == nil {
		t.Fatal("应返回错误")
	}
	if !acc.Disabled {
		t.Fatalf("连续 %d 次失败后应判定登录态失效", refreshDisableThreshold)
	}
}

func TestSessionDeadSignalKeepsCredentialsUntilThreshold(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 401, `{"code":12153,"msg":"invalid_grant: Token is not active"}`
	})
	acc := newTestAccount(t, "access", "refresh")
	if err := refreshAccountToken(acc, "单元测试"); err == nil {
		t.Fatal("应返回错误")
	}
	if acc.Disabled {
		t.Fatal("首次会话失效只告警，不应立即删除凭据")
	}
	if _, err := os.Stat(acc.Path); err != nil {
		t.Fatalf("凭据文件应保留: %v", err)
	}
	if !strings.Contains(acc.LastRefreshError, "登录态失效") {
		t.Fatalf("应记录判定类型: %s", acc.LastRefreshError)
	}
	for i := 1; i < refreshDisableThreshold; i++ {
		_ = refreshAccountToken(acc, "单元测试")
	}
	if !acc.Disabled {
		t.Fatalf("连续 %d 次会话失效后应停止调度", refreshDisableThreshold)
	}
}

func TestRefreshSuccessResetsFailureCounter(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(call int) (int, string) {
		if call == 1 {
			return 500, `{"code":500,"msg":"temporary"}`
		}
		return 200, `{"code":0,"data":{"accessToken":"ok-access","expiresIn":3600}}`
	})
	acc := newTestAccount(t, "access", "refresh")
	if err := refreshAccountToken(acc, "单元测试"); err == nil {
		t.Fatal("首次应失败")
	}
	if acc.RefreshFailCount != 1 {
		t.Fatalf("失败计数=%d", acc.RefreshFailCount)
	}
	if err := refreshAccountToken(acc, "单元测试"); err != nil {
		t.Fatal(err)
	}
	if acc.RefreshFailCount != 0 || acc.LastRefreshError != "" {
		t.Fatalf("成功后续期计数应清零: %d %q", acc.RefreshFailCount, acc.LastRefreshError)
	}
}

func TestKeepaliveSkipsUnusableAccountsAndRefreshesValid(t *testing.T) {
	chdirTemp(t)
	refreshTestServer(t, func(int) (int, string) {
		return 200, `{"code":0,"data":{"accessToken":"keepalive-access","expiresIn":3600}}`
	})
	valid := newTestAccount(t, "access", "refresh")
	noRefresh := &Account{Path: "no-refresh.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x"}}}
	disabled := &Account{Path: "disabled.json", Disabled: true, Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", RefreshToken: "y"}}}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{valid, noRefresh, disabled}
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})

	runKeepaliveOnce("单元测试", false)

	if valid.Auth.Auth.AccessToken != "keepalive-access" {
		t.Fatal("可用账号应被主动续期")
	}
	if disabled.Disabled != true || disabled.Auth.Auth.AccessToken != "x" {
		t.Fatal("已失效账号不应被刷新")
	}
	if noRefresh.Auth.Auth.AccessToken != "x" {
		t.Fatal("缺少刷新令牌的账号不应被刷新")
	}
}

func TestKeepaliveOverdueDetection(t *testing.T) {
	now := time.Now()
	accountMu.Lock()
	defer accountMu.Unlock()
	for _, tc := range []struct {
		name string
		acc  *Account
		want bool
	}{
		{"从未续期", &Account{Auth: &StoredAuth{}}, true},
		{"刚续期", &Account{Auth: &StoredAuth{Auth: StoredTokens{LastRefreshTime: now.Unix()}}}, false},
		{"超过阈值", &Account{Auth: &StoredAuth{Auth: StoredTokens{LastRefreshTime: now.Add(-keepaliveOverdueAfter - time.Minute).Unix()}}}, true},
		{"无凭据", &Account{}, false},
		{"空账号", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isKeepaliveOverdue(tc.acc, now); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestRenewalStatusTextShowsExpiryAndWarnings(t *testing.T) {
	accountMu.Lock()
	defer accountMu.Unlock()
	soon := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	acc := &Account{
		Auth: &StoredAuth{Auth: StoredTokens{
			RefreshToken:     "opaque",
			RefreshExpiresAt: soon.Unix(),
			LastRefreshTime:  time.Now().Add(-time.Hour).Unix(),
		}},
	}
	text := formatRenewalStatus(acc)
	for _, want := range []string{"刷新令牌", "即将到期", "最近续期"} {
		if !strings.Contains(text, want) {
			t.Fatalf("缺少 %q: %s", want, text)
		}
	}
	// 未知到期时间不能显示成"有效"。
	unknown := formatRenewalStatus(&Account{Auth: &StoredAuth{Auth: StoredTokens{RefreshToken: "opaque"}}})
	if !strings.Contains(unknown, "到期时间未知") || strings.Contains(unknown, "有效") {
		t.Fatalf("未知到期时间处理不当: %s", unknown)
	}
	if got := formatRenewalStatus(nil); got != "" {
		t.Fatalf("空账号应返回空串: %q", got)
	}
}

func TestAccountTableWarnsOnExpiringRefreshToken(t *testing.T) {
	soon := time.Now().Add(48 * time.Hour).Unix()
	expired := time.Now().Add(-time.Hour).Unix()
	out := renderAccountTable([]accountSnapshot{
		{Path: "soon.json", QuotaKnown: true, PlanLabel: "免费", RefreshExpiresAt: soon},
		{Path: "dead.json", QuotaKnown: true, PlanLabel: "免费", RefreshExpiresAt: expired},
		{Path: "failing.json", QuotaKnown: true, PlanLabel: "免费", RefreshFailCount: 2},
	})
	for _, want := range []string{"soon.json 刷新令牌", "建议尽快重新登录", "dead.json 刷新令牌已过期", "failing.json 续期连续失败 2 次"} {
		if !strings.Contains(out, want) {
			t.Fatalf("监控表缺少预警 %q:\n%s", want, out)
		}
	}
	// 长期有效的账号不应产生噪声。
	clean := renderAccountTable([]accountSnapshot{{Path: "ok.json", QuotaKnown: true, PlanLabel: "免费", RefreshExpiresAt: time.Now().Add(200 * 24 * time.Hour).Unix()}})
	if strings.Contains(clean, "重新登录") || strings.Contains(clean, "续期连续失败") {
		t.Fatalf("长期有效账号不应告警:\n%s", clean)
	}
}
