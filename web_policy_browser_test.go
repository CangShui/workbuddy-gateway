package main

import (
	"context"
	"encoding/json"
	"log"
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
	cfg.ModelsRefresh, cfg.ReloadInterval, cfg.KeepaliveHours = 60, 5, []int{22}
	cfg.DisablePriceProbes = true
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
	now := time.Now()
	for _, fixture := range []struct {
		id, status, detail string
		next               time.Time
	}{
		{"quota_scan", "partial", "合成测试：一个账号查询超时", now.Add(5 * time.Minute)},
		{"token_check", "success", "合成测试：两个账号检查正常", now.Add(5 * time.Minute)},
		{"status_snapshot", "success", "合成测试：快照已写入", now.Add(3 * time.Second)},
		{"model_catalog", "success", "合成测试：两站目录已同步", now.Add(time.Hour)},
		{"daily_checkin", "success", "合成测试：签到成功", nextDailyCheckin(now)},
		{"keepalive", "success", "合成测试：续期成功", nextKeepaliveTime(now, cfg.KeepaliveHours)},
		{"keepalive_catchup", "skipped", "合成测试：近期已续期，不补跑", time.Time{}},
	} {
		logScheduledTask(fixture.id, "completed", fixture.status, newTraceID(), now.Add(-time.Minute), fixture.next, fixture.detail)
	}
	logScheduledTask("price_probe", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "合成测试：自动探测关闭")
	// 合成倍率来源日志：用于验证「感叹号」在有日志/未知/无日志三种情况下的展示。
	log.Printf("[Models] 官方倍率 站点=cn 模型=hy3 原始credits=\"x0.29\" 生效倍率=0.29 有倍率=true 促销=\"\" 促销截止=\"\" 促销已过期=false 来源=实时接口 刷新时间=%s",
		now.Format("2006-01-02T15:04:05"))
	log.Printf("[Models] 官方倍率 站点=intl 模型=deepseek-v4.1-flash 原始credits=\"x0.00\" 生效倍率=0.00 有倍率=true 促销=\"Free now\" 促销截止=\"2026-09-08T00:00:00+08:00\" 促销已过期=true 来源=实时接口 刷新时间=%s",
		now.Format("2006-01-02T15:04:05"))
	log.Printf("[ModelPrice] 载入缓存结论 站点=intl 模型=deepseek-v4.1-flash 判定=paid credit=0.05 tokens=385 探测时间=2026-10-02T00:23:40 说明=usage.credit>0 收费 来源=缓存文件（非本轮新探测）")
	log.Printf("[ModelPrice] traceId=%s 阶段=自动探测调度 allowPriceProbes=false legacyDisablePriceProbes=true 结果=已关闭 原因=兼容旧参数-disable-price-probes=true 业务影响=不会自动请求模型", newTraceID())
	log.Printf("[ModelCost] requestId=3472 账号 workbuddy-cn-a.json 站点=cn 模型 hy3 实测 usage.credit=0.29，已学习为收费模型")
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
