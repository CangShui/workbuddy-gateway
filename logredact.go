package main

import (
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// 脱敏仅用于日志/调试展示。不得把脱敏后的错误送回业务分类或上游响应解析。
//
// 性能形态（pprof 实测）：redactSensitiveText 由状态快照（3 秒）与统计落盘
// （5 秒）周期调用，每次都遍历全部注册秘密并重建 strings.Replacer，成本随词表
// 大小线性放大且与请求量无关。词表只在 register/unregister 时变化（写少读多），
// 因此把「汇总 + 排序 + 预编译」缓存为不可变快照，词表变更时置脏、下次调用重建。
var sensitiveSecrets sync.Map

// redactionBundle 是 redactSensitiveText 使用的不可变派生快照。
type redactionBundle struct {
	replacer  *strings.Replacer // 长秘密（>=8 字符）一次替换；nil 表示无长秘密
	shortList []string          // 短秘密（<8 字符）逐词替换，沿用长值优先顺序
}

// redactionCache 把快照与构建时的词表版本号绑定：读取端每次校验版本号，
// 注册端置脏后旧快照立即失效，不会长期滞留。
type redactionCache struct {
	seq    uint64
	bundle *redactionBundle
}

var (
	redactionCachePtr atomic.Pointer[redactionCache]
	redactionSeq      atomic.Uint64 // 词表变更计数
)

var (
	namedSecretPattern  = regexp.MustCompile(`(?i)(\b(?:access[_-]?token|refresh[_-]?token|admin[_-]?key|api[_-]?key|x-api-key|x-refresh-token|authorization)\b["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'[^']*'|(?:Bearer[ \t]+)?[^\s,;}\[\]]+)`)
	bearerSecretPattern = regexp.MustCompile(`(?i)(\bBearer[ \t]+)[A-Za-z0-9._~+/=-]+`)
)

func invalidateRedactionBundle() {
	redactionSeq.Add(1)
	redactionCachePtr.Store(nil)
}

func registerSecrets(values ...string) {
	changed := false
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, loaded := sensitiveSecrets.Swap(value, struct{}{}); !loaded {
			changed = true
		}
	}
	if changed {
		invalidateRedactionBundle()
	}
}

// unregisterSecrets 把已作废的秘密值移出词表，供令牌轮换后延迟淘汰使用，
// 控制词表规模不再随运行时长只增不减。
func unregisterSecrets(values ...string) {
	changed := false
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, loaded := sensitiveSecrets.LoadAndDelete(value); loaded {
			changed = true
		}
	}
	if changed {
		invalidateRedactionBundle()
	}
}

func registerCredentialSecrets(auth *StoredAuth) {
	if auth != nil {
		registerSecrets(auth.Auth.AccessToken, auth.Auth.RefreshToken)
	}
}

// buildRedactionBundle 汇总当前词表并预编译替换器。
func buildRedactionBundle() *redactionBundle {
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
	bundle := &redactionBundle{}
	var pairs []string
	for _, secret := range secrets {
		if len(secret) >= 8 {
			pairs = append(pairs, secret, "[REDACTED]")
		} else {
			bundle.shortList = append(bundle.shortList, secret)
		}
	}
	if len(pairs) > 0 {
		bundle.replacer = strings.NewReplacer(pairs...)
	}
	return bundle
}

// currentRedactionBundle 返回与当前词表版本一致的快照；脏或不存在时重建。
// 版本号校验保证并发注册最多让新秘密晚一个调用周期生效，不会长期滞留。
func currentRedactionBundle() *redactionBundle {
	for {
		seq := redactionSeq.Load()
		if cache := redactionCachePtr.Load(); cache != nil && cache.seq == seq {
			return cache.bundle
		}
		bundle := buildRedactionBundle()
		if latest := redactionSeq.Load(); latest != seq {
			continue // 构建期间词表又变了，重试以保证快照完整
		}
		redactionCachePtr.Store(&redactionCache{seq: seq, bundle: bundle})
		return bundle
	}
}

func redactSensitiveText(text string) string {
	bundle := currentRedactionBundle()
	if bundle.replacer != nil {
		text = bundle.replacer.Replace(text)
	}
	for _, secret := range bundle.shortList {
		text = redactShortSecret(text, secret)
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

// 短秘密值仅替换完整词，避免短测试密钥吞掉普通错误原因中的单个字母。
func redactShortSecret(text, secret string) string {
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	var out strings.Builder
	for {
		index := strings.Index(text, secret)
		if index < 0 {
			out.WriteString(text)
			return out.String()
		}
		end := index + len(secret)
		left, _ := utf8.DecodeLastRuneInString(text[:index])
		right, _ := utf8.DecodeRuneInString(text[end:])
		out.WriteString(text[:index])
		if (index == 0 || !word(left)) && (end == len(text) || !word(right)) {
			out.WriteString("[REDACTED]")
		} else {
			out.WriteString(secret)
		}
		text = text[end:]
	}
}

type redactingLogWriter struct{ writer io.Writer }

func (w *redactingLogWriter) Write(data []byte) (int, error) {
	redacted := redactSensitiveText(string(data))
	n, err := io.WriteString(w.writer, redacted)
	if err == nil && n != len(redacted) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, err
	}
	// io.Writer 返回消耗的原始输入长度，而不是脱敏后的输出长度。
	return len(data), nil
}

func redactDebugValue(value any) any {
	switch v := value.(type) {
	case string:
		return redactSensitiveText(v)
	case error:
		return safeDebugError(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if secretLogField(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = redactDebugValue(item)
			}
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(v))
		for key, item := range v {
			if secretLogField(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = redactSensitiveText(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactDebugValue(item)
		}
		return out
	case []string:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = redactSensitiveText(item)
		}
		return out
	default:
		return value
	}
}

func secretLogField(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch key {
	case "accesstoken", "refreshtoken", "adminkey", "apikey", "xapikey", "xrefreshtoken", "authorization":
		return true
	default:
		return false
	}
}
