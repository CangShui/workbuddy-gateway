package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDebugConfigTolerantValues(t *testing.T) {
	isolatedGatewayConfig(t)
	cases := []struct {
		name          string
		document      string
		enabled, full bool
	}{
		{"missing debug", `{}`, false, false},
		{"null debug", `{"debug":null}`, false, false},
		{"empty object", `{"debug":{}}`, false, false},
		{"empty string", `{"debug":""}`, false, false},
		{"array value", `{"debug":[]}`, false, false},
		{"empty fields", `{"debug":{"enabled":"","fullRequests":null}}`, false, false},
		{"string booleans", `{"debug":{"enabled":"true","fullRequests":"false"}}`, true, false},
		{"numeric booleans", `{"debug":{"enabled":1,"fullRequests":0}}`, true, false},
		{"boolean values", `{"debug":{"enabled":true,"fullRequests":true}}`, true, true},
		{"unknown debug field", `{"debug":{"enabled":false,"fullRequests":false,"future":"ignored"}}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseRuntimeConfig([]byte(tc.document))
			if err != nil {
				t.Fatalf("容错配置不应阻断启动: %v", err)
			}
			if c.Debug.Enabled != tc.enabled || c.Debug.FullRequests != tc.full {
				t.Fatalf("debug 解析错误: %+v，期望 enabled=%t fullRequests=%t", c.Debug, tc.enabled, tc.full)
			}
		})
	}
}

func TestMissingConfigDefaultsDebugOff(t *testing.T) {
	isolatedGatewayConfig(t)
	oldFull := debugFullRequests.Load()
	oldDebug := cfg.DebugEnabled
	t.Cleanup(func() {
		debugFullRequests.Store(oldFull)
		cfg.DebugEnabled = oldDebug
	})
	if err := os.Remove(runtimeConfigFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatalf("缺少 config.json 时不应阻断启动: %v", err)
	}
	if cfg.DebugEnabled || debugFullRequests.Load() {
		t.Fatal("缺少配置时完整请求日志必须默认关闭")
	}
}

func TestConfigExampleDeclaresDebugPlaceholders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.json"))
	if os.IsNotExist(err) {
		t.Skip("config.example.json 不在隔离回归目录，仓库内测试会覆盖该文件")
	}
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	debug, ok := document["debug"]
	if !ok {
		t.Fatal("config.example.json 缺少 debug 段")
	}
	for _, key := range []string{"enabled", "fullRequests"} {
		value, ok := debug[key]
		if !ok {
			t.Fatalf("config.example.json 缺少 debug.%s", key)
		}
		if string(value) != "false" {
			t.Fatalf("默认占位必须是 false: debug.%s=%s", key, value)
		}
	}
}
