package main

import (
	"sync"
	"testing"
)

// isolateSecrets 只清 sensitiveSecrets 表；本文件还要复位派生缓存与淘汰登记，
// 避免用例间通过全局状态互相污染。
func isolateRedactionState(t *testing.T) {
	t.Helper()
	isolateSecrets(t)
	redactionCachePtr.Store(nil)
	secretRetireMu.Lock()
	oldState := secretRetireState
	secretRetireState = nil
	secretRetireMu.Unlock()
	t.Cleanup(func() {
		redactionCachePtr.Store(nil)
		secretRetireMu.Lock()
		secretRetireState = oldState
		secretRetireMu.Unlock()
	})
}

func secretRegistered(value string) bool {
	_, ok := sensitiveSecrets.Load(value)
	return ok
}

func rotateOnce(t *testing.T, path string, from, to StoredTokens) {
	t.Helper()
	registerSecrets(to.AccessToken, to.RefreshToken) // refreshTokenPayload/saveAuthTo 内完成
	retireStaleCredentialSecrets(path, from, to)
}

// 缓存命中路径：注册后首次调用构建快照，重复调用结果一致（幂等），且修改词表后
// 新值立即生效、旧快照不滞留。
func TestRedactionBundleCacheInvalidation(t *testing.T) {
	isolateRedactionState(t)
	registerSecrets("synthetic-cache-secret-alpha")
	first := redactSensitiveText("x synthetic-cache-secret-alpha y")
	if first != "x [REDACTED] y" {
		t.Fatalf("首次脱敏未命中新注册值: %q", first)
	}
	for i := 0; i < 3; i++ {
		if again := redactSensitiveText("x synthetic-cache-secret-alpha y"); again != first {
			t.Fatalf("缓存命中路径结果漂移: %q != %q", again, first)
		}
	}
	// 新注册必须立即生效：不允许读到旧快照
	registerSecrets("synthetic-cache-secret-beta")
	if got := redactSensitiveText("synthetic-cache-secret-beta"); got != "[REDACTED]" {
		t.Fatalf("新注册值未生效（旧快照滞留）: %q", got)
	}
	// 注销必须立即生效
	unregisterSecrets("synthetic-cache-secret-beta")
	if got := redactSensitiveText("synthetic-cache-secret-beta"); got != "synthetic-cache-secret-beta" {
		t.Fatalf("已注销值仍被替换: %q", got)
	}
}

// 并发注册与读取不得竞态、不得丢脱敏：注册 goroutine 持续写新值的同时读取端
// 反复脱敏，结束后已知秘密必须仍然脱敏。
func TestRedactionBundleConcurrentRegisterAndRedact(t *testing.T) {
	isolateRedactionState(t)
	registerSecrets("synthetic-concurrent-base-secret")
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				registerSecrets("synthetic-concurrent-secret-w")
				_ = redactSensitiveText("payload synthetic-concurrent-base-secret tail")
			}
		}()
	}
	wg.Wait()
	if got := redactSensitiveText("synthetic-concurrent-secret-w"); got != "[REDACTED]" {
		t.Fatalf("并发注册后脱敏缺失: %q", got)
	}
}

// 两代宽限：第一次真实轮换只记录不删除；第二次轮换淘汰已闲置满一个周期的上一代；
// 词表规模随之有界，旧值不再消耗脱敏成本、在用值保护不变。
func TestRetireStaleCredentialSecretsTwoGenerationGrace(t *testing.T) {
	isolateRedactionState(t)
	const path = "workbuddy-retire-test.json"
	generations := []StoredTokens{
		{AccessToken: "retire-access-gen0", RefreshToken: "retire-refresh-gen0"},
		{AccessToken: "retire-access-gen1", RefreshToken: "retire-refresh-gen1"},
		{AccessToken: "retire-access-gen2", RefreshToken: "retire-refresh-gen2"},
		{AccessToken: "retire-access-gen3", RefreshToken: "retire-refresh-gen3"},
	}
	registerSecrets(generations[0].AccessToken, generations[0].RefreshToken)

	rotateOnce(t, path, generations[0], generations[1])
	if !secretRegistered(generations[0].AccessToken) {
		t.Fatalf("第一代轮换不应立即删除刚退位的 gen0（宽限期）")
	}

	rotateOnce(t, path, generations[1], generations[2])
	if secretRegistered(generations[0].AccessToken) || secretRegistered(generations[0].RefreshToken) {
		t.Fatalf("第二轮换应淘汰已闲置一个周期的 gen0")
	}
	if !secretRegistered(generations[1].AccessToken) {
		t.Fatalf("上一代 gen1 处于宽限期，应保留")
	}

	rotateOnce(t, path, generations[2], generations[3])
	if secretRegistered(generations[1].AccessToken) {
		t.Fatalf("第三轮换应淘汰 gen1")
	}
	for _, secret := range []string{generations[2].AccessToken, generations[3].AccessToken} {
		if !secretRegistered(secret) {
			t.Fatalf("在用/宽限期秘密不应被删除: %s", secret)
		}
	}
	if got := redactSensitiveText(generations[0].AccessToken); got != generations[0].AccessToken {
		t.Fatalf("已淘汰的 gen0 不应再消耗脱敏成本: %q", got)
	}
	if got := redactSensitiveText(generations[3].AccessToken); got != "[REDACTED]" {
		t.Fatalf("在用的 gen3 必须继续脱敏: %q", got)
	}
}

// 上游返回相同令牌（未真实轮换）时不淘汰：保护 doRefreshToken 单文件模式下
// 旧值仍被使用的场景。
func TestRetireStaleCredentialSecretsNoRealRotationSkips(t *testing.T) {
	isolateRedactionState(t)
	const path = "workbuddy-retire-same.json"
	same := StoredTokens{AccessToken: "retire-same-access-value", RefreshToken: "retire-same-refresh-value"}
	registerSecrets(same.AccessToken, same.RefreshToken)
	for i := 0; i < 3; i++ {
		retireStaleCredentialSecrets(path, same, same)
	}
	if !secretRegistered(same.AccessToken) || !secretRegistered(same.RefreshToken) {
		t.Fatalf("未发生真实轮换时不应淘汰仍在使用的令牌")
	}
	if got := redactSensitiveText(same.AccessToken); got != "[REDACTED]" {
		t.Fatalf("在用令牌脱敏丢失: %q", got)
	}
}

// 其他凭据路径仍在使用同一令牌值时，淘汰不得破坏其脱敏保护。
func TestRetireStaleCredentialSecretsSharedValueSurvives(t *testing.T) {
	isolateRedactionState(t)
	shared := StoredTokens{AccessToken: "shared-retire-access-value", RefreshToken: "shared-retire-refresh-value"}
	const pathA = "workbuddy-retire-a.json"
	const pathB = "workbuddy-retire-b.json"
	registerSecrets(shared.AccessToken, shared.RefreshToken)
	// B 路径也登记为在用（持有同一值），A 轮换两次后不得删除共享值。
	retireStaleCredentialSecrets(pathB, shared, shared)
	gen1 := StoredTokens{AccessToken: "a-gen1-access-xxxx", RefreshToken: "a-gen1-refresh-xxxx"}
	gen2 := StoredTokens{AccessToken: "a-gen2-access-xxxx", RefreshToken: "a-gen2-refresh-xxxx"}
	rotateOnce(t, pathA, shared, gen1)
	rotateOnce(t, pathA, gen1, gen2)
	if !secretRegistered(shared.AccessToken) || !secretRegistered(shared.RefreshToken) {
		t.Fatalf("共享令牌被另一路径的淘汰误删")
	}
	if got := redactSensitiveText(shared.AccessToken); got != "[REDACTED]" {
		t.Fatalf("共享令牌的脱敏保护丢失: %q", got)
	}
}

// 淘汰登记不随轮换次数无界增长：同一路径反复轮换后登记表仍只有固定条目。
func TestRetireStaleCredentialSecretsBookkeepingBounded(t *testing.T) {
	isolateRedactionState(t)
	const path = "workbuddy-retire-bounded.json"
	prev := StoredTokens{AccessToken: "bounded-access-gen0", RefreshToken: "bounded-refresh-gen0"}
	registerSecrets(prev.AccessToken, prev.RefreshToken)
	for gen := 1; gen <= 50; gen++ {
		prefix := string(rune('a'+gen/26)) + string(rune('a'+gen%26))
		next := StoredTokens{AccessToken: prefix + "-bounded-access-xxxx", RefreshToken: prefix + "-bounded-refresh-xxxx"}
		rotateOnce(t, path, prev, next)
		prev = next
	}
	secretRetireMu.Lock()
	graceSize, currentSize := len(secretRetireState.grace), len(secretRetireState.current)
	secretRetireMu.Unlock()
	if graceSize != 1 || currentSize != 1 {
		t.Fatalf("登记表应按凭据路径有界，grace=%d current=%d", graceSize, currentSize)
	}
	if got := redactSensitiveText(prev.AccessToken); got != "[REDACTED]" {
		t.Fatalf("在用令牌脱敏丢失: %q", got)
	}
	// gen0 是否退出词表取决于全局 accounts 是否仍持有该值（其他测试可能残留），
	// 这里只断言登记表有界与当前值脱敏；退表行为由 TwoGenerationGrace 用例钉住。
}
