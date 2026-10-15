package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
