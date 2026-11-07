package main

// 请求脱敏：移植自 magpie（github.com/yetone/magpie）internal/redact 的规则部分，不含本地模型。
//
// 发往上游前，把提示词里的密钥、凭据、个人信息和用户自定义词换成占位符
// {{类型_xxxxxxxx}}（8 位为 HMAC 摘要，同一值每次相同，便于上游缓存命中）；
// 上游回复里出现的占位符（含模型改写的大小写、丢掉的花括号）在网关内换回原值。
// 原值只保存在本进程内存中，不写盘、不进日志；进程重启后占位符密钥随之更换。
//
// 本文件中的识别规则、占位符格式与还原逻辑改编自 magpie，按其 MIT 许可保留以下声明：
//
// MIT License
//
// Copyright (c) 2026 yetone
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// ---- 配置 -----------------------------------------------------------------------

// redactFileConfig 是 config.json 的 redact 段。三项全关（默认）时不改动任何请求。
type redactFileConfig struct {
	Secrets  bool     `json:"secrets"`  // 密钥与凭据：API Key、Token、JWT、私钥、密码、Authorization/Cookie
	Personal bool     `json:"personal"` // 个人信息：邮箱、手机号、身份证号、银行卡号
	Words    []string `json:"words"`    // 自定义脱敏词，按原样（区分大小写）匹配，含在长词内也替换
}

const (
	redactMaxWords   = 500
	redactMaxWordLen = 200
)

func normalizeRedactConfig(c redactFileConfig) (redactFileConfig, error) {
	seen := map[string]bool{}
	words := make([]string, 0, len(c.Words))
	for _, w := range c.Words {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		if strings.ContainsAny(w, "\r\n") {
			return c, gatewayValidationError("redact.words 每项必须是单行文本")
		}
		if utf8.RuneCountInString(w) < 2 {
			return c, gatewayValidationError("redact.words 每项至少 2 个字符，避免单个字母吞掉普通文本")
		}
		if utf8.RuneCountInString(w) > redactMaxWordLen {
			return c, gatewayValidationError("redact.words 每项不能超过 200 个字符")
		}
		seen[w] = true
		words = append(words, w)
	}
	if len(words) > redactMaxWords {
		return c, gatewayValidationError("redact.words 最多 500 项")
	}
	c.Words = words
	return c, nil
}

type redactOptions struct {
	secrets, personal bool
	words             []string // 已按长度降序，长词优先
	listed            []string // 配置中的原顺序，仅供管理台展示
}

func (o redactOptions) active() bool { return o.secrets || o.personal || len(o.words) > 0 }

var redactSettings atomic.Pointer[redactOptions]

func setRedactConfig(c redactFileConfig) {
	c, _ = normalizeRedactConfig(c) // 调用前 parseRuntimeConfig 已校验
	words := append([]string(nil), c.Words...)
	sort.SliceStable(words, func(i, j int) bool { return len(words[i]) > len(words[j]) })
	// 自定义词由用户标为敏感：普通日志里同样隐藏。
	registerSecrets(words...)
	redactSettings.Store(&redactOptions{secrets: c.Secrets, personal: c.Personal, words: words, listed: c.Words})
}

func currentRedactOptions() redactOptions {
	if o := redactSettings.Load(); o != nil {
		return *o
	}
	return redactOptions{}
}

// redactHit 是一次命中：类型、占位符与提示（开头几个字符和长度），从不包含原值。
type redactHit struct {
	Kind        string `json:"kind"`
	Placeholder string `json:"placeholder"`
	Head        string `json:"head"`
	Len         int    `json:"len"`
}

// redactHint 是可以留在记录里的那一点原值：首字符（密钥则为标明类型的前缀，如 sk-、ghp_）与字符数。
func redactHint(kind, v string) (string, int) {
	n := utf8.RuneCountInString(v)
	if n < 2 {
		return "", n
	}
	k := 1
	switch kind {
	case "API_KEY", "TOKEN", "SECRET", "PRIVATE_KEY":
		if n >= 16 {
			k = 0
			for _, c := range v {
				if k >= 4 || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_", c) {
					break
				}
				k++
				if c == '-' || c == '_' {
					break
				}
			}
			k = max(k, 1)
		}
	}
	return string([]rune(v)[:k]), n
}

func summarizeRedactHits(hits []redactHit) string {
	counts := map[string]int{}
	for _, h := range hits {
		counts[h.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = k + ":" + strconv.Itoa(counts[k])
	}
	return strings.Join(parts, ",")
}

// ---- 规则（移植自 magpie internal/redact/redact.go） -----------------------------

type redactRule struct {
	kind     string
	re       *regexp.Regexp
	markers  []string // 文本中必须出现其一才运行正则；空表示总是运行
	bound    string   // 不得紧贴匹配两侧的字符：长词里的一段不算密钥
	ok       func(string) bool
	at       func(s string, a, b int) bool
	personal bool
}

const (
	rAlnum    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	rTokenCh  = rAlnum + "_-"
	rDigits   = "0123456789"
	rQuote    = `(?:\\?["'])?`
	rSecretCh = `[A-Za-z0-9_\-./+=~!@#%^&*]`
)

var redactSecretNames = `password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential`

var cjkPasswordNames = []string{"密码", "密碼", "口令", "PIN码", "PIN碼", "安全码", "安全碼", "パスワード", "暗証番号", "비밀번호", "암호", "비번"}

var cjkKeyNames = []string{
	"密钥", "密鑰", "秘钥", "秘鑰", "私钥", "私鑰", "金钥", "金鑰", "令牌", "权杖", "權杖", "授权码", "授權碼",
	"秘密鍵", "トークン", "シークレット", "APIキー", "アクセスキー", "토큰", "비밀키", "API키",
}

func cjkNamedPattern(names []string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:` + strings.Join(names, "|") + `)[ \t]*(?:[:：=][ \t]*|[为為是][ \t]*[:：]?[ \t]*|は[ \t]*|는[ \t]*)?(?:\\?["'“「])?(` + rSecretCh + `{3,63}[A-Za-z0-9_\-/+=~!@#%^&*])`)
}

var redactRules = []redactRule{
	{kind: "PRIVATE_KEY", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----[\s\S]+?-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`), markers: []string{"PRIVATE KEY"}},
	{kind: "API_KEY", re: regexp.MustCompile(`sk-(?:ant-|proj-|or-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`), markers: []string{"sk-"}, bound: rTokenCh, ok: notASlug},
	{kind: "API_KEY", re: regexp.MustCompile(`[a-z]{2,4}_sk_[A-Za-z0-9_-]{20,}`), markers: []string{"_sk_"}, bound: rTokenCh, ok: secretValue},
	{kind: "API_KEY", re: regexp.MustCompile(`ark-[A-Za-z0-9][A-Za-z0-9_-]{15,}`), markers: []string{"ark-"}, bound: rTokenCh, ok: secretValue},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}|glpat-[A-Za-z0-9_-]{20,})`), markers: []string{"gh", "github_pat_", "glpat-"}, bound: rTokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), markers: []string{"AIza"}, bound: rTokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`xox[abposr]-[0-9A-Za-z-]{10,}`), markers: []string{"xox"}, bound: rTokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:sk|rk)_live_[0-9A-Za-z]{20,}`), markers: []string{"_live_"}, bound: rTokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), markers: []string{"AKIA", "ASIA"}, bound: rAlnum},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:hf_[A-Za-z0-9]{30,}|gsk_[A-Za-z0-9]{40,}|xai-[A-Za-z0-9]{40,}|npm_[A-Za-z0-9]{36}|pypi-[A-Za-z0-9_-]{50,}|dop_v1_[a-f0-9]{64}|SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43})`),
		markers: []string{"hf_", "gsk_", "xai-", "npm_", "pypi-", "dop_v1_", "SG."}, bound: rTokenCh},
	{kind: "TOKEN", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), markers: []string{"eyJ"}, bound: rTokenCh},
	// URL 里 user:password@host 的密码
	{kind: "PASSWORD", re: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s:@/?#"'<>]*:([^\s@/?#"'<>]{3,})@`), markers: []string{"://"}, ok: notAVariable, at: hiddenInURL},
	// password = …、API_KEY: "…"：.env 与配置文件写法
	{kind: "SECRET", re: regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:` + redactSecretNames + `)[A-Za-z0-9_.-]*` + rQuote + `[ \t]*[:=][ \t]*` + rQuote + `(` + rSecretCh + `{8,})`),
		markers: []string{"pass", "PASS", "Pass", "secret", "SECRET", "Secret", "token", "TOKEN", "Token", "key", "KEY", "Key", "credential", "CREDENTIAL", "Credential"},
		ok:      fieldSecretValue, at: notCode},
	// 大写环境变量名以 _KEY/_PASS 结尾：SERVICE_X_KEY、DB_PASS
	{kind: "SECRET", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])[A-Za-z0-9_.-]*_(?:KEY|PASS)` + rQuote + `[ \t]*[:=][ \t]*` + rQuote + `(` + rSecretCh + `{8,})`),
		markers: []string{"_KEY", "_PASS"}, ok: fieldSecretValue, at: notPlainKeyOrCode},
	// Authorization / Cookie 请求头的值
	{kind: "SECRET", re: regexp.MustCompile(`(?im)^[ \t]*(?:authorization|cookie)[ \t]*:[ \t]*(\S[^\r\n]{7,})`),
		markers: []string{"authorization", "Authorization", "AUTHORIZATION", "cookie", "Cookie", "COOKIE"}, ok: headerValue},
	{kind: "TOKEN", re: regexp.MustCompile(`(?i)\bBearer[ \t]+([A-Za-z0-9._~+/=-]{16,})`), markers: []string{"earer", "EARER"}, bound: rTokenCh, ok: secretValue},
	// 中日韩文里的密码与密钥：密码：…、令牌是 …
	{kind: "PASSWORD", re: cjkNamedPattern(cjkPasswordNames), markers: cjkPasswordNames, ok: notAVariable},
	{kind: "SECRET", re: cjkNamedPattern(cjkKeyNames), markers: cjkKeyNames, at: keyAt},
	// 中文消息里的英文名：apikey 为 …、token 是 …、API Key：…
	{kind: "SECRET", re: regexp.MustCompile(`(?i)(?:` + redactSecretNames + `|pwd|api[ ]key|access[ ]key|secret[ ]key|key|\bpin(?:[ ]?code)?)` + rQuote +
		`[ \t]*(?:[为為是][ \t]*[:：]?|：)[ \t]*(?:\\?["'“「])?(` + rSecretCh + `{3,}[A-Za-z0-9_\-/+=~!@#%^&*])`),
		markers: []string{"为", "為", "是", "："}, at: keyAt},

	{kind: "EMAIL", re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`), markers: []string{"@"}, bound: rAlnum + "_%+-", ok: realEmail, at: emailAt, personal: true},
	{kind: "ID_CARD", re: regexp.MustCompile(`[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]`), bound: rAlnum, ok: chineseID, personal: true},
	{kind: "PHONE", re: regexp.MustCompile(`(?:\+?86[- ]?)?1[3-9]\d{9}`), bound: rAlnum, at: phoneAt, personal: true},
	{kind: "BANK_CARD", re: regexp.MustCompile(`[3-6]\d{3}(?:[ -]?\d{4}){2,3}(?:[ -]?\d{1,3})?`), bound: rAlnum, ok: luhn, personal: true},
}

// notAVariable 排除代表值的写法：${PASS}、<password>、****。
func notAVariable(v string) bool {
	if v == "" || strings.ContainsAny(v[:1], "$%{<[*") {
		return false
	}
	return strings.Trim(v, "*xX.") != ""
}

func hasLetter(v string) bool {
	return strings.IndexFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0
}

// secretValue：看起来像秘密值而不是代码——字母数字兼有，不是变量引用。
func secretValue(v string) bool {
	if !notAVariable(v) || strings.HasPrefix(v, "process.env") || strings.HasPrefix(v, "os.") {
		return false
	}
	return strings.ContainsAny(v, rDigits) && hasLetter(v)
}

var versionLike = regexp.MustCompile(`^v?\d+(?:\.\d+)+(?:[-+][0-9A-Za-z.-]+)?$`)

func pathValue(v string) bool {
	return strings.HasPrefix(v, "/") || strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../") || strings.HasPrefix(v, "~/")
}

// fieldSecretValue：字段后的值，排除路径与版本号（token 是 0.15.0-rc1、私钥 ~/.ssh/id_ed25519）。
func fieldSecretValue(v string) bool {
	return secretValue(v) && !pathValue(v) && !versionLike.MatchString(v)
}

func keyValue(v string) bool {
	if !notAVariable(v) {
		return false
	}
	return hasLetter(v) && (strings.ContainsAny(v, rDigits) || len(v) >= 16)
}

var keySeparators = []string{"为", "為", "是", "：", ":", "=", "は", "는"}
var quantityUnits = []string{"位", "比特", "字节", "字節", "个", "個", "次", "秒", "毫秒", "分钟", "分鐘", "小时", "小時", "天", "年", "bit", "byte", "ms", "sec", "min"}

// keyAt：密钥值，或紧跟分隔符、不计量的六位以上纯数字（密钥为 2048 位 不算）。
func keyAt(s string, a, b int) bool {
	v := s[a:b]
	if pathValue(v) || versionLike.MatchString(v) {
		return false
	}
	if keyValue(v) {
		return true
	}
	if len(v) < 6 || strings.Trim(v, rDigits) != "" {
		return false
	}
	before := strings.TrimRight(s[:a], " \t\"'“「\\")
	after := strings.TrimLeft(s[b:], " \t")
	sep := false
	for _, c := range keySeparators {
		if strings.HasSuffix(before, c) {
			sep = true
			break
		}
	}
	if !sep {
		return false
	}
	for _, u := range quantityUnits {
		if strings.HasPrefix(after, u) {
			return false
		}
	}
	return true
}

var identPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)*$`)

// notCode 排除代码里“命名”秘密的写法：secretKey: toBase64(…)、private_key: details.2,
func notCode(s string, a, b int) bool {
	if !identPath.MatchString(s[a:b]) {
		return true
	}
	rest := strings.TrimLeft(s[b:], " \t")
	if strings.HasPrefix(rest, "(") {
		return false
	}
	if a > 0 && (s[a-1] == '"' || s[a-1] == '\'') || !strings.HasSuffix(strings.TrimRight(s[:a], " \t"), ":") {
		return true
	}
	return rest == "" || !strings.ContainsRune(";,)}]|?&<>", rune(rest[0]))
}

func notAPlainKey(s string, a, b int) bool {
	i := a
	for i > 0 && strings.IndexByte(`"' `+"\t:=", s[i-1]) >= 0 {
		i--
	}
	end := i
	for i > 0 && strings.IndexByte(rAlnum+"_.-", s[i-1]) >= 0 {
		i--
	}
	name := strings.ToLower(s[i:end])
	for _, plain := range []string{"sort_key", "cache_key", "public_key", "primary_key", "foreign_key", "partition_key"} {
		if strings.HasSuffix(name, plain) {
			return false
		}
	}
	return true
}

func notPlainKeyOrCode(s string, a, b int) bool { return notAPlainKey(s, a, b) && notCode(s, a, b) }

// notASlug：小写单词以连字符相连的是文章地址（sk-hynix-third-quarter），不是密钥。
func notASlug(v string) bool {
	if strings.ContainsFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' }) {
		return true
	}
	if !strings.ContainsFunc(strings.TrimPrefix(v, "sk-"), func(r rune) bool {
		return r != '-' && r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) {
		return true
	}
	for _, w := range strings.FieldsFunc(v, func(r rune) bool { return r == '-' || r == '_' }) {
		if len(w) >= 16 {
			return true
		}
	}
	return false
}

// headerValue：Authorization/Cookie 行的值是凭据，而不是以该词开头的正文。
func headerValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || !notAVariable(v) {
		return false
	}
	notLetter := func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') }
	f := strings.Fields(v)
	if len(f) == 1 || len(f) == 2 && strings.IndexFunc(f[0], notLetter) < 0 {
		cred := f[len(f)-1]
		return len(cred) >= 8 && notAVariable(cred) && strings.IndexFunc(cred, notLetter) >= 0
	}
	for _, kv := range strings.Split(v, ";") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return false
		}
	}
	return strings.Contains(v, "=")
}

// hiddenInURL：URL 里的密码若同时作为用户名、主机或路径原样出现（postgres:postgres），
// 遮掉也藏不住，反而会把项目名当成密码，保持原样。
func hiddenInURL(s string, a, b int) bool {
	pw := s[a:b]
	i := strings.LastIndex(s[:a], "//")
	if i < 0 || s[a-1] != ':' {
		return true
	}
	if s[i+2:a-1] == pw {
		return false
	}
	end := b + 1
	for end < len(s) && !strings.ContainsRune(" \t\r\n\"'<>`\\)]}", rune(s[end])) {
		end++
	}
	if b+1 > end {
		return true
	}
	for _, part := range strings.FieldsFunc(s[b+1:end], func(r rune) bool { return strings.ContainsRune("/:.?#&=;,@", r) }) {
		if part == pw {
			return false
		}
	}
	return true
}

// inUserinfo：s[a] 位于 scheme:// 与主机之间（URL 的用户信息里）。
func inUserinfo(s string, a int) bool {
	i := a
	for i > 0 && !strings.ContainsRune(" \t\r\n/?#\"'<>@`", rune(s[i-1])) {
		i--
	}
	if i < 3 || s[i-1] != '/' || s[i-2] != '/' || s[i-3] != ':' {
		return false
	}
	j := i - 3
	k := j
	for k > 0 && (s[k-1] >= 'a' && s[k-1] <= 'z' || s[k-1] >= 'A' && s[k-1] <= 'Z' || s[k-1] >= '0' && s[k-1] <= '9' || strings.IndexByte("+.-", s[k-1]) >= 0) {
		k--
	}
	for k < j && !(s[k] >= 'a' && s[k] <= 'z' || s[k] >= 'A' && s[k] <= 'Z') {
		k++
	}
	return k < j
}

func emailAt(s string, a, b int) bool {
	if a > 0 && s[a-1] == '.' {
		return false
	}
	if b+1 < len(s) && s[b] == '.' && strings.IndexByte(rTokenCh, s[b+1]) >= 0 {
		return false
	}
	return !inUserinfo(s, a)
}

var linkerWords = map[string]bool{"plt": true, "got": true, "sec": true, "iplt": true, "igot": true}

var docFiles = map[string]bool{
	"agents.md": true, "claude.md": true, "gemini.md": true, "readme.md": true, "skill.md": true,
	"changelog.md": true, "contributing.md": true, "license.md": true, "lessons.md": true, "todo.md": true,
}

var fileExt = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "svg": true, "heic": true, "avif": true, "bmp": true,
	"ico": true, "pdf": true, "mp4": true, "webm": true, "mkv": true, "mp3": true, "wav": true, "ogg": true, "ttf": true,
	"otf": true, "woff": true, "woff2": true, "json": true, "yaml": true, "yml": true, "toml": true, "xml": true, "txt": true,
	"csv": true, "log": true, "html": true, "htm": true, "css": true, "scss": true, "js": true, "mjs": true, "cjs": true,
	"jsx": true, "ts": true, "tsx": true, "doc": true, "docx": true, "xls": true, "xlsx": true, "ppt": true, "pptx": true,
	"gz": true, "tgz": true, "tar": true, "rar": true, "7z": true, "dmg": true, "go": true, "py": true, "rs": true,
}

// 常见可收信顶级域；两字母国家/地区代码一律接受。不引入 publicsuffix 依赖，
// 避免 x@pytest.fixture 这类装饰器写法被当成邮箱。
var mailTLDs = map[string]bool{
	"com": true, "net": true, "org": true, "edu": true, "gov": true, "mil": true, "int": true, "info": true, "biz": true,
	"name": true, "pro": true, "mobi": true, "asia": true, "xyz": true, "top": true, "site": true, "online": true,
	"tech": true, "cloud": true, "vip": true, "club": true, "shop": true, "store": true, "app": true, "dev": true,
	"page": true, "email": true, "live": true, "life": true, "world": true, "today": true, "link": true, "space": true,
	"website": true, "blog": true, "news": true, "art": true, "design": true, "studio": true, "network": true,
	"systems": true, "solutions": true, "company": true, "group": true, "agency": true, "digital": true, "media": true,
	"team": true, "work": true, "zone": true, "center": true, "fun": true, "ltd": true, "inc": true, "llc": true,
	"icu": true, "wang": true, "ren": true, "red": true, "kim": true, "pub": true, "run": true, "host": true,
	"press": true, "mail": true, "family": true, "games": true, "city": true, "global": true, "plus": true,
	"local": true, "internal": true, "lan": true, "corp": true, "home": true, "intranet": true, "localdomain": true,
}

func realEmail(v string) bool {
	at := strings.LastIndexByte(v, '@')
	if at <= 0 || strings.EqualFold(v[:at], "git") || strings.HasSuffix(v[:at], "@") {
		return false
	}
	host := strings.ToLower(v[at+1:])
	if !strings.Contains(host, ".") || docFiles[host] {
		return false
	}
	labels := strings.Split(host, ".")
	linker := true
	for _, l := range labels {
		if l == "" {
			return false
		}
		if !linkerWords[l] {
			linker = false
		}
	}
	if linker {
		return false
	}
	tld := labels[len(labels)-1]
	if fileExt[tld] || !(len(tld) == 2 || mailTLDs[tld]) {
		return false
	}
	for _, fake := range []string{"example.com", "example.org", "example.net", "localhost", ".test", ".invalid", ".example"} {
		if host == strings.TrimPrefix(fake, ".") || strings.HasSuffix(host, fake) {
			return false
		}
	}
	return !strings.HasPrefix(host, "noreply") && !strings.Contains(strings.ToLower(v), "noreply@")
}

func chineseID(v string) bool {
	w := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(v[i]-'0') * w[i]
	}
	return strings.ToUpper(v[17:]) == string("10X98765432"[sum%11])
}

func luhn(v string) bool {
	var ds []int
	for _, r := range v {
		if r >= '0' && r <= '9' {
			ds = append(ds, int(r-'0'))
		}
	}
	if len(ds) < 15 || len(ds) > 19 {
		return false
	}
	sum := 0
	for i := range ds {
		d := ds[len(ds)-1-i]
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// 编号前的词：订单号 13800138000 是编号不是手机号（magpie phone.go 的简化版）。
var phoneIDWords = []string{"订单号", "订单", "编号", "单号", "流水号", "工单", "order", "id", "no.", "no", "#", "uid", "seq", "count", "total"}

func phoneAt(s string, a, b int) bool {
	v := s[a:b]
	if strings.HasPrefix(v, "+86") || strings.HasPrefix(v, "86") && len(v) > 11 {
		return true
	}
	before := strings.ToLower(strings.TrimRight(s[max(0, a-24):a], " \t:=：\"'_-"))
	for _, w := range phoneIDWords {
		if strings.HasSuffix(before, w) {
			return false
		}
	}
	return true
}

// ---- 占位符 ---------------------------------------------------------------------

var redactStore = struct {
	sync.RWMutex
	key    []byte
	values map[string]string // 占位符 → 原值
	used   map[string]uint64
	kinds  map[string]bool
	clock  uint64
}{values: map[string]string{}, used: map[string]uint64{}, kinds: map[string]bool{}}

const redactMaxValues = 100_000

// redactMaxKind 是占位符类型名的最大长度（TERM、PRIVATE_KEY、BANK_CARD…）。
const redactMaxKind = 16

var redactB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func redactKey() []byte {
	redactStore.RLock()
	k := redactStore.key
	redactStore.RUnlock()
	if k != nil {
		return k
	}
	redactStore.Lock()
	defer redactStore.Unlock()
	if redactStore.key == nil {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		redactStore.key = b
	}
	return redactStore.key
}

// redactTag 是值的占位符，不登记（预览用）。
func redactTag(kind, v string) string {
	h := hmac.New(sha256.New, redactKey())
	h.Write([]byte(kind + "\x00" + v))
	return "{{" + kind + "_" + strings.ToLower(redactB32.EncodeToString(h.Sum(nil))[:8]) + "}}"
}

// redactPlaceholder 生成并登记占位符，供回复还原。
func redactPlaceholder(kind, v string) string {
	p := redactTag(kind, v)
	redactStore.Lock()
	if _, ok := redactStore.values[p]; !ok {
		if len(redactStore.values) >= redactMaxValues {
			ps := make([]string, 0, len(redactStore.values))
			for x := range redactStore.values {
				ps = append(ps, x)
			}
			sort.Slice(ps, func(i, j int) bool { return redactStore.used[ps[i]] < redactStore.used[ps[j]] })
			for _, x := range ps[:len(ps)/2] {
				delete(redactStore.values, x)
				delete(redactStore.used, x)
			}
		}
		redactStore.values[p] = v
		redactStore.kinds[kind] = true
	}
	redactStore.clock++
	redactStore.used[p] = redactStore.clock
	redactStore.Unlock()
	return p
}

var redactPlaceholderRe = regexp.MustCompile(`\{\{[A-Z][A-Z0-9_]*_[a-z2-7]{8}\}\}`)

// ---- 文本脱敏 -------------------------------------------------------------------

type redactSpan struct {
	start, end int
	kind       string
}

// redactText 把 o 覆盖的内容换成 put 生成的占位符，命中追加到 hits。
func redactText(s string, o redactOptions, put func(kind, v string) string, hits *[]redactHit) string {
	if len(s) < 3 {
		return s
	}
	var found []redactSpan
	for _, w := range o.words {
		for i := 0; ; {
			j := strings.Index(s[i:], w)
			if j < 0 {
				break
			}
			found = append(found, redactSpan{i + j, i + j + len(w), "TERM"})
			i += j + len(w)
		}
	}
	if o.secrets || o.personal {
		for i := range redactRules {
			r := &redactRules[i]
			if r.personal && !o.personal || !r.personal && !o.secrets {
				continue
			}
			found = append(found, r.spans(s)...)
		}
	}
	if len(found) == 0 {
		return s
	}
	// 已有占位符内不再替换
	var existing []redactSpan
	if strings.Contains(s, "{{") {
		for _, m := range redactPlaceholderRe.FindAllStringIndex(s, -1) {
			existing = append(existing, redactSpan{m[0], m[1], ""})
		}
	}
	// 先出现者优先，同起点长者优先；重叠者丢弃
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end-found[i].start > found[j].end-found[j].start
	})
	var b strings.Builder
	last := 0
	for _, f := range found {
		if f.start < last || f.end <= f.start {
			continue
		}
		inside := false
		for _, e := range existing {
			if f.start < e.end && e.start < f.end {
				inside = true
				break
			}
		}
		if inside {
			continue
		}
		p := put(f.kind, s[f.start:f.end])
		b.WriteString(s[last:f.start])
		b.WriteString(p)
		last = f.end
		if hits != nil {
			head, n := redactHint(f.kind, s[f.start:f.end])
			*hits = append(*hits, redactHit{Kind: f.kind, Placeholder: p, Head: head, Len: n})
		}
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func (r *redactRule) spans(s string) []redactSpan {
	if len(r.markers) > 0 {
		hit := false
		for _, m := range r.markers {
			if strings.Contains(s, m) {
				hit = true
				break
			}
		}
		if !hit {
			return nil
		}
	}
	var out []redactSpan
	for _, m := range r.re.FindAllStringSubmatchIndex(s, -1) {
		a, b := m[0], m[1]
		if len(m) >= 4 && m[2] >= 0 {
			a, b = m[2], m[3]
		}
		if a >= b {
			continue
		}
		if r.bound != "" {
			if a > 0 && strings.IndexByte(r.bound, s[a-1]) >= 0 || b < len(s) && strings.IndexByte(r.bound, s[b]) >= 0 {
				continue
			}
		}
		v := s[a:b]
		if r.ok != nil && !r.ok(v) || r.at != nil && !r.at(s, a, b) {
			continue
		}
		out = append(out, redactSpan{a, b, r.kind})
	}
	return out
}

// ---- 请求体（Chat Completions JSON）脱敏 ----------------------------------------

// 整棵子树原样发送：工具定义、输出格式和多媒体数据（base64 会误中密钥规则）。
var redactSkipSubtree = map[string]bool{
	"tools": true, "tool_choice": true, "response_format": true, "stream_options": true, "functions": true,
	"image_url": true, "input_audio": true, "file": true, "file_data": true, "image": true,
	"audio": true, "video_url": true,
}

// 这些字段是标识或枚举，不是用户内容。
var redactSkipValue = map[string]bool{
	"model": true, "role": true, "type": true, "id": true, "tool_call_id": true, "call_id": true, "name": true,
	"object": true, "finish_reason": true, "url": true, "detail": true, "format": true, "data": true,
}

var errRedactFailed = errors.New("请求脱敏失败：请求体不是有效 JSON，为避免原文外发，未发送上游")

// redactSegment 是上游实际收到的一段文字（已脱敏，只含占位符），供脱敏记录展示。
type redactSegment struct {
	Where string `json:"where"` // 在请求中的位置，如 messages[3].content (user)
	Text  string `json:"text"`
}

// redactResult 是一次请求体脱敏的结果；不含任何原值。
type redactResult struct {
	hits     []redactHit
	segments []redactSegment
	model    string
}

const (
	redactSegmentMax      = 6000  // 单段超过此长度时只保留占位符附近的文字
	redactSegmentWindow   = 240   // 占位符两侧各保留的字节数
	redactRecordTextLimit = 64000 // 单条记录保留的文字总量
	redactRecordSegments  = 60
)

// maskRequestJSON 返回脱敏后的请求体和结果；没有命中时原样返回 body、结果为 nil。
func maskRequestJSON(body []byte, o redactOptions) ([]byte, *redactResult, error) {
	if !o.active() {
		return body, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, nil, errRedactFailed
	}
	res := &redactResult{}
	if m, ok := v.(map[string]any); ok {
		res.model, _ = m["model"].(string)
	}
	v = redactWalk(v, "", "", "", o, res)
	if len(res.hits) == 0 {
		return body, nil, nil
	}
	out, err := marshalNoHTMLEscape(v)
	if err != nil {
		return nil, nil, errRedactFailed
	}
	return out, res, nil
}

// redactWalk 遍历请求体；path 是当前位置，role 是所在消息的角色（用于记录展示）。
func redactWalk(v any, key, path, role string, o redactOptions, res *redactResult) any {
	switch x := v.(type) {
	case map[string]any:
		if r, ok := x["role"].(string); ok {
			role = r
		}
		for k, item := range x {
			if redactSkipSubtree[k] {
				continue
			}
			p := k
			if path != "" {
				p = path + "." + k
			}
			x[k] = redactWalk(item, k, p, role, o, res)
		}
		return x
	case []any:
		for i, item := range x {
			x[i] = redactWalk(item, key, path+"["+strconv.Itoa(i)+"]", role, o, res)
		}
		return x
	case string:
		if redactSkipValue[key] {
			return x
		}
		before := len(res.hits)
		var out string
		if key == "arguments" {
			out = redactJSONText(x, o, redactPlaceholder, &res.hits)
		} else {
			out = redactText(x, o, redactPlaceholder, &res.hits)
		}
		if len(res.hits) > before && len(res.segments) < redactRecordSegments {
			where := path
			if role != "" {
				where += " (" + role + ")"
			}
			res.segments = append(res.segments, redactSegment{Where: where, Text: redactExcerpt(out)})
		}
		return out
	default:
		return v
	}
}

// redactExcerpt 保留长文本中占位符附近的文字，其余以 … 省略。
func redactExcerpt(s string) string {
	if len(s) <= redactSegmentMax {
		return s
	}
	var spans [][2]int
	for _, m := range redactPlaceholderRe.FindAllStringIndex(s, -1) {
		a, b := max(0, m[0]-redactSegmentWindow), min(len(s), m[1]+redactSegmentWindow)
		if n := len(spans); n > 0 && a <= spans[n-1][1] {
			spans[n-1][1] = b
			continue
		}
		spans = append(spans, [2]int{a, b})
	}
	var out strings.Builder
	for i, sp := range spans {
		a, b := sp[0], sp[1]
		for a > 0 && !utf8.RuneStart(s[a]) {
			a--
		}
		for b < len(s) && !utf8.RuneStart(s[b]) {
			b++
		}
		if i == 0 && a > 0 || i > 0 {
			out.WriteString(" … ")
		}
		out.WriteString(s[a:b])
		if out.Len() > redactSegmentMax*2 {
			break
		}
	}
	if len(spans) > 0 && spans[len(spans)-1][1] < len(s) {
		out.WriteString(" …")
	}
	return out.String()
}

// redactJSONText 对工具调用参数这类“字符串里的 JSON”逐个脱敏字符串值，
// 保留原有键序与格式；不是 JSON 时按普通文本处理。
func redactJSONText(s string, o redactOptions, put func(kind, v string) string, hits *[]redactHit) string {
	if !json.Valid([]byte(s)) {
		return redactText(s, o, put, hits)
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != '"' {
			if s[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(s) {
			break
		}
		lit := s[i : j+1]
		k := j + 1
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
			k++
		}
		isKey := k < len(s) && s[k] == ':'
		if !isKey {
			var val string
			if json.Unmarshal([]byte(lit), &val) == nil {
				if masked := redactText(val, o, put, hits); masked != val {
					b.WriteString(s[last:i])
					b.WriteString(jsonQuote(masked))
					last = j + 1
				}
			}
		}
		i = j
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func jsonQuote(s string) string {
	out, _ := marshalNoHTMLEscape(s)
	return string(out)
}

// jsonEscape 是 s 写进 JSON 字符串时的内容（不含两侧引号）。
func jsonEscape(s string) string {
	q := jsonQuote(s)
	return q[1 : len(q)-1]
}

// redactPreview 按设置脱敏一段文本，但不登记任何原值；设置全关时按“密钥+个人信息”预览。
func redactPreview(s string) (string, []redactHit, bool) {
	o := currentRedactOptions()
	off := false
	if !o.active() {
		o.secrets, o.personal, off = true, true, true
	}
	var hits []redactHit
	out := redactText(s, o, redactTag, &hits)
	return out, hits, off
}

// ---- 还原（移植自 magpie Restore / partialTail） ---------------------------------

// 模型不一定按原样写回占位符：会写成 {{ PHONE_b5puzbfv }}、丢一个或两个花括号、改大小写。
// 只要剩下的部分是本进程生成过的占位符（类型 + 8 位摘要），一律还原。
var redactPlaceholderBody = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*_[A-Za-z2-7]{8}`)

func redactRestore(s string, escaped bool) string { return redactRestoreCount(s, escaped, nil) }

// redactRestoreCount 同 redactRestore，并在 n 不为 nil 时累加还原的占位符个数。
func redactRestoreCount(s string, escaped bool, n *int) string {
	if !strings.Contains(s, "_") {
		return s
	}
	redactStore.RLock()
	defer redactStore.RUnlock()
	if len(redactStore.values) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range redactPlaceholderBody.FindAllStringIndex(s, -1) {
		a, z := m[0], m[1]
		if a < last || a > 0 && isWordByte(s[a-1]) || z < len(s) && isWordByte(s[z]) {
			continue
		}
		v, ok := redactStore.values[redactCanonical(s[a:z])]
		if !ok {
			continue
		}
		a, z = redactBraces(s, a, z)
		if a < last {
			continue
		}
		b.WriteString(s[last:a])
		if escaped {
			v = jsonEscape(v)
		}
		b.WriteString(v)
		last = z
		if n != nil {
			*n++
		}
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func redactCanonical(body string) string {
	cut := len(body) - 8
	return "{{" + strings.ToUpper(body[:cut]) + strings.ToLower(body[cut:]) + "}}"
}

func isWordByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

func redactBraces(s string, a, z int) (int, int) {
	la, l := a, 0
	for la > 0 && (s[la-1] == ' ' || s[la-1] == '\t') {
		la--
	}
	for l < 2 && la > 0 && s[la-1] == '{' {
		la--
		l++
	}
	rz, r := z, 0
	for rz < len(s) && (s[rz] == ' ' || s[rz] == '\t') {
		rz++
	}
	for r < 2 && rz < len(s) && s[rz] == '}' {
		rz++
		r++
	}
	if l == 2 || l == 1 && r >= 1 {
		a = la
	}
	if r == 2 || r == 1 && l >= 1 {
		z = rz
	}
	return a, z
}

const redactMaxPlaceholder = len("{{") + redactMaxKind + len("_") + 8 + len("}}")

// redactPartialTail 是 s 末尾可能由下一个流式分片补全成占位符的长度。
func redactPartialTail(s string) int {
	if n := redactBraceTail(s); n > 0 {
		return n
	}
	return redactWordTail(s)
}

func redactBraceTail(s string) int {
	i := strings.LastIndexByte(s, '{')
	if i < 0 {
		return 0
	}
	if i > 0 && s[i-1] == '{' {
		i--
	}
	t := s[i:]
	if len(t) >= redactMaxPlaceholder+4 || strings.Contains(t, "}}") {
		return 0
	}
	body := strings.TrimLeft(strings.TrimLeft(t, "{"), " \t")
	if body == "" {
		return len(t)
	}
	if c := body[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
		return 0
	}
	j := 0
	for j < len(body) && isWordByte(body[j]) {
		j++
	}
	rest := strings.TrimLeft(body[j:], " \t")
	if rest != "" && rest != "}" {
		return 0
	}
	if j < len(body) && redactPlaceholderBody.FindString(body[:j]) != body[:j] {
		return 0
	}
	return len(t)
}

func redactWordTail(s string) int {
	i := len(s)
	for i > 0 && isWordByte(s[i-1]) {
		i--
	}
	w := s[i:]
	if w == "" || len(w) > redactMaxKind+9 || !(w[0] >= 'A' && w[0] <= 'Z') {
		return 0
	}
	u := strings.ToUpper(w)
	redactStore.RLock()
	defer redactStore.RUnlock()
	for k := range redactStore.kinds {
		k += "_"
		if strings.HasPrefix(k, u) {
			return len(w)
		}
		if strings.HasPrefix(u, k) && len(u)-len(k) <= 8 && strings.Trim(strings.ToLower(u[len(k):]), "abcdefghijklmnopqrstuvwxyz234567") == "" {
			return len(w)
		}
	}
	return 0
}

// ---- 流式回复还原 ---------------------------------------------------------------

// redactRestoreBody 逐行读取上游 SSE，把 delta 里的占位符换回原值。
// 占位符可能被拆在多个分片里：每个分片末尾“可能是占位符开头”的部分先扣下，
// 与下一个分片合并后再还原；遇到 finish_reason、字段切换、[DONE] 或 EOF 时补发。
type redactRestoreBody struct {
	src      io.ReadCloser
	br       *bufio.Reader
	out      []byte
	err      error
	held     map[string]*redactHeld
	meta     map[string]any
	rec      *redactRecord // 可为 nil；关闭时把还原次数记到这条脱敏记录
	restored int
	closed   sync.Once
}

type redactHeld struct {
	choice int
	field  string // content / reasoning_content / tool
	tool   int
	text   string
}

func newRedactRestoreBody(src io.ReadCloser, rec *redactRecord) *redactRestoreBody {
	return &redactRestoreBody{src: src, br: bufio.NewReaderSize(src, 64*1024), held: map[string]*redactHeld{}, rec: rec}
}

func (b *redactRestoreBody) Close() error {
	b.closed.Do(func() { noteRedactRestored(b.rec, b.restored) })
	return b.src.Close()
}

func (b *redactRestoreBody) restore(s string, escaped bool) string {
	return redactRestoreCount(s, escaped, &b.restored)
}

func (b *redactRestoreBody) Read(p []byte) (int, error) {
	for len(b.out) == 0 && b.err == nil {
		line, err := b.br.ReadBytes('\n')
		if len(line) > 0 {
			b.out = append(b.out, b.processLine(line)...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				b.out = append(b.out, b.flushAll("data: ")...)
			}
			b.err = err
		}
	}
	if len(b.out) == 0 {
		return 0, b.err
	}
	n := copy(p, b.out)
	b.out = b.out[n:]
	return n, nil
}

func (b *redactRestoreBody) processLine(line []byte) []byte {
	s := string(line)
	body := strings.TrimRight(s, "\r\n")
	eol := s[len(body):]
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") {
		return line
	}
	// 保留原有前缀（含重复 data:、裸 JSON），只替换 JSON 部分。
	k := 0
	for {
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if strings.HasPrefix(body[k:], "data:") {
			k += len("data:")
			continue
		}
		break
	}
	prefix, payload := body[:k], body[k:]
	if strings.TrimSpace(payload) == "[DONE]" {
		p := prefix
		if p == "" {
			p = "data: "
		}
		return append([]byte(b.flushAll(p)), line...)
	}
	if !strings.HasPrefix(payload, "{") {
		return []byte(b.restore(body, true) + eol)
	}
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var chunk map[string]any
	if dec.Decode(&chunk) != nil || chunk == nil {
		return []byte(b.restore(body, true) + eol)
	}
	if b.transformChunk(chunk) {
		if encoded, err := marshalNoHTMLEscape(chunk); err == nil {
			payload = string(encoded)
		}
	}
	return []byte(prefix + b.restore(payload, true) + eol)
}

func choiceIndex(c map[string]any) int {
	switch v := c["index"].(type) {
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case float64:
		return int(v)
	}
	return 0
}

func heldKey(choice int, field string, tool int) string {
	return strconv.Itoa(choice) + "/" + field + "/" + strconv.Itoa(tool)
}

// restoreField 合并扣下的尾巴、还原，并按需扣下新的尾巴。
func (b *redactRestoreBody) restoreField(choice int, field string, tool int, text string, final bool) string {
	key := heldKey(choice, field, tool)
	if h := b.held[key]; h != nil {
		text = h.text + text
		delete(b.held, key)
	}
	out := b.restore(text, field == "tool")
	if final {
		return out
	}
	if n := redactPartialTail(out); n > 0 {
		b.held[key] = &redactHeld{choice: choice, field: field, tool: tool, text: out[len(out)-n:]}
		out = out[:len(out)-n]
	}
	return out
}

func (b *redactRestoreBody) transformChunk(chunk map[string]any) bool {
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint"} {
		if v, ok := chunk[k]; ok {
			if b.meta == nil {
				b.meta = map[string]any{}
			}
			b.meta[k] = v
		}
	}
	choices, _ := chunk["choices"].([]any)
	changed := false
	for _, item := range choices {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		idx := choiceIndex(c)
		fr, _ := c["finish_reason"].(string)
		final := fr != ""
		delta, ok := c["delta"].(map[string]any)
		if !ok {
			if final && b.heldFor(idx) {
				delta = map[string]any{}
				c["delta"] = delta
			} else {
				continue
			}
		}
		present := map[string]bool{}
		for _, field := range []string{"content", "reasoning_content"} {
			if v, ok := delta[field].(string); ok {
				present[heldKey(idx, field, 0)] = true
				if out := b.restoreField(idx, field, 0, v, final); out != v {
					delta[field] = out
					changed = true
				}
			}
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, ci := range calls {
				call, ok := ci.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				args, ok := fn["arguments"].(string)
				if !ok {
					continue
				}
				t := choiceIndex(call)
				present[heldKey(idx, "tool", t)] = true
				if out := b.restoreField(idx, "tool", t, args, final); out != args {
					fn["arguments"] = out
					changed = true
				}
			}
		}
		// 本分片转向别的字段或已结束：先前扣下的尾巴不会再被补全，补发到本分片。
		if len(present) > 0 || final || len(delta) > 0 {
			for _, key := range b.sortedHeld(idx) {
				if present[key] {
					continue
				}
				h := b.held[key]
				delete(b.held, key)
				b.injectHeld(delta, h)
				changed = true
			}
		}
	}
	return changed
}

func (b *redactRestoreBody) heldFor(choice int) bool {
	for _, h := range b.held {
		if h.choice == choice {
			return true
		}
	}
	return false
}

func (b *redactRestoreBody) sortedHeld(choice int) []string {
	var keys []string
	for k, h := range b.held {
		if choice < 0 || h.choice == choice {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func (b *redactRestoreBody) injectHeld(delta map[string]any, h *redactHeld) {
	if h.field == "tool" {
		calls, _ := delta["tool_calls"].([]any)
		delta["tool_calls"] = append(calls, map[string]any{"index": h.tool, "function": map[string]any{"arguments": h.text}})
		return
	}
	existing, _ := delta[h.field].(string)
	delta[h.field] = h.text + existing
}

// flushAll 在 [DONE] 或 EOF 前把仍扣着的尾巴作为一个补充分片发出。
func (b *redactRestoreBody) flushAll(prefix string) string {
	if len(b.held) == 0 {
		return ""
	}
	byChoice := map[int]map[string]any{}
	var order []int
	for _, key := range b.sortedHeld(-1) {
		h := b.held[key]
		delta := byChoice[h.choice]
		if delta == nil {
			delta = map[string]any{}
			byChoice[h.choice] = delta
			order = append(order, h.choice)
		}
		b.injectHeld(delta, h)
	}
	b.held = map[string]*redactHeld{}
	sort.Ints(order)
	chunk := map[string]any{}
	for k, v := range b.meta {
		chunk[k] = v
	}
	if _, ok := chunk["object"]; !ok {
		chunk["object"] = "chat.completion.chunk"
	}
	choices := make([]any, 0, len(order))
	for _, i := range order {
		choices = append(choices, map[string]any{"index": i, "delta": byChoice[i]})
	}
	chunk["choices"] = choices
	encoded, err := marshalNoHTMLEscape(chunk)
	if err != nil {
		return ""
	}
	return prefix + string(encoded) + "\n\n"
}
