package main

// logredact_bench_test.go —— 脱敏与统计落盘的性能基准。
// 场景取自生产 pprof 实测：多账号词表 ~25 条秘密、stats.json ~21MB（70k 条记录）。

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// benchRedactSetup 注册 25 条秘密（模拟 5 账号 × 2 token + 5 个 key + 转义变体）。
func benchRedactSetup(b *testing.B, count int) map[string]struct{} {
	b.Helper()
	old := map[string]struct{}{}
	sensitiveSecrets.Range(func(key, _ any) bool {
		old[key.(string)] = struct{}{}
		sensitiveSecrets.Delete(key)
		return true
	})
	redactionCachePtr.Store(nil)
	b.Cleanup(func() {
		sensitiveSecrets.Range(func(key, _ any) bool {
			sensitiveSecrets.Delete(key)
			return true
		})
		for key := range old {
			sensitiveSecrets.Store(key, struct{}{})
		}
		redactionCachePtr.Store(nil)
	})
	registered := map[string]struct{}{}
	for i := 0; i < count; i++ {
		v := fmt.Sprintf("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.bench-payload-%03d-QWERTYUIOPASDFGHJKL", i)
		registerSecrets(v)
		registered[v] = struct{}{}
	}
	return registered
}

// BenchmarkRedactSensitiveTextCached：稳态路径（缓存命中，无重建）。
func BenchmarkRedactSensitiveTextCached(b *testing.B) {
	benchRedactSetup(b, 25)
	text := "[模型冷却] 账号 workbuddy.json 模型 deepseek-v4.1-flash 触发频率限制 reason=rate limited traceId=abc-123 eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.bench-payload-007-QWERTYUIOPASDFGHJKL"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = redactSensitiveText(text)
	}
}

// BenchmarkFlushOptionalStatsPrune70k：21MB 规模文档的 flush（含裁剪 + Marshal）。
// 对照组：不裁剪时的 Marshal 成本（更早版本行为：持续增长的文档永远全量序列化）。
func BenchmarkFlushOptionalStatsPrune70k(b *testing.B) {
	dir := b.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(dir)
	b.Cleanup(func() { _ = os.Chdir(oldWd) })

	now := time.Now()
	requestStats.Lock()
	requestStats.document = statsDocument{Schema: statsSchema, StartedAt: now.Add(-35 * 24 * time.Hour).UnixMilli(), Accounts: map[string]statsAccount{}, Requests: []statsRequest{}}
	requestStats.enabled, requestStats.blocked = true, false
	requestStats.document.Accounts["acct-bench"] = statsAccount{File: "workbuddy-bench.json"}
	for i := 0; i < 70000; i++ {
		age := 35*24 - i%36*24 // 0~35 天跨度，约一半在窗口外
		requestStats.document.Requests = append(requestStats.document.Requests, statsRequest{
			Account: "acct-bench", At: now.Add(-time.Duration(age) * time.Hour).UnixMilli(), Status: 200, LatencyMS: 120,
		})
	}
	requestStats.revision = 1
	requestStats.saved = 0
	requestStats.Unlock()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		requestStats.Lock()
		requestStats.saved = requestStats.revision - 1 // 强制视为有变更
		requestStats.Unlock()
		if err := flushOptionalStats(); err != nil {
			b.Fatalf("flush 失败: %v", err)
		}
	}
}
