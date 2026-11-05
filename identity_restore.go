package main

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// -----------------------------------------------------------------------------
// 实验性出站身份还原（identity.responseRestore，默认关闭）
//
// 入站时网关把客户端身份改写成配置的 identity.target；上游因此可能在回复的
// 任意可见文本中输出目标身份。开启本功能后，按可配置 target 做子串级还原：
// 命中后若前后紧贴 ASCII 标识符则跳过，以避免破坏代码/标识符。
//   - 客户端身份只从本次入站原始请求的 instructions / system / developer 位置推断；
//   - 还原后的文本取自 identity.replacements，不硬编码任何客户端名称；
//   - 入站请求转换、出站请求体、工具参数与协议结构均不改动。
//
// 状态全部挂在请求 context 上，请求结束即失效，不存在跨请求或会话状态。
// -----------------------------------------------------------------------------

// identityRestoreMaxTailBytes 是单条流等待「被分片切开的目标文本」时最多缓存的
// 目标长度上限。实际尾部另保留边界 rune 与一个域名后缀判断字节。
const identityRestoreMaxTailBytes = 4096

type identityRestoreRule struct {
	// from 是响应里要替换的精确文本（入站时写入的 identity.target），
	// to 是还原后的客户端原文。配置导出的规则里 from 是客户端身份名、
	// to 是它被改写成的值，二者在生成请求计划时对调。
	from string
	to   string
	// exactName 表示该规则来自配置，from/to 是「客户端身份名/改写值」方向，
	// 只有这类规则能用于从指令中推断客户端身份。
	exactName bool
}

type identityRestoreContextKey struct{}

// identityRestore 是一次请求内的还原计划。空规则表示不参与还原。
// hold 是最长目标文本的字节数，也是流式扫描末端的最大回看窗口。
type identityRestore struct {
	rules      []identityRestoreRule
	hold       int
	firstBytes [256]bool
}

func (p identityRestore) active() bool { return len(p.rules) > 0 }

// identityRestoreFor 返回当前请求的还原计划；未开启、未推断出客户端身份或
// 没有可还原目标时返回空计划。请求处理过程中只读，重复调用结果一致。
func identityRestoreFor(r *http.Request) identityRestore {
	if r == nil {
		return identityRestore{}
	}
	plan, _ := r.Context().Value(identityRestoreContextKey{}).(identityRestore)
	return plan
}

// planIdentityRestore 在读完入站原始请求、任何转换之前调用一次，随后把计划
// 写回请求 context，供三个协议的响应出口使用。同一请求重复调用不重新推断。
func planIdentityRestore(r *http.Request, body map[string]any) {
	if r == nil || identityRestoreFor(r).active() {
		return
	}
	identityConfigMu.RLock()
	enabled := currentIdentity.responseRestore && currentIdentity.enabled
	identityConfigMu.RUnlock()
	if !enabled {
		return
	}
	settings := identityStateForRequest(r).settings
	if !settings.responseRestore || !settings.enabled {
		return
	}
	target := settings.target
	originals := originalClientIdentitiesWithRules(body, settings.restoreTargets)
	if len(originals) != 1 {
		log.Printf("[响应身份还原] traceId=%s 阶段=身份判定 实验功能=true 明确身份数=%d 结果=跳过 原因=未找到唯一身份声明 业务影响=响应原样保留，不记录提示词或身份名称", debugTraceID(r), len(originals))
		debugEvent(r, "debug", "identity_response_restore_skipped", map[string]any{
			"identity_count": len(originals), "reason": "no_unique_declaration",
		})
		return
	}
	rules := []identityRestoreRule{{from: target, to: originals[0]}}
	maxFrom := len(target)
	// 目标超过上限时宁可不还原，也不能拿截断的 target 做前缀判断。
	if maxFrom == 0 || maxFrom > identityRestoreMaxTailBytes {
		log.Printf("[响应身份还原] traceId=%s 阶段=计划校验 结果=跳过 目标字节数=%d 上限=%d 业务影响=响应原样保留", debugTraceID(r), maxFrom, identityRestoreMaxTailBytes)
		return
	}
	plan := identityRestore{rules: rules, hold: maxFrom}
	for _, rule := range rules {
		if rule.from != "" {
			plan.firstBytes[rule.from[0]] = true
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), identityRestoreContextKey{}, plan))
	log.Printf("[响应身份还原] traceId=%s 阶段=计划建立 实验功能=true 明确身份数=1 目标字节数=%d 结果=启用 业务影响=仅还原正文和推理文本；保留地址、路径及工具参数，不记录正文", debugTraceID(r), plan.hold)
	debugEvent(r, "debug", "identity_response_restore_planned", map[string]any{
		"targets":         len(plan.rules),
		"hold_bytes":      plan.hold,
		"prompt_logged":   false,
		"business_impact": "仅还原响应正文中的目标身份文本，入站与出站请求不变",
	})
}

// originalClientIdentities 只从原始入站请求的 instructions、system、developer
// 位置收集明确的客户端身份声明（按出现顺序去重）。其他角色、content 中非文本块、
// 工具参数与推理字段都不参与推断。
func originalClientIdentities(body map[string]any) []string {
	return originalClientIdentitiesWithRules(body, identitySnapshot().restoreTargets)
}

func originalClientIdentitiesWithRules(body map[string]any, rules []identityRestoreRule) []string {
	if body == nil {
		return nil
	}
	var found []string
	for _, text := range identityInstructionTexts(body) {
		client, ambiguous := clientIdentitiesInText(rules, text)
		if ambiguous {
			return nil
		}
		for _, name := range client {
			duplicate := false
			for _, existing := range found {
				if existing == name {
					duplicate = true
					break
				}
			}
			if !duplicate {
				found = append(found, name)
			}
		}
	}
	return found
}

// identityInstructionTexts 取出请求中 instructions、system、developer 三类
// 指令位置的纯文本，覆盖 Chat、Responses、Anthropic Messages 三种入站形状。
func identityInstructionTexts(body map[string]any) []string {
	var texts []string
	texts = appendContentText(texts, body["instructions"])
	texts = appendContentText(texts, body["system"])
	texts = append(texts, instructionMessageTexts(body["messages"])...)
	texts = append(texts, instructionMessageTexts(body["input"])...)
	return texts
}

// instructionMessageTexts 处理 messages / input 数组，只保留 system/developer
// 角色的文本；其他角色、工具结果与推理字段都不属于身份来源。
func instructionMessageTexts(raw any) []string {
	list, _ := raw.([]any)
	var texts []string
	for _, itemAny := range list {
		message, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		role := roleOfMessage(message)
		if message["type"] == "message" && role == "" {
			// Responses input 中的消息项：{"type":"message","role":"system",...}
			role = roleOfMessage(map[string]any{"role": message["role"]})
		}
		switch role {
		case "system", "developer":
			texts = appendContentText(texts, message["content"])
		}
	}
	return texts
}

// appendContentText 收集字符串或文本块数组中的文字，忽略图片等非文本块。
func appendContentText(texts []string, content any) []string {
	switch value := content.(type) {
	case string:
		if value != "" {
			texts = append(texts, value)
		}
	case []any:
		for _, blockAny := range value {
			block, ok := blockAny.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text", "input_text":
				if text, ok := block["text"].(string); ok && text != "" {
					texts = append(texts, text)
				}
			}
		}
	}
	return texts
}

var identityDeclarationPrefix = regexp.MustCompile(`(?i)(?:^|[.!?。！？;；]\s*)(?:you are an AI agent powered by\s+|you are\s+|you're\s+|you’re\s+|i am\s+|i'm\s+|i’m\s+|we are\s+|你是\s*|您是\s*|我是\s*)`)

// 只接受行首或句首的自我身份声明；普通提及、否定指令、引用和围栏代码不作为身份来源。
// 名称必须紧随声明词，描述中的其他产品名不参与判定；并列/选择句式视为歧义。
func clientIdentitiesInText(rules []identityRestoreRule, text string) ([]string, bool) {
	names := identityNameRules(rules)
	if len(names) == 0 {
		return nil, false
	}
	var found []string
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(line, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fence = line[:3]
			continue
		}
		if strings.HasPrefix(line, ">") || strings.HasPrefix(line, "\"") ||
			strings.HasPrefix(line, "'") || strings.HasPrefix(line, "`") {
			continue
		}
		for _, prefix := range identityDeclarationPrefix.FindAllStringIndex(line, -1) {
			declaration := strings.TrimLeft(line[prefix[1]:], "`*_")
			name := ""
			for _, rule := range names {
				if len(rule.from) > len(name) && strings.HasPrefix(declaration, rule.from) &&
					identityMatchBoundary(declaration, 0, len(rule.from)) {
					name = rule.from
				}
			}
			if name == "" {
				continue
			}
			tail := strings.ToLower(strings.TrimLeft(declaration[len(name):], "`*_,， \t"))
			for _, connector := range []string{"or ", "and ", "/", "或", "、"} {
				if strings.HasPrefix(tail, connector) {
					return nil, true
				}
			}
			found = append(found, name)
		}
	}
	return found, false
}

// identityNameRules 返回计划中身份名对应的规则副本，用于在指令里定位客户端身份。
func identityNameRules(rules []identityRestoreRule) []identityRestoreRule {
	names := make([]identityRestoreRule, 0, len(rules))
	for _, rule := range rules {
		if rule.exactName {
			names = append(names, rule)
		}
	}
	return names
}

// identityMatch 是匹配到的一处目标文本；from 用于把命中对应回配置规则。
type identityMatch struct {
	start, end int
	from       string
	to         string
}

// matchIdentityTarget 按给定规则在 offset 起点之后找出最多一次目标出现。
// 位置最靠前者优先，同一位置取最长目标；被 ASCII 字母、数字或下划线包围的命中
// 一律跳过，因此 WorkBuddy2、my_WorkBuddy 这类标识符不会被改写。
func matchIdentityTarget(rules []identityRestoreRule, offset int, text string) (identityMatch, bool) {
	var best identityMatch
	found := false
	for _, rule := range rules {
		for from := offset; ; {
			at := strings.Index(text[from:], rule.from)
			if at < 0 {
				break
			}
			start := from + at
			end := start + len(rule.from)
			from = start + 1
			if !identityMatchBoundary(text, start, end) {
				continue // 标识符内部的出现跳过，继续找同一条规则的下一处。
			}
			if !found || start < best.start || (start == best.start && end > best.end) {
				best, found = identityMatch{start: start, end: end, from: rule.from, to: rule.to}, true
			}
			// strings.Index 已经返回该规则的最早可判定命中；继续向后搜不可能
			// 改善当前结果，只会把重复 target 的整段扫描放大成二次复杂度。
			break
		}
	}
	return best, found
}

func identityMatchBoundary(text string, start, end int) bool {
	if start > 0 && isASCIIIdentifierByte(text[start-1]) {
		return false
	}
	if end < len(text) && isASCIIIdentifierByte(text[end]) {
		return false
	}
	return true
}

// isASCIIIdentifierByte 判断字节是否为 ASCII 字母、数字或下划线。
func isASCIIIdentifierByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// -----------------------------------------------------------------------------
// 流式正文还原
//
// 每个被还原的文本通道（正文、推理正文）各持有一个 restorer。跨分片只保留
// 末尾「最早还可能变化」的一段字节，其余内容立即还原下发；因此流式输出的
// 拼接结果与把整段文本一次性还原完全一致，UTF-8 字节也不会被改写。
// -----------------------------------------------------------------------------

type identityRestorer struct {
	plan    identityRestore
	carry   []byte // 尾部最多为目标长度、边界 rune 和一个点号
	active  bool
	address identityAddressContext
}

// 只记已消费原文的地址状态，不缓存整条 URL 或路径。引号内路径允许包含空格。
// dot 表示刚收到“标识符.”，下一字节若仍为标识符则属于域名或文件名。
type identityAddressContext struct {
	path  bool
	dot   bool
	prev  byte
	quote rune
}

func (c *identityAddressContext) advance(text string) {
	for _, r := range text {
		if (r == '"' || r == '\'' || r == '`') &&
			(c.quote == r || c.quote == 0 && !isASCIIIdentifierByte(c.prev)) {
			if c.quote == 0 {
				c.quote = r
			} else if c.quote == r {
				*c = identityAddressContext{}
			}
			c.prev, c.dot = 0, false
			continue
		}
		if unicode.IsSpace(r) || strings.ContainsRune("<>，。！？；", r) {
			if c.quote == 0 || r == '\n' || r == '\r' {
				*c = identityAddressContext{}
			} else {
				c.prev, c.dot = 0, false
			}
			continue
		}
		id := r < utf8.RuneSelf && isASCIIIdentifierByte(byte(r))
		if c.dot && id {
			c.path = true
		}
		if r == '/' || r == '\\' || r == ':' || r == '@' {
			c.path = true
		}
		c.dot = r == '.' && isASCIIIdentifierByte(c.prev)
		c.prev = 0
		if r < utf8.RuneSelf {
			c.prev = byte(r)
		}
	}
}

func (c identityAddressContext) protects(text string, end int) bool {
	if c.path || c.prev == '.' || c.prev == '-' {
		return true
	}
	if end == len(text) {
		return false
	}
	switch text[end] {
	case '/', '\\', ':', '@', '-':
		return true
	case '.':
		return end+1 < len(text) && (isASCIIIdentifierByte(text[end+1]) || text[end+1] == '-')
	}
	return false
}

func newIdentityRestorer(plan identityRestore) *identityRestorer {
	return &identityRestorer{plan: plan, active: plan.active()}
}

// Write 处理一条分片：先与缓存拼接，再还原其中已经可以判定的部分。
func (s *identityRestorer) Write(delta string) string {
	if !s.active {
		return delta
	}
	pending := delta
	if len(s.carry) > 0 {
		pending = string(s.carry) + delta
	}
	s.carry = nil
	cut := restorePendingCut(pending, s.plan)
	if cut == len(pending) && cut > 0 {
		// 没有未决候选时也要保留最后一个 rune，作为下一片判断左边界的上下文。
		cut = lastRuneStart(pending)
	}
	cut = runeStartAtOrBefore(pending, cut)
	out, consumed := restoreIdentityPrefix(s.plan.rules, pending, cut, s.address, false)
	s.address.advance(pending[:consumed])
	if consumed < len(pending) {
		s.carry = append(s.carry, pending[consumed:]...)
	}
	return out
}

// Flush 在流结束时交出剩余尾部；目标文本被上游在该分片处截断时按原文下发。
func (s *identityRestorer) Flush() string {
	if !s.active || len(s.carry) == 0 {
		return ""
	}
	pending := string(s.carry)
	out, _ := restoreIdentityPrefix(s.plan.rules, pending, len(pending), s.address, true)
	s.carry = s.carry[:0]
	s.address = identityAddressContext{}
	return out
}

// restorePendingCut 找出尚未结束文本的初始处理边界：只有在当前文本末端仍可能是目标前缀的
// 位置才扣留，已经出现后继字节的完整目标可以立即判定并还原。
//
// 保留末尾的目标前缀、完整目标，以及“目标.”：后者要等待下个字节区分句号
// 和域名/文件后缀。同时保留目标前的边界上下文，扫描窗口为 hold 加一个字节。
func restorePendingCut(text string, plan identityRestore) int {
	cut := len(text)
	scanStart := len(text) - plan.hold - 1
	if scanStart < 0 {
		scanStart = 0
	}
	for start := scanStart; start < len(text); start++ {
		if !plan.firstBytes[text[start]] {
			continue
		}
		suffix := text[start:]
		for _, rule := range plan.rules {
			// HasPrefix 覆盖真前缀和完整目标；单独保留尚未判定的点号后缀。
			dottedTarget := len(suffix) == len(rule.from)+1 && suffix[len(suffix)-1] == '.' &&
				strings.HasPrefix(suffix, rule.from)
			if !strings.HasPrefix(rule.from, suffix) && !dottedTarget {
				continue
			}
			// 目标起点前的一个字节用于判断左边界。若它还没有出现在 pending
			// 中，Write 的无候选路径会至少保留上一片的最后一个 rune。
			candidateStart := start
			if candidateStart > 0 {
				candidateStart--
			}
			cut = minInt(cut, candidateStart)
		}
	}
	if cut < 0 {
		cut = 0
	}
	return cut
}

// restoreIdentityPrefix 只处理 limit 以内的命中，结合原文地址上下文跳过 URL、
// 域名和路径。流式调用保留未决目标；final 调用允许处理恰好位于文末的目标。
func restoreIdentityPrefix(rules []identityRestoreRule, text string, limit int, address identityAddressContext, final bool) (string, int) {
	if limit <= 0 {
		return "", 0
	}
	var b strings.Builder
	started := false
	cursor := 0
	scan := 0
	for {
		match, found := matchIdentityTarget(rules, scan, text)
		if !found {
			break
		}
		if match.start >= limit {
			break
		}
		if match.end > limit || !final && match.end == len(text) {
			// 目标跨过当前处理边界，或末尾仍缺少右边界；从目标起点开始暂存。
			if match.start > cursor {
				if !started {
					return text[:match.start], match.start
				}
				b.WriteString(text[cursor:match.start])
			}
			return b.String(), match.start
		}
		address.advance(text[scan:match.start])
		protected := address.protects(text, match.end)
		address.advance(text[match.start:match.end])
		scan = match.end
		if protected {
			continue
		}
		if !started {
			b.Grow(limit)
			started = true
		}
		b.WriteString(text[cursor:match.start])
		b.WriteString(match.to)
		cursor = match.end
	}
	if !started {
		return text[:limit], limit
	}
	b.WriteString(text[cursor:limit])
	return b.String(), limit
}

func lastRuneStart(text string) int {
	if len(text) == 0 {
		return 0
	}
	start := len(text) - 1
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	return start
}

func runeStartAtOrBefore(text string, index int) int {
	if index <= 0 {
		return 0
	}
	if index > len(text) {
		index = len(text)
	}
	for index > 0 && index < len(text) && !utf8.RuneStart(text[index]) {
		index--
	}
	return index
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// restoreIdentityText 用同一套匹配规则替换一段完整文本。命中之间未改动的
// 字节原样保留；标识符边界与地址保护规则与流式逐片判定一致。
func restoreIdentityText(rules []identityRestoreRule, text string) string {
	out, _ := restoreIdentityPrefix(rules, text, len(text), identityAddressContext{}, true)
	return out
}
