package main

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"
)

// -----------------------------------------------------------------------------
// 出站身份还原（identity.responseRestore，默认关闭）
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
// 字节数。超过该长度的异常目标不启用还原，避免用截断后的目标误判边界。
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
	settings := identitySnapshot()
	if !settings.responseRestore || !settings.enabled || len(settings.restoreTargets) == 0 {
		return
	}
	target := settings.target
	var rules []identityRestoreRule
	for _, original := range originalClientIdentitiesWithRules(body, settings.restoreTargets) {
		rules = append(rules, identityRestoreRule{from: target, to: original})
	}
	if len(rules) == 0 {
		return
	}
	maxFrom := 0
	for _, rule := range rules {
		if len(rule.from) > maxFrom {
			maxFrom = len(rule.from)
		}
	}
	// 目标超过上限时宁可不还原，也不能拿截断的 target 做前缀判断。
	if maxFrom == 0 || maxFrom > identityRestoreMaxTailBytes {
		return
	}
	plan := identityRestore{rules: rules, hold: maxFrom}
	for _, rule := range rules {
		if rule.from != "" {
			plan.firstBytes[rule.from[0]] = true
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), identityRestoreContextKey{}, plan))
	debugEvent(r, "debug", "identity_response_restore_planned", map[string]any{
		"targets":         len(plan.rules),
		"hold_bytes":      plan.hold,
		"prompt_logged":   false,
		"business_impact": "仅还原响应正文中的目标身份文本，入站与出站请求不变",
	})
}

// originalClientIdentities 只从原始入站请求的 instructions、system、developer
// 位置收集客户端身份原文（按出现顺序去重）。其他角色、content 中非文本块、
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
		client := clientIdentitiesInText(rules, text)
		if len(client) == 0 {
			continue
		}
		// 还原计划需要的是客户端身份名本身，不是完整系统提示行；用规则表把
		// 这一行映射回它包含的身份名（例如 "You are Claude Code." -> "Claude Code"）。
		for _, name := range clientIdentitiesInLine(rules, client[0]) {
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

// clientIdentitiesInText 找出指令原文里的客户端身份：被改写成 target 的客户端名
// （例如 "Claude Code"）出现的那一行完整文本就是客户端身份原文。
// 像 WorkBuddy 这样的产品名可能本来就在客户端提示词里，不能据此断定客户端身份。
// 同一行出现多个不同的身份名时整行作废，避免把上游注入的身份句当成客户端身份。
func clientIdentitiesInText(rules []identityRestoreRule, text string) []string {
	names := identityNameRules(rules)
	if len(names) == 0 {
		return nil
	}
	var found []string
	for _, line := range strings.Split(text, "\n") {
		ids := clientIdentitiesInLine(names, line)
		if len(ids) == 0 {
			continue
		}
		found = append(found, line)
	}
	return found
}

// clientIdentitiesInLine 用一次最长匹配扫描提取行内实际身份名，避免 "Grok Build"
// 被 "Grok" 规则二次命中而误判成多身份。同一行出现多个不同身份名时返回空。
func clientIdentitiesInLine(rules []identityRestoreRule, line string) []string {
	var found []string
	for scan := 0; scan < len(line); {
		best := -1
		bestEnd := scan
		bestFrom := ""
		for _, rule := range rules {
			for from := scan; ; {
				at := strings.Index(line[from:], rule.from)
				if at < 0 {
					break
				}
				start := from + at
				end := start + len(rule.from)
				from = start + 1
				if !identityMatchBoundary(line, start, end) {
					continue
				}
				if best < 0 || start < best || (start == best && end > bestEnd) {
					best, bestEnd, bestFrom = start, end, rule.from
				}
				break
			}
		}
		if best < 0 {
			break
		}
		if len(found) > 0 && found[0] != bestFrom {
			return nil
		}
		found = append(found, bestFrom)
		scan = bestEnd
	}
	return found
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
	plan   identityRestore
	carry  []byte // 上一条分片留下的尾部，最多为目标长度加一个 rune
	active bool
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
	out, consumed := restoreStreamingPrefix(s.plan.rules, pending, cut)
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
	out := restoreIdentityText(s.plan.rules, string(s.carry))
	s.carry = s.carry[:0]
	return out
}

// restorePendingCut 找出尚未结束文本的初始处理边界：只有在当前文本末端仍可能是目标前缀的
// 位置才扣留，已经出现后继字节的完整目标可以立即判定并还原。
//
// 需要保留的情况只有两种：末尾是某条目标的真前缀，或末尾正好是完整目标。
// 同时保留目标前的一个字节，供下一片判断左边界；目标后一个字节不属于扣留
// 条件，因为一旦它已经到达，右边界就已经确定。hold 把扫描限制在一个目标长度内。
func restorePendingCut(text string, plan identityRestore) int {
	cut := len(text)
	scanStart := len(text) - plan.hold
	if scanStart < 0 {
		scanStart = 0
	}
	for start := scanStart; start < len(text); start++ {
		if !plan.firstBytes[text[start]] {
			continue
		}
		suffix := text[start:]
		for _, rule := range plan.rules {
			// HasPrefix 同时覆盖真前缀和完整目标；目标后面已经有字节时
			// suffix 会变长，不能再被当作未决前缀。
			if !strings.HasPrefix(rule.from, suffix) {
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

// restoreStreamingPrefix 只处理 limit 以内的命中，同时允许用 limit 后面的一个
// 字节判断右边界。跨过 limit 的目标整体留给下一片；已经到达边界但右边界
// 尚未出现的目标也会被保留，避免流式路径把未完成的目标提前替换。
func restoreStreamingPrefix(rules []identityRestoreRule, text string, limit int) (string, int) {
	if limit <= 0 {
		return "", 0
	}
	var b strings.Builder
	started := false
	cursor := 0
	for {
		match, found := matchIdentityTarget(rules, cursor, text)
		if !found {
			break
		}
		if match.start >= limit {
			break
		}
		if match.end > limit || match.end == len(text) {
			// 目标跨过当前处理边界，或末尾仍缺少右边界；从目标起点开始暂存。
			if match.start > cursor {
				if !started {
					return text[:match.start], match.start
				}
				b.WriteString(text[cursor:match.start])
			}
			return b.String(), match.start
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
// 字节原样保留；首尾边界按「是否被标识符字符包围」判断，与流式逐片判定一致。
// cursor 从 0 开始，因此只要发生过替换就必然写入过前缀，用它区分「无命中」。
func restoreIdentityText(rules []identityRestoreRule, text string) string {
	var b strings.Builder
	cursor := 0
	for {
		match, found := matchIdentityTarget(rules, cursor, text)
		if !found {
			break
		}
		b.WriteString(text[cursor:match.start])
		b.WriteString(match.to)
		cursor = match.end
	}
	if cursor == 0 {
		return text
	}
	b.WriteString(text[cursor:])
	return b.String()
}
