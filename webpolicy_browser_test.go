package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 可重复的浏览器验收夹具：仅启用本地管理 HTTP，不登录、不请求上游。
// WORKBUDDY_BROWSER_FIXTURE=1 go test -run TestPolicyBrowserFixture -v
func TestPolicyBrowserFixture(t *testing.T) {
	if os.Getenv("WORKBUDDY_BROWSER_FIXTURE") != "1" {
		t.Skip("仅用于显式浏览器验收")
	}
	isolatePolicies(t)
	cfg.Port, cfg.WebPort = 18317, 18316
	writeRuntimeFixture(t, `{"gateway":{"apiPort":18317,"webPort":18316,"adminKey":"visual-test-admin"},"systemPrompt":{"force":"浏览器验收：此处直接显示 systemPrompt 正文。"},"models":{"allowlist":["hy3"],"accounts":{"all":{"allowlist":["workbuddy-cn-a.json"],"blocklist":[]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	catalogModels = map[string][]catalogModel{"cn": {
		{ID: "hy3", HasMultiplier: true, Multiplier: 1},
		{ID: "deepseek-v4.1-flash", HasMultiplier: true, Multiplier: 0},
		{ID: "claude-sonnet-4.5", HasMultiplier: true, Multiplier: 2},
	}}
	accounts = []*Account{
		{Path: "workbuddy-cn-a.json", Auth: &StoredAuth{Account: StoredAccount{Nickname: "国内测试账号"}, Auth: StoredTokens{AccessToken: "synthetic-not-a-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, Edition: "cn", QuotaKnown: true, QuotaTotal: 2000, QuotaRemaining: 1800},
		{Path: "workbuddy-intl-b.json", Auth: &StoredAuth{Edition: "intl", Account: StoredAccount{Nickname: "国际测试账号"}, Auth: StoredTokens{AccessToken: "synthetic-not-a-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, Edition: "intl", QuotaKnown: true, QuotaTotal: 1000, QuotaRemaining: 500},
	}
	for _, account := range accounts {
		raw, err := json.MarshalIndent(account.Auth, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(account.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldDay := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	_ = os.WriteFile(filepath.Join(logDir, "gateway-"+oldDay+".log"), []byte("synthetic historical log\n"), 0600)
	writeStatusSnapshot()
	listener, err := net.Listen("tcp", "127.0.0.1:18316")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: newWebUIHandler(), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer func() {
		cancel()
		_ = server.Close()
		workers.Wait()
	}()
	workers.Add(2)
	go func() {
		defer workers.Done()
		runtimeConfigLoop(ctx)
	}()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeStatusSnapshot()
			}
		}
	}()
	go server.Serve(listener)
	t.Log("浏览器验收地址 http://127.0.0.1:18316/ui/，合成管理 Key visual-test-admin；夹具不访问上游")
	duration := 180 * time.Second
	if raw := os.Getenv("WORKBUDDY_BROWSER_DURATION"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(duration)
}
