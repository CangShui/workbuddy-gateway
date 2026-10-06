package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate source file")
	}
	return filepath.Dir(file)
}

// jsonKeyPaths 递归收集 JSON 对象的字段路径，用于比较示例文件与运行时结构。
func jsonKeyPaths(value any, prefix string) []string {
	var out []string
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			out = append(out, path)
			out = append(out, jsonKeyPaths(item, path)...)
		}
	case []any:
		for _, item := range v {
			out = append(out, jsonKeyPaths(item, prefix)...)
		}
	}
	return out
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// 示例文件必须覆盖运行时结构的每个字段，避免新增配置项后用户照着示例部署却缺项。
func TestConfigExampleCoversEveryRuntimeField(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := json.Marshal(runtimeFileConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, path := range jsonKeyPaths(decodeObject(t, schema), "") {
		want[path] = true
	}
	got := map[string]bool{}
	for _, path := range jsonKeyPaths(decodeObject(t, raw), "") {
		got[path] = true
	}
	var missing []string
	for path := range want {
		if !got[path] {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("config.example.json 缺少配置项: %v", missing)
	}
	var extra []string
	for path := range got {
		if !want[path] {
			extra = append(extra, path)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Fatalf("config.example.json 含未定义配置项: %v", extra)
	}
}

// 全新部署：直接使用示例文件必须能正常启动，并落到默认端口。
func TestConfigExampleStartsFreshDeployment(t *testing.T) {
	isolatedGatewayConfig(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeConfigFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.PortExplicit = false
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatalf("示例配置无法加载: %v", err)
	}
	if cfg.Port != defaultAPIPort || cfg.WebPort != defaultWebPort {
		t.Fatalf("示例配置端口=%d/%d，期望 %d/%d", cfg.Port, cfg.WebPort, defaultAPIPort, defaultWebPort)
	}
	if currentAdminKey() != "" || currentAPIKey() != "" {
		t.Fatal("示例配置不应预置任何 Key")
	}
}

// 空值/缺省：端口为 0 或省略、Key 为空、开关为 false 时都必须按默认值启动。
func TestConfigEmptyValuesFallBackToDefaults(t *testing.T) {
	for name, content := range map[string]string{
		"gateway 全空值":  `{"gateway":{"apiPort":0,"webPort":0,"adminKey":"","apiKeyEnabled":false,"apiKey":""}}`,
		"gateway 空对象":  `{"gateway":{}}`,
		"完全省略 gateway": `{}`,
		"仅端口为 0":       `{"gateway":{"apiPort":0,"webPort":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolatedGatewayConfig(t)
			if err := os.WriteFile(runtimeConfigFile, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.PortExplicit = false
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatalf("空值配置必须能启动: %v", err)
			}
			if cfg.Port != defaultAPIPort || cfg.WebPort != defaultWebPort {
				t.Fatalf("端口=%d/%d，期望默认 %d/%d", cfg.Port, cfg.WebPort, defaultAPIPort, defaultWebPort)
			}
			if currentAPIKey() != "" || currentAdminKey() != "" {
				t.Fatal("空值配置不应启用任何鉴权")
			}
			// 空值配置下服务必须真的能起来（含 -webui 双端口）。
			cfg.WebUI = true
			cfg.Port, cfg.WebPort = freeTCPPort(t), freeTCPPort(t)
			_, listeners, err := prepareGatewayServers()
			if err != nil {
				t.Fatalf("空值配置监听失败: %v", err)
			}
			for _, listener := range listeners {
				_ = listener.Close()
			}
		})
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// 空的 Key 字符串必须被规范化为空值，不能被当成有效凭据。
func TestConfigBlankKeysAreTreatedAsUnset(t *testing.T) {
	isolatedGatewayConfig(t)
	content := `{"gateway":{"apiPort":8317,"webPort":8316,"adminKey":"","apiKeyEnabled":false,"apiKey":""}}`
	if err := os.WriteFile(runtimeConfigFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(currentAdminKey()) != "" || strings.TrimSpace(currentAPIKey()) != "" {
		t.Fatal("空 Key 不应被视为已配置")
	}
	if !reflect.DeepEqual(gatewaySettingsView()["apiKeyEnabled"], false) {
		t.Fatal("apiKeyEnabled=false 必须保持关闭")
	}
}
