package main

import (
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// runtimeIdentityConfig controls text replacements applied when normalizing
// system/developer/assistant text. An absent config uses the built-in
// WorkBuddy identity and channel-compatibility replacements.
type runtimeIdentityConfig struct {
	Enabled      *bool             `json:"enabled,omitempty"`
	Target       string            `json:"target,omitempty"`
	Replacements map[string]string `json:"replacements,omitempty"`
	// ResponseRestore 是默认关闭的实验功能。开启后只改写响应可见正文：把出站时被替换掉的
	// 客户端身份还原回来。入站请求处理与出站请求体完全不受影响。
	ResponseRestore *bool `json:"responseRestore,omitempty"`
}

type identityReplacement struct {
	Old string
	New string
	// Only the exact token (.*) has wildcard meaning. All other bytes are literal.
	Wildcard bool
	Prefix   string
	Suffix   string
}

type identitySettings struct {
	enabled      bool
	target       string
	replacements []identityReplacement
	invalidRules int
	// restoreTargets 只在实验开关开启时建立，包含替换值等于 target 的字面身份规则。
	responseRestore bool
	restoreTargets  []identityRestoreRule
}

var (
	identityConfigMu sync.RWMutex
	currentIdentity  = defaultIdentitySettings()
)

const (
	identityWildcardToken    = "(.*)"
	identityWildcardMaxRunes = 256
)

// Used only by one-time default completion to preserve the old configured target.
// This key has no special meaning in the request matching engine.
const defaultDSHIdentityText = "You are an AI agent powered by DeepSeek Harness"

func defaultIdentityReplacements(target string) map[string]string {
	// config.example.json is the single source of default phrase data. Do not
	// maintain a second client-specific rule list in Go matching functions.
	var template struct {
		Identity struct {
			Target       string            `json:"target"`
			Replacements map[string]string `json:"replacements"`
		} `json:"identity"`
	}
	replacements := make(map[string]string)
	if err := json.Unmarshal(embeddedDefaultRuntimeConfig, &template); err != nil {
		log.Printf("[默认替换规则] 结果=未启用默认规则 原因=内置配置模板解析失败 说明=不记录模板正文")
		return replacements
	}
	for old, newValue := range template.Identity.Replacements {
		if template.Identity.Target != "" && target != template.Identity.Target {
			newValue = strings.ReplaceAll(newValue, template.Identity.Target, target)
		}
		replacements[old] = newValue
	}
	return replacements
}

func defaultIdentitySettings() identitySettings {
	return buildIdentitySettings(defaultIdentityConfig())
}

func defaultIdentityConfig() runtimeIdentityConfig {
	enabled := true
	responseRestore := false
	return runtimeIdentityConfig{
		Enabled: &enabled, Target: "WorkBuddy",
		Replacements:    defaultIdentityReplacements("WorkBuddy"),
		ResponseRestore: &responseRestore,
	}
}

func setIdentityConfig(c runtimeIdentityConfig) {
	settings := buildIdentitySettings(c)
	identityConfigMu.Lock()
	currentIdentity = settings
	identityConfigMu.Unlock()
}

func buildIdentitySettings(c runtimeIdentityConfig) identitySettings {
	enabled := true
	if c.Enabled != nil {
		enabled = *c.Enabled
	}
	target := strings.TrimSpace(c.Target)
	if target == "" {
		target = "WorkBuddy"
	}
	configured := c.Replacements
	if len(configured) == 0 {
		configured = defaultIdentityReplacements(target)
	}
	replacements := make([]identityReplacement, 0, len(configured))
	invalid := 0
	for old, newValue := range configured {
		rule, ok := compileIdentityReplacement(old, newValue)
		if !ok {
			invalid++
			continue
		}
		replacements = append(replacements, rule)
	}
	// More fixed text first; exact literals win ties. Deterministic ordering
	// also retains the existing longest-first behavior for literal rules.
	sort.SliceStable(replacements, func(i, j int) bool {
		a, b := replacements[i], replacements[j]
		an, bn := len(a.Old), len(b.Old)
		if a.Wildcard {
			an -= len(identityWildcardToken)
		}
		if b.Wildcard {
			bn -= len(identityWildcardToken)
		}
		if an != bn {
			return an > bn
		}
		if a.Wildcard != b.Wildcard {
			return !a.Wildcard
		}
		return a.Old < b.Old
	})
	responseRestore := c.ResponseRestore != nil && *c.ResponseRestore
	var restoreRules []identityRestoreRule
	if enabled && responseRestore {
		restoreRules = buildIdentityRestoreRules(target, replacements)
	}
	return identitySettings{
		enabled: enabled, target: target, replacements: replacements, invalidRules: invalid,
		responseRestore: responseRestore, restoreTargets: restoreRules,
	}
}

// buildIdentityRestoreRules 从已编译的替换规则导出身份名对照：from 是客户端身份名
// （例如 "Claude Code"），to 是它入站时被改写成的值（identity.target）；
// 生成请求还原计划时把二者对调。
// 通配规则没有固定的客户端身份；拼写修正等其他替换值不含 target，入站时没有被
// 改写，逆向替换会产生错误事实。两类规则都不参与还原。
// 同一客户端名只保留一条规则，结果按客户端名长度降序排列。
func buildIdentityRestoreRules(target string, rules []identityReplacement) []identityRestoreRule {
	out := make([]identityRestoreRule, 0, len(rules))
	seenName := make(map[string]bool)
	for _, rule := range rules {
		value := strings.TrimSpace(rule.New)
		if rule.Wildcard || value != target || seenName[rule.Old] {
			continue
		}
		seenName[rule.Old] = true
		out = append(out, identityRestoreRule{from: rule.Old, to: value, exactName: true})
	}
	return sortRestoreRules(out)
}

func sortRestoreRules(rules []identityRestoreRule) []identityRestoreRule {
	sort.SliceStable(rules, func(i, j int) bool {
		if len(rules[i].from) != len(rules[j].from) {
			return len(rules[i].from) > len(rules[j].from)
		}
		return rules[i].from < rules[j].from
	})
	return rules
}

func compileIdentityReplacement(old, newValue string) (identityReplacement, bool) {
	rule := identityReplacement{Old: old, New: newValue}
	if strings.TrimSpace(old) == "" {
		return rule, false
	}
	switch strings.Count(old, identityWildcardToken) {
	case 0:
		return rule, true
	case 1:
		rule.Prefix, rule.Suffix, _ = strings.Cut(old, identityWildcardToken)
		if strings.TrimSpace(rule.Prefix) == "" || strings.TrimSpace(rule.Suffix) == "" ||
			strings.ContainsAny(old, "\r\n") {
			return rule, false
		}
		rule.Wildcard = true
		return rule, true
	default:
		return rule, false
	}
}

func identitySnapshot() identitySettings {
	identityConfigMu.RLock()
	snapshot := currentIdentity
	snapshot.replacements = append([]identityReplacement(nil), snapshot.replacements...)
	snapshot.restoreTargets = append([]identityRestoreRule(nil), snapshot.restoreTargets...)
	identityConfigMu.RUnlock()
	return snapshot
}

func identitySettingsSummary() (bool, string, int) {
	settings := identitySnapshot()
	return settings.enabled, settings.target, len(settings.replacements)
}

func identityRuleDetails() (literal, wildcard, invalid int) {
	settings := identitySnapshot()
	for _, rule := range settings.replacements {
		if rule.Wildcard {
			wildcard++
		} else {
			literal++
		}
	}
	return literal, wildcard, settings.invalidRules
}

// Limited anchored matching, not a regular expression. The first suffix wins,
// the dynamic part cannot cross a line or exceed 256 Unicode characters, and
// everything after the matched suffix is preserved byte for byte.
func (rule identityReplacement) replace(text string) (string, int) {
	if !rule.Wildcard {
		count := strings.Count(text, rule.Old)
		if count == 0 {
			return text, 0
		}
		return strings.ReplaceAll(text, rule.Old, rule.New), count
	}
	if !strings.HasPrefix(text, rule.Prefix) {
		return text, 0
	}
	remainder := text[len(rule.Prefix):]
	limit := 4*identityWildcardMaxRunes + len(rule.Suffix)
	if len(remainder) > limit {
		remainder = remainder[:limit]
	}
	if newline := strings.IndexAny(remainder, "\r\n"); newline >= 0 {
		remainder = remainder[:newline]
	}
	end := strings.Index(remainder, rule.Suffix)
	if end < 0 || utf8.RuneCountInString(remainder[:end]) > identityWildcardMaxRunes {
		return text, 0
	}
	if rule.New == rule.Old {
		// Mapping an automatically completed default to itself disables that
		// rule without reinserting the literal wildcard token into the prompt.
		return text, 1
	}
	return rule.New + text[len(rule.Prefix)+end+len(rule.Suffix):], 1
}

func (settings identitySettings) normalize(text string, allowWildcard bool) (string, int, int) {
	if !settings.enabled {
		return text, 0, 0
	}
	literal, wildcard := 0, 0
	if allowWildcard {
		// Choose at most one wildcard against the ORIGINAL block. Never run it
		// against a literal rule's output, where a later suffix could eat prose.
		original := text
		for _, rule := range settings.replacements {
			if !rule.Wildcard {
				if strings.HasPrefix(original, rule.Old) {
					break // A more specific exact prefix, including a no-op, wins.
				}
				continue
			}
			if rewritten, n := rule.replace(original); n > 0 {
				text, wildcard = rewritten, n
				break
			}
		}
	}
	for _, rule := range settings.replacements {
		if !rule.Wildcard {
			var n int
			text, n = rule.replace(text)
			literal += n
		}
	}
	return text, literal, wildcard
}

// normalizeAssistantIdentityText applies the configurable prefix and
// display-name replacements. It is called only for system/developer/assistant
// text; user/tool content is intentionally outside this function.
func normalizeAssistantIdentityText(text string) (string, bool) {
	original := text
	text, _, _ = identitySnapshot().normalize(text, true)
	return text, text != original
}
