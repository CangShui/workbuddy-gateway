package main

import (
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// 脱敏仅用于日志/调试展示。不得把脱敏后的错误送回业务分类或上游响应解析。
var sensitiveSecrets sync.Map

var (
	namedSecretPattern  = regexp.MustCompile(`(?i)(\b(?:access[_-]?token|refresh[_-]?token|admin[_-]?key|api[_-]?key|x-api-key|x-refresh-token|authorization)\b["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'[^']*'|(?:Bearer[ \t]+)?[^\s,;}\[\]]+)`)
	bearerSecretPattern = regexp.MustCompile(`(?i)(\bBearer[ \t]+)[A-Za-z0-9._~+/=-]+`)
)

func registerSecrets(values ...string) {
	for _, value := range values {
		if value != "" {
			sensitiveSecrets.Store(value, struct{}{})
		}
	}
}

func registerCredentialSecrets(auth *StoredAuth) {
	if auth != nil {
		registerSecrets(auth.Auth.AccessToken, auth.Auth.RefreshToken)
	}
}

func redactSensitiveText(text string) string {
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
