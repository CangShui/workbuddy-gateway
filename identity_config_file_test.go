package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRuntimeConfigFileCreatesCompleteDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Enabled == nil || !*c.Identity.Enabled || c.Identity.Target != "WorkBuddy" {
		t.Fatalf("default identity=%#v", c.Identity)
	}
	if c.Identity.ResponseRestore == nil || *c.Identity.ResponseRestore {
		t.Fatalf("new config must explicitly disable experimental response restore: %#v", c.Identity.ResponseRestore)
	}
	for _, term := range []string{"Claude Code", "Claude", "DeepSeek Harness", "Codex", "ZCode", "Grok Build", "Grok", "Grok released by xAI"} {
		if c.Identity.Replacements[term] != "WorkBuddy" {
			t.Fatalf("default replacement %q=%q", term, c.Identity.Replacements[term])
		}
	}
	if c.Identity.Replacements["workspace root"] != "workspace base" {
		t.Fatalf("default workspace replacement=%q", c.Identity.Replacements["workspace root"])
	}
	if c.Identity.Replacements["are paths the user"] != "are paths that the user" {
		t.Fatalf("default paths replacement=%q", c.Identity.Replacements["are paths the user"])
	}
}

func TestUpgradeAddsDisabledResponseRestore(t *testing.T) {
	complete := defaultIdentityConfig()
	complete.ResponseRestore = nil
	completeJSON, err := json.Marshal(complete)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		identity string
		want     bool
	}{
		{"missing_section", "", false},
		{"null_section", "null", false},
		{"empty_section", "{}", false},
		{"disabled_identity", `{"enabled":false,"target":"Assistant","replacements":{"Custom":"Assistant"}}`, false},
		{"complete_old_rules", string(completeJSON), false},
		{"explicit_false", `{"responseRestore":false}`, false},
		{"explicit_true", `{"responseRestore":true}`, true},
		{"case_insensitive", `{"ResponseRestore":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			doc := map[string]json.RawMessage{
				"systemPrompt": json.RawMessage(`{"force":"keep"}`),
				"concurrent":   json.RawMessage(`{"MaxAcc":7,"MaxGlobal":12}`),
			}
			if tc.identity != "" {
				doc["identity"] = json.RawMessage(tc.identity)
			}
			raw, _ := json.Marshal(doc)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ensureRuntimeConfigFile(path); err != nil {
				t.Fatal(err)
			}
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var c runtimeFileConfig
			if err := json.Unmarshal(first, &c); err != nil {
				t.Fatal(err)
			}
			if c.Identity.ResponseRestore == nil || *c.Identity.ResponseRestore != tc.want {
				t.Fatalf("responseRestore=%v want explicit %t", c.Identity.ResponseRestore, tc.want)
			}
			if c.SystemPrompt.Force != "keep" || c.Concurrent.MaxAcc != 7 || c.Concurrent.MaxGlobal != 12 {
				t.Fatalf("unrelated configuration changed: %s", first)
			}
			if tc.name == "disabled_identity" && (c.Identity.Enabled == nil || *c.Identity.Enabled ||
				c.Identity.Target != "Assistant" || c.Identity.Replacements["Custom"] != "Assistant") {
				t.Fatalf("custom identity changed: %s", first)
			}
			if err := ensureRuntimeConfigFile(path); err != nil {
				t.Fatal(err)
			}
			second, _ := os.ReadFile(path)
			if !bytes.Equal(first, second) {
				t.Fatalf("second startup changed config:\n%s\n%s", first, second)
			}
		})
	}
}

func TestEnsureRuntimeConfigFileMigratesOldConfigWithoutIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"gateway":{"apiPort":18317},"systemPrompt":{"force":"keep"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["identity"]; !ok {
		t.Fatal("identity was not added to old config")
	}
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Target != "WorkBuddy" || c.Gateway.APIPort != 18317 || c.SystemPrompt.Force != "keep" {
		t.Fatalf("old config was not preserved: %#v", c)
	}
}

func TestEnsureRuntimeConfigFilePreservesExistingIdentityAndAddsDefaultReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"identity":{"enabled":true,"target":"Assistant","replacements":{"kimi":"K2"}},"concurrent":{"MaxAcc":4,"MaxGlobal":0}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Target != "Assistant" || c.Identity.Replacements["kimi"] != "K2" {
		t.Fatalf("existing identity was overwritten: %#v", c.Identity)
	}
	if c.Identity.Replacements["workspace root"] != "workspace base" {
		t.Fatalf("missing default workspace replacement: %#v", c.Identity.Replacements)
	}
	if c.Identity.Replacements["are paths the user"] != "are paths that the user" {
		t.Fatalf("missing default paths replacement: %#v", c.Identity.Replacements)
	}
	for _, term := range []string{"Grok Build", "Grok", "Grok released by xAI"} {
		if c.Identity.Replacements[term] != "Assistant" {
			t.Fatalf("missing default replacement %q: %#v", term, c.Identity.Replacements)
		}
	}
}

func TestEnsureRuntimeConfigFilePreservesCustomWorkspaceReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"identity":{"enabled":true,"target":"WorkBuddy","replacements":{"workspace root":"custom base","Grok released by xAI":"Local Agent"}},"concurrent":{"MaxAcc":4,"MaxGlobal":0}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Replacements["workspace root"] != "custom base" || c.Identity.Replacements["Grok released by xAI"] != "Local Agent" {
		t.Fatalf("custom replacement overwritten: %#v", c.Identity.Replacements)
	}
	if c.Identity.Replacements["are paths the user"] != "are paths that the user" {
		t.Fatalf("missing paths replacement beside custom workspace rule: %#v", c.Identity.Replacements)
	}
}

func TestEnsureRuntimeConfigFileAddsMissingDefaultWithoutEnablingDisabledIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"identity":{"enabled":false,"target":"WorkBuddy","replacements":{"workspace root":"custom base"}},"concurrent":{"MaxAcc":4,"MaxGlobal":0}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Enabled == nil || *c.Identity.Enabled {
		t.Fatal("migration must not enable an explicitly disabled identity section")
	}
	if c.Identity.Replacements["workspace root"] != "custom base" ||
		c.Identity.Replacements["are paths the user"] != "are paths that the user" ||
		c.Identity.Replacements["Grok released by xAI"] != "WorkBuddy" {
		t.Fatalf("migration did not preserve/add expected replacements: %#v", c.Identity.Replacements)
	}
}

func TestEnsureRuntimeConfigFileAddsCompleteDefaultsWhenIdentityReplacementsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"identity":{"enabled":true,"target":"Assistant"},"concurrent":{"MaxAcc":4,"MaxGlobal":0}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Replacements["Codex"] != "Assistant" ||
		c.Identity.Replacements["workspace root"] != "workspace base" ||
		c.Identity.Replacements["are paths the user"] != "are paths that the user" {
		t.Fatalf("default identity replacements incomplete: %#v", c.Identity.Replacements)
	}
}

func TestIdentityConfigSupportsArbitraryReplacementKeys(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	enabled := true
	setIdentityConfig(runtimeIdentityConfig{Enabled: &enabled, Target: "WorkBuddy", Replacements: map[string]string{
		"kimi": "WorkBuddy",
	}})
	got, changed := normalizeAssistantIdentityText("kimi is the assistant")
	if !changed || got != "WorkBuddy is the assistant" {
		t.Fatalf("arbitrary replacement got=%q changed=%t", got, changed)
	}
}

func TestIdentityConfigHotReloadsFromDisk(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	write := func(target string) {
		data := []byte(`{"gateway":{"apiPort":18317,"webPort":18316},"identity":{"enabled":true,"target":"` + target + `"}}`)
		if err := os.WriteFile(runtimeConfigFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Assistant")
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if got, _ := normalizeAssistantIdentityText("You are Codex"); got != "You are Assistant" {
		t.Fatalf("first load got=%q", got)
	}
	write("WorkBuddy")
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if got, _ := normalizeAssistantIdentityText("You are Codex"); got != "You are WorkBuddy" {
		t.Fatalf("hot reload got=%q", got)
	}
}

func TestIdentityConfigEmptyValuesUseDefaultWithoutError(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Target: "", Replacements: map[string]string{}})
	got, changed := normalizeAssistantIdentityText("You are Codex")
	if !changed || got != "You are WorkBuddy" {
		t.Fatalf("empty identity config got=%q changed=%t", got, changed)
	}
	got, changed = normalizeAssistantIdentityText("workspace root")
	if !changed || got != "workspace base" {
		t.Fatalf("default workspace replacement got=%q changed=%t", got, changed)
	}
	got, changed = normalizeAssistantIdentityText("absolute paths are paths the user can use")
	if !changed || got != "absolute paths are paths that the user can use" {
		t.Fatalf("default paths replacement got=%q changed=%t", got, changed)
	}
}
