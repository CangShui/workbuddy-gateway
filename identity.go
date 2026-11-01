package main

import (
	"sort"
	"strings"
	"sync"
)

// runtimeIdentityConfig controls text replacements applied when normalizing
// system/developer/assistant text. An absent config uses the built-in
// WorkBuddy identity and channel-compatibility replacements.
type runtimeIdentityConfig struct {
	Enabled      *bool             `json:"enabled,omitempty"`
	Target       string            `json:"target,omitempty"`
	Replacements map[string]string `json:"replacements,omitempty"`
}

type identityReplacement struct {
	Old string
	New string
}

type identitySettings struct {
	enabled      bool
	prefix       string
	replacements []identityReplacement
}

var (
	defaultIdentityTerms = []string{"Claude Code", "Claude", "DeepSeek Harness", "Codex", "ZCode", "Grok Build", "Grok", "Grok released by xAI"}
	identityConfigMu     sync.RWMutex
	currentIdentity      = defaultIdentitySettings()
)

const (
	defaultWorkspaceRootPhrase = "workspace root"
	defaultWorkspaceBasePhrase = "workspace base"
	defaultUserPathsPhrase     = "are paths the user"
	defaultUserPathsNeutral    = "are paths that the user"
)

func defaultIdentityReplacements(target string) map[string]string {
	replacements := make(map[string]string, len(defaultIdentityTerms)+2)
	for _, term := range defaultIdentityTerms {
		replacements[term] = target
	}
	replacements[defaultWorkspaceRootPhrase] = defaultWorkspaceBasePhrase
	replacements[defaultUserPathsPhrase] = defaultUserPathsNeutral
	return replacements
}

func defaultIdentitySettings() identitySettings {
	return buildIdentitySettings(defaultIdentityConfig())
}

func defaultIdentityConfig() runtimeIdentityConfig {
	enabled := true
	return runtimeIdentityConfig{
		Enabled: &enabled, Target: "WorkBuddy",
		Replacements: defaultIdentityReplacements("WorkBuddy"),
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
	prefixTarget := target
	replacements := make([]identityReplacement, 0, len(c.Replacements))
	if len(c.Replacements) > 0 {
		for old, newValue := range c.Replacements {
			if strings.TrimSpace(old) == "" {
				continue
			}
			replacements = append(replacements, identityReplacement{Old: old, New: newValue})
		}
		if replacement, ok := c.Replacements["DeepSeek Harness"]; ok && strings.TrimSpace(replacement) != "" {
			prefixTarget = replacement
		}
	} else {
		for old, newValue := range defaultIdentityReplacements(target) {
			replacements = append(replacements, identityReplacement{Old: old, New: newValue})
		}
	}
	// Longest first avoids splitting "Claude Code" into "Assistant Code".
	sort.SliceStable(replacements, func(i, j int) bool {
		if len(replacements[i].Old) != len(replacements[j].Old) {
			return len(replacements[i].Old) > len(replacements[j].Old)
		}
		return replacements[i].Old < replacements[j].Old
	})
	return identitySettings{
		enabled: enabled, prefix: "You are " + prefixTarget, replacements: replacements,
	}
}

func identitySnapshot() identitySettings {
	identityConfigMu.RLock()
	snapshot := currentIdentity
	snapshot.replacements = append([]identityReplacement(nil), currentIdentity.replacements...)
	identityConfigMu.RUnlock()
	return snapshot
}

func identitySettingsSummary() (bool, string, int) {
	settings := identitySnapshot()
	target := strings.TrimPrefix(settings.prefix, "You are ")
	return settings.enabled, target, len(settings.replacements)
}

// normalizeAssistantIdentityText applies the configurable prefix and
// display-name replacements. It is called only for system/developer/assistant
// text; user/tool content is intentionally outside this function.
func normalizeAssistantIdentityText(text string) (string, bool) {
	settings := identitySnapshot()
	if !settings.enabled {
		return text, false
	}
	original := text
	for _, prefix := range blockedSystemPromptPrefixes {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			text = settings.prefix + text[len(prefix):]
			break
		}
	}
	text = normalizeGrokBuildIdentityPrefix(text, settings.replacements)
	for _, replacement := range settings.replacements {
		text = strings.ReplaceAll(text, replacement.Old, replacement.New)
	}
	return text, text != original
}

// Grok Build 可按模型设置身份标签；固定来源句和配置规则共同限定匹配范围。
func normalizeGrokBuildIdentityPrefix(text string, replacements []identityReplacement) string {
	body, ok := strings.CutPrefix(text, "You are ")
	if !ok {
		return text
	}
	label, suffix, ok := strings.Cut(body, " released by xAI.")
	if !ok || strings.TrimSpace(label) == "" || strings.ContainsAny(label, "\r\n!?") ||
		strings.Contains(label, ". ") || strings.Contains(label, ".\t") {
		return text
	}
	if suffix != "" && !strings.ContainsRune(" \t\r\n", rune(suffix[0])) {
		return text
	}
	identity := body[:len(label)+len(" released by xAI")]
	target, enabled := "", false
	for _, replacement := range replacements {
		if replacement.Old == identity {
			return text // 完整身份词条交给现有替换流程，保留用户配置的优先级。
		}
		if replacement.Old == "Grok released by xAI" {
			target, enabled = replacement.New, replacement.New != replacement.Old
		}
	}
	if enabled {
		return "You are " + target + "." + suffix
	}
	return text
}
