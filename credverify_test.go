package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestCredentialIdentityExtractsAccountAndRealm(t *testing.T) {
	token := makeTestJWT(t, map[string]any{
		"sub": "user-abc",
		"iss": "https://www.codebuddy.cn/auth/realms/copilot",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	id, realm := credentialIdentity(StoredTokens{AccessToken: token})
	if id != "user-abc" || realm != "copilot" {
		t.Fatalf("id=%q realm=%q", id, realm)
	}
	if id, realm := credentialIdentity(StoredTokens{AccessToken: "opaque"}); id != "" || realm != "" {
		t.Fatalf("不可解析的令牌应返回空: %q %q", id, realm)
	}
	if shortID("user-abc") == shortID("user-abd") {
		t.Fatal("短标识应能区分不同账号")
	}
	if strings.Contains(shortID("user-abc"), "user-abc") {
		t.Fatal("短标识不应包含原文")
	}
}

// credTestServer 同时提供刷新接口与只读校验接口。
func credTestServer(t *testing.T, refreshToken, refreshSub string, verifyStatus int, verifyBody string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{
				"accessToken":  makeTestJWT(t, map[string]any{"sub": refreshSub, "iss": "https://www.codebuddy.cn/auth/realms/copilot", "exp": time.Now().Add(time.Hour).Unix()}),
				"refreshToken": refreshToken,
				"expiresIn":    3600,
			}})
			_, _ = w.Write(body)
		case "/billing/meter/get-user-resource-summary":
			w.WriteHeader(verifyStatus)
			_, _ = w.Write([]byte(verifyBody))
		default:
			t.Errorf("非预期路径: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	oldBase, oldOrigin, oldClient := profileCN.Base, profileCN.Origin, cfg.HttpClient
	profileCN.Base, profileCN.Origin, cfg.HttpClient = server.URL, server.URL, server.Client()
	t.Cleanup(func() {
		profileCN.Base, profileCN.Origin, cfg.HttpClient = oldBase, oldOrigin, oldClient
	})
}

func newVerifiedTestAccount(t *testing.T, sub string) *Account {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-test.json")
	sa := &StoredAuth{Edition: "cn", Auth: StoredTokens{
		AccessToken:  makeTestJWT(t, map[string]any{"sub": sub, "iss": "https://www.codebuddy.cn/auth/realms/copilot", "exp": time.Now().Add(time.Hour).Unix()}),
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}}
	if err := saveAuthTo(path, sa); err != nil {
		t.Fatal(err)
	}
	return &Account{Path: path, Auth: sa, Edition: "cn"}
}

func TestRefreshCommitsOnlyVerifiedCredential(t *testing.T) {
	for _, tc := range []struct {
		name          string
		refreshSub    string
		verifyStatus  int
		verifyBody    string
		wantCommitted bool
	}{
		{"账号一致且校验通过", "user-A", 200, `{"code":0,"data":{"Packages":[]}}`, true},
		{"账号标识不一致", "user-B", 200, `{"code":0,"data":{"Packages":[]}}`, false},
		{"新凭据校验被拒", "user-A", 401, `{"code":12153,"msg":"invalid_grant"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			credTestServer(t, "new-refresh", tc.refreshSub, tc.verifyStatus, tc.verifyBody)
			acc := newVerifiedTestAccount(t, "user-A")
			oldAccess := acc.Auth.Auth.AccessToken

			err := refreshAccountToken(acc, "单元测试")

			var onDisk StoredAuth
			raw, readErr := os.ReadFile(acc.Path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err := json.Unmarshal(raw, &onDisk); err != nil {
				t.Fatal(err)
			}
			if tc.wantCommitted {
				if err != nil {
					t.Fatalf("应提交新凭据: %v", err)
				}
				if onDisk.Auth.RefreshToken != "new-refresh" {
					t.Fatalf("磁盘未写入新刷新令牌: %+v", onDisk.Auth)
				}
				if acc.RefreshFailCount != 0 {
					t.Fatal("成功提交后应清零失败计数")
				}
			} else {
				if err == nil {
					t.Fatal("应拒绝提交")
				}
				if onDisk.Auth.RefreshToken != "old-refresh" || onDisk.Auth.AccessToken != oldAccess {
					t.Fatalf("旧凭据应原样保留: %+v", onDisk.Auth)
				}
				if acc.Auth.Auth.RefreshToken != "old-refresh" {
					t.Fatal("内存中的旧凭据也应保留")
				}
				// 校验失败不应累计到「判定失效」的计数里，否则会把还能用的账号停掉。
				if acc.RefreshFailCount != 0 {
					t.Fatalf("覆盖前校验失败不应计入失效计数: %d", acc.RefreshFailCount)
				}
				if !strings.Contains(acc.LastRefreshError, "覆盖前校验") {
					t.Fatalf("应记录告警: %q", acc.LastRefreshError)
				}
			}
		})
	}
}

// 覆盖前校验必须在日志里可见，否则生产上无法确认它真的执行过。
func TestOverwriteVerificationIsObservable(t *testing.T) {
	chdirTemp(t)
	credTestServer(t, "new-refresh", "user-A", 200, `{"code":0,"data":{"Packages":[]}}`)
	acc := newVerifiedTestAccount(t, "user-A")
	var buf bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldWriter)

	if err := refreshAccountToken(acc, "单元测试"); err != nil {
		t.Fatal(err)
	}
	log.SetOutput(oldWriter)
	out := buf.String()
	for _, want := range []string{"阶段=覆盖前校验", "结果=通过", "与旧凭据一致", "允许覆盖旧凭据"} {
		if !strings.Contains(out, want) {
			t.Fatalf("覆盖前校验缺少可观测日志 %q:\n%s", want, out)
		}
	}
	// 拒绝覆盖时也必须留下原因。
	credTestServer(t, "new-refresh", "user-B", 200, `{"code":0,"data":{"Packages":[]}}`)
	acc2 := newVerifiedTestAccount(t, "user-A")
	buf.Reset()
	log.SetOutput(&buf)
	if err := refreshAccountToken(acc2, "单元测试"); err == nil {
		t.Fatal("账号不一致时应拒绝覆盖")
	}
	log.SetOutput(oldWriter)
	if !strings.Contains(buf.String(), "账号标识与旧凭据不一致") {
		t.Fatalf("拒绝覆盖应说明原因:\n%s", buf.String())
	}
}

func TestVerifyAccountUsableDistinguishesDeadFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantUsable bool
		wantErr    bool
	}{
		{"可用", 200, `{"code":0,"data":{}}`, true, false},
		{"明确失效401", 401, `{"code":12153,"msg":"invalid_grant"}`, false, false},
		{"明确失效403", 403, `{"code":10002,"msg":"forbidden"}`, false, false},
		{"无法判定500", 500, `{"code":500,"msg":"oops"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			oldOrigin, oldClient := profileCN.Origin, cfg.HttpClient
			profileCN.Origin, cfg.HttpClient = server.URL, server.Client()
			defer func() { profileCN.Origin, cfg.HttpClient = oldOrigin, oldClient }()

			usable, err := verifyAccountUsable(t.Context(), StoredAuth{Auth: StoredTokens{AccessToken: "token"}})
			if usable != tc.wantUsable || (err != nil) != tc.wantErr {
				t.Fatalf("usable=%v err=%v want usable=%v err=%v", usable, err, tc.wantUsable, tc.wantErr)
			}
		})
	}
	// 缺少令牌时不得发起请求，也不得判定为可用。
	if usable, err := verifyAccountUsable(t.Context(), StoredAuth{}); usable || err == nil {
		t.Fatal("缺少访问令牌应返回不可用且带原因")
	}
}

func TestDisableAccountKeepsOrDeletesBasedOnVerification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantDeleted bool
	}{
		{"凭据仍可用则保留", 200, `{"code":0,"data":{}}`, false},
		{"无法判定则保守保留", 500, `{"code":500,"msg":"oops"}`, false},
		{"确认失效才删除", 401, `{"code":12153,"msg":"invalid_grant"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer still-valid" {
					t.Error("缺少鉴权头")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			oldOrigin, oldClient := profileCN.Origin, cfg.HttpClient
			profileCN.Origin, cfg.HttpClient = server.URL, server.Client()
			defer func() { profileCN.Origin, cfg.HttpClient = oldOrigin, oldClient }()

			authPath := filepath.Join(t.TempDir(), "workbuddy-test.json")
			if err := os.WriteFile(authPath, []byte(`{"auth":{"accessToken":"still-valid"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			acc := &Account{Path: authPath, Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "still-valid"}}}

			disableAccount(acc, "测试")

			if !acc.Disabled {
				t.Fatal("无论是否删除都应停止调度")
			}
			_, statErr := os.Stat(authPath)
			if deleted := os.IsNotExist(statErr); deleted != tc.wantDeleted {
				t.Fatalf("deleted=%v want=%v", deleted, tc.wantDeleted)
			}
			if _, err := os.Stat(markerPath(authPath)); err != nil {
				t.Fatalf("失效标记应始终写入: %v", err)
			}
		})
	}
}

// 凭据文件仍在但存在失效标记时，重启后账号必须保持失效，不能被重新调度。
func TestDisabledMarkerWinsWhenCredentialFileRemains(t *testing.T) {
	chdirTemp(t)
	// 用工作目录内的相对路径，才能走到「自动发现」这条真实扫描路径。
	path := "workbuddy1.json"
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"kept"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// 标记必须比凭据文件新，才代表「凭据之后没有被重新登录覆盖」。
	marker := disabledMarker{Path: path, Reason: "测试保留", DisabledAt: time.Now().Unix()}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(path), data, 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(markerPath(path), later, later); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: path, Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "kept"}}}}
	loadDisabledMarkers()
	got := accounts[0]
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})

	if !got.Disabled {
		t.Fatal("存在失效标记时，凭据文件仍在也必须保持失效")
	}
	if got.DisabledReason != "测试保留" {
		t.Fatalf("失效原因未恢复: %q", got.DisabledReason)
	}
}

// 凭据文件比标记新，说明用户已重新登录或手动续期，旧标记应自动清除。
func TestStaleDisabledMarkerClearedWhenCredentialIsNewer(t *testing.T) {
	chdirTemp(t)
	path := "workbuddy2.json"
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"renewed"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	marker := disabledMarker{Path: path, Reason: "旧标记", DisabledAt: time.Now().Add(-time.Hour).Unix()}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(path), data, 0600); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(markerPath(path), older, older); err != nil {
		t.Fatal(err)
	}

	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: path, Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "renewed"}}}}
	loadDisabledMarkers()
	got := accounts[0]
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})

	if got.Disabled {
		t.Fatal("凭据文件更新后不应再被判为失效")
	}
	if _, err := os.Stat(markerPath(path)); !os.IsNotExist(err) {
		t.Fatalf("过期标记应被清除: %v", err)
	}
}
