package main

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
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
var sensitiveSecrets sync.Map

// 词表仍只增加；只缓存派生替换器，不改变任何秘密值的保护期。
type redactionBundle struct {
	replacer  *strings.Replacer
	shortList []string
}

type redactionCache struct {
	seq    uint64
	bundle *redactionBundle
}

var (
	redactionCachePtr atomic.Pointer[redactionCache]
	redactionSeq      atomic.Uint64
)

var (
	namedSecretPattern  = regexp.MustCompile(`(?i)(\b(?:access[_-]?token|refresh[_-]?token|admin[_-]?key|api[_-]?key|x-api-key|x-refresh-token|authorization)\b["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'[^']*'|(?:Bearer[ \t]+)?[^\s,;}\[\]]+)`)
	bearerSecretPattern = regexp.MustCompile(`(?i)(\bBearer[ \t]+)[A-Za-z0-9._~+/=-]+`)
)

func registerSecrets(values ...string) {
	changed := false
	for _, value := range values {
		if value != "" {
			if _, loaded := sensitiveSecrets.LoadOrStore(value, struct{}{}); !loaded {
				changed = true
			}
		}
	}
	if changed {
		invalidateRedactionBundle()
	}
}

func invalidateRedactionBundle() {
	redactionSeq.Add(1)
	redactionCachePtr.Store(nil)
}

func registerCredentialSecrets(auth *StoredAuth) {
	if auth != nil {
		registerSecrets(auth.Auth.AccessToken, auth.Auth.RefreshToken)
	}
}

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

func currentRedactionBundle() *redactionBundle {
	for {
		seq := redactionSeq.Load()
		if cache := redactionCachePtr.Load(); cache != nil && cache.seq == seq {
			return cache.bundle
		}
		bundle := buildRedactionBundle()
		if redactionSeq.Load() != seq {
			continue
		}
		redactionCachePtr.Store(&redactionCache{seq: seq, bundle: bundle})
		return bundle
	}
}

func redactSensitiveText(text string) string {
	if text == "" {
		return ""
	}
	bundle := currentRedactionBundle()
	if bundle.replacer != nil {
		text = bundle.replacer.Replace(text)
	}
	for _, secret := range bundle.shortList {
		text = redactShortSecret(text, secret)
	}
	if mayContainNamedOrBearerSecret(text) {
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
	}
	if containsASCIIFold(text, "eyj") {
		text = bearerLikeRe.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

// mayContainNamedOrBearerSecret 在进入重度回溯正则前先做零分配的 ASCII 子串初筛；
// 仅排除绝对不含 token/key/authorization/bearer 的纯普通文本，不改变任何命中语义。
func mayContainNamedOrBearerSecret(s string) bool {
	return containsASCIIFold(s, "token") ||
		containsASCIIFold(s, "key") ||
		containsASCIIFold(s, "authorization") ||
		containsASCIIFold(s, "bearer")
}

func containsASCIIFold(s, needleLower string) bool {
	n := len(needleLower)
	if n == 0 {
		return true
	}
	if len(s) < n {
		return false
	}
	c0 := needleLower[0]
	c0Upper := c0
	if c0 >= 'a' && c0 <= 'z' {
		c0Upper = c0 - ('a' - 'A')
	}
	limit := len(s) - n
	for i := 0; i <= limit; i++ {
		b := s[i]
		if b != c0 && b != c0Upper {
			continue
		}
		matched := true
		for j := 1; j < n; j++ {
			ch := s[i+j]
			if ch >= 'A' && ch <= 'Z' {
				ch += 'a' - 'A'
			}
			if ch != needleLower[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
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
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		return redactSensitiveText(v)
	case error:
		return safeDebugError(v)
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return v
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
		return redactReflectValue(reflect.ValueOf(value))
	}
}

// redactReflectValue 在不经过 JSON 二次反序列化树的前提下，递归脱敏任意结构体、切片、指针和 map 副本。
// 保持与 json.Marshal 相同的导出字段/标签规则，同时避免为大响应重复构造 map[string]any。
func redactReflectValue(rv reflect.Value) any {
	if !rv.IsValid() {
		return nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.CanInterface() {
		if m, ok := rv.Interface().(json.Marshaler); ok {
			raw, err := m.MarshalJSON()
			if err != nil {
				return nil
			}
			var parsed any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if dec.Decode(&parsed) == nil {
				return redactDebugValue(parsed)
			}
			return redactSensitiveText(string(raw))
		}
		if errVal, ok := rv.Interface().(error); ok {
			return safeDebugError(errVal)
		}
	}
	switch rv.Kind() {
	case reflect.String:
		return redactSensitiveText(rv.String())
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Slice:
		if rv.IsNil() {
			return nil
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return rv.Interface()
		}
		n := rv.Len()
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = redactReflectValue(rv.Index(i))
		}
		return out
	case reflect.Array:
		n := rv.Len()
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = redactReflectValue(rv.Index(i))
		}
		return out
	case reflect.Map:
		if rv.IsNil() {
			return nil
		}
		if rv.Type().Key().Kind() != reflect.String {
			return rv.Interface()
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			k := iter.Key().String()
			if secretLogField(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redactReflectValue(iter.Value())
			}
		}
		return out
	case reflect.Struct:
		if !typeMayContainStringsOrMaps(rv.Type()) {
			if rv.CanInterface() {
				return rv.Interface()
			}
		}
		out := make(map[string]any, rv.NumField())
		collectRedactedStructFields(rv, out)
		return out
	default:
		if rv.CanInterface() {
			return rv.Interface()
		}
		return nil
	}
}

// typeMayContainStringsOrMaps 判断结构体是否可能包含需要脱敏的字符串、错误或动态容器字段。
// 纯数值结构体直接交由标准库编码，既省分配又严格保持标准 JSON 字段语义。
func typeMayContainStringsOrMaps(rt reflect.Type) bool {
	for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
		rt = rt.Elem()
	}
	switch rt.Kind() {
	case reflect.String, reflect.Interface, reflect.Map:
		return true
	case reflect.Struct:
		for i := 0; i < rt.NumField(); i++ {
			sf := rt.Field(i)
			if (sf.PkgPath != "" && !sf.Anonymous) || sf.Tag.Get("json") == "-" {
				continue
			}
			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			if sf.PkgPath != "" && name != "" {
				continue
			}
			if name == "" {
				name = sf.Name
			}
			if secretLogField(name) || typeMayContainStringsOrMaps(sf.Type) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func collectRedactedStructFields(rv reflect.Value, out map[string]any) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" && !sf.Anonymous {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		omitEmpty := hasJSONTagOption(opts, "omitempty")
		fv := rv.Field(i)
		if sf.Anonymous && name == "" && !omitEmpty {
			embedded := fv
			for embedded.Kind() == reflect.Pointer {
				if embedded.IsNil() {
					embedded = reflect.Value{}
					break
				}
				embedded = embedded.Elem()
			}
			if embedded.IsValid() && embedded.Kind() == reflect.Struct {
				collectRedactedStructFields(embedded, out)
				continue
			}
		}
		if sf.PkgPath != "" {
			continue
		}
		if omitEmpty && isEmptyJSONReflectValue(fv) {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		if secretLogField(name) {
			out[name] = "[REDACTED]"
		} else {
			out[name] = redactReflectValue(fv)
		}
	}
}

func hasJSONTagOption(opts, target string) bool {
	for opts != "" {
		opt, rest, _ := strings.Cut(opts, ",")
		if opt == target {
			return true
		}
		opts = rest
	}
	return false
}

func isEmptyJSONReflectValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

func secretLogField(key string) bool {
	var buf [32]byte
	n := 0
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '_' || c == '-' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if n >= len(buf) {
			return false
		}
		buf[n] = c
		n++
	}
	switch string(buf[:n]) {
	case "accesstoken", "refreshtoken", "adminkey", "apikey", "xapikey", "xrefreshtoken", "authorization":
		return true
	default:
		return false
	}
}
