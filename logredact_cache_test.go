package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// 保留未缓存规则作为语义和性能对照，不用于业务路径。
func uncachedRedactionForTest(text string) string {
	var secrets []string
	sensitiveSecrets.Range(func(key, _ any) bool {
		value := key.(string)
		secrets = append(secrets, value)
		// 错误正文可能是 JSON 字符串；同时隐藏其转义后的表示。
		escaped := strconv.Quote(value)
		escaped = escaped[1 : len(escaped)-1]
		if escaped != value {
			secrets = append(secrets, escaped)
		}
		return true
	})
	// 长值优先、一次替换，避免重叠秘密值导致部分泄漏或改写替换标记。
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	var pairs []string
	for _, secret := range secrets {
		if len(secret) >= 8 {
			pairs = append(pairs, secret, "[REDACTED]")
		}
	}
	if len(pairs) > 0 {
		text = strings.NewReplacer(pairs...).Replace(text)
	}
	for _, secret := range secrets {
		if len(secret) < 8 {
			text = redactShortSecret(text, secret)
		}
	}
	text = namedSecretPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := namedSecretPattern.FindStringSubmatch(match)
		value := "[REDACTED]"
		if parts[2][0] == '"' {
			value = `"[REDACTED]"`
		} else if parts[2][0] == '\'' {
			value = "'[REDACTED]'"
		}
		return parts[1] + value
	})
	text = bearerSecretPattern.ReplaceAllString(text, "${1}[REDACTED]")
	return bearerLikeRe.ReplaceAllString(text, "[REDACTED]")
}

func TestRedactionCacheMatchesUncachedRules(t *testing.T) {
	isolateSecrets(t)
	registerSecrets("synthetic-long-secret-alpha", "synthetic-long-secret-alpha-tail", "k7",
		"私有短词", "quoted\\\"synthetic-secret", "synthetic-unicode-令牌-尾部")
	for _, text := range []string{
		"", "ordinary diagnostic",
		"synthetic-long-secret-alpha-tail and synthetic-long-secret-alpha",
		"k7 sk7 a_k7 k7_",
		"私有短词 与中文",
		"quoted\\\"synthetic-secret",
		`quoted\\\"synthetic-secret`,
		"synthetic-unicode-令牌-尾部",
		`api_key="unregistered-value" Authorization: Bearer synthetic-token`,
	} {
		if got, want := redactSensitiveText(text), uncachedRedactionForTest(text); got != want {
			t.Fatal("cached redaction changed existing output rules")
		}
	}
}

func TestRedactionCacheReuseAndRegistration(t *testing.T) {
	isolateSecrets(t)
	empty := currentRedactionBundle()
	registerSecrets("", "")
	if currentRedactionBundle() != empty {
		t.Fatal("empty registration rebuilt cache")
	}
	registerSecrets("synthetic-cache-first-secret")
	first := currentRedactionBundle()
	registerSecrets("synthetic-cache-first-secret", "")
	if currentRedactionBundle() != first {
		t.Fatal("duplicate registration rebuilt cache")
	}
	registerSecrets("synthetic-cache-second-secret")
	if currentRedactionBundle() == first {
		t.Fatal("new secret did not invalidate cache")
	}
	if redactSensitiveText("synthetic-cache-second-secret synthetic-cache-first-secret") != "[REDACTED] [REDACTED]" {
		t.Fatal("registered secrets lost protection")
	}
	if first.replacer.Replace("synthetic-cache-second-secret") != "synthetic-cache-second-secret" {
		t.Fatal("published bundle was mutated by later registration")
	}
}

func TestRedactionCacheConcurrentRegistration(t *testing.T) {
	isolateSecrets(t)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				secret := fmt.Sprintf("synthetic-cache-concurrent-%d-%d", w, i)
				registerSecrets(secret)
				if redactSensitiveText(secret) != "[REDACTED]" {
					t.Error("completed registration did not protect its secret")
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestRedactionCachePreservesStoredOldTokens(t *testing.T) {
	isolatePolicies(t)
	isolateSecrets(t)
	resetStatsState(t)
	loadOptionalStats()
	old := StoredTokens{AccessToken: "synthetic-cache-old-access-value", RefreshToken: "synthetic-cache-old-refresh-value"}
	acc := &Account{Path: "cache-synthetic.json", Auth: &StoredAuth{Auth: old},
		ModelStates: map[string]*modelRuntimeState{"synthetic-model": {
			CostClass: modelCostPaid, LastReason: "HTTP 429 access=" + old.AccessToken,
		}}}
	accounts = []*Account{acc}
	registerCredentialSecrets(acc.Auth)
	_ = redactSensitiveText("warm cached redactor")
	for i := 0; i < 50; i++ {
		acc.Auth.Auth.AccessToken = fmt.Sprintf("synthetic-cache-new-access-%d", i)
		// 刷新令牌保持不变也是受支持的刷新形态。
		registerCredentialSecrets(acc.Auth)
	}
	if _, ok := sensitiveSecrets.Load(old.AccessToken); !ok {
		t.Fatal("cache optimization retired old access token")
	}
	if err := flushOptionalStats(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), old.AccessToken) || !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatal("stored model reason exposed old token")
	}
}

func TestRedactionCacheAuditPersistsWithoutSecrets(t *testing.T) {
	isolatedGatewayConfig(t)
	isolateSecrets(t)
	secret := "synthetic-cache-audit-secret"
	registerSecrets(secret)
	log.Printf("[缓存验收] traceId=redaction-cache-audit 阶段=诊断写入 access=%s", secret)
	files, err := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
	if err != nil || len(files) == 0 {
		t.Fatal("file-backed audit log missing")
	}
	var audit strings.Builder
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		audit.Write(raw)
	}
	if !strings.Contains(audit.String(), "[日志脱敏] traceId=") ||
		!strings.Contains(audit.String(), "traceId=redaction-cache-audit") ||
		!strings.Contains(audit.String(), "access=[REDACTED]") || strings.Contains(audit.String(), secret) {
		t.Fatal("cache policy or redacted diagnostic missing from audit")
	}
}

var redactionBenchmarkOutput string

func BenchmarkRedactionCache(b *testing.B) {
	isolateSecrets(b)
	for i := 0; i < 25; i++ {
		registerSecrets(fmt.Sprintf("synthetic-benchmark-secret-%03d-long-value", i))
	}
	text := "[诊断] traceId=synthetic-benchmark status=429 access=synthetic-benchmark-secret-007-long-value"
	want := uncachedRedactionForTest(text)
	if got := redactSensitiveText(text); got != want {
		b.Fatal("benchmark variants produce different redacted output")
	}
	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			redactionBenchmarkOutput = uncachedRedactionForTest(text)
		}
	})
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			redactionBenchmarkOutput = redactSensitiveText(text)
		}
	})
}
