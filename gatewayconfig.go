package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

const (
	defaultAPIPort = 8317
	defaultWebPort = 8316
)

// Key 按用户配置以明文落盘；APIKeyEnabled 只影响模型 API，绝不能关闭管理鉴权。
type gatewayFileConfig struct {
	APIPort       int    `json:"apiPort"`
	WebPort       int    `json:"webPort"`
	AdminKey      string `json:"adminKey"`
	APIKeyEnabled bool   `json:"apiKeyEnabled"`
	APIKey        string `json:"apiKey"`
}

type gatewayValidationError string

func (e gatewayValidationError) Error() string { return string(e) }

var (
	gatewayConfigMu sync.RWMutex // 串行配置写入及运行时 Key 切换；不持锁调用 HTTP handler
	gatewaySettings gatewayFileConfig
	errSetupDone    = errors.New("管理 Key 已配置；外部修改后请等待配置热加载并重新登录")
	errConfigChange = errors.New("管理 Key 已被外部修改，请等待配置热加载并使用新 Key 重新登录")
)

func normalizeGatewayConfig(g gatewayFileConfig) (gatewayFileConfig, error) {
	if g.APIPort == 0 {
		g.APIPort = defaultAPIPort
	}
	if g.WebPort == 0 {
		g.WebPort = defaultWebPort
	}
	if g.APIPort < 1 || g.APIPort > 65535 || g.WebPort < 1 || g.WebPort > 65535 {
		return g, gatewayValidationError("gateway.apiPort/webPort 必须为 1～65535，省略或 0 使用默认值")
	}
	if g.APIPort == g.WebPort {
		return g, gatewayValidationError("API 端口和 Web 端口不能相同")
	}
	for name, value := range map[string]string{"adminKey": g.AdminKey, "apiKey": g.APIKey} {
		for _, c := range value {
			if c < 33 || c > 126 {
				return g, gatewayValidationError(fmt.Sprintf("gateway.%s 只能包含非空白可打印 ASCII 字符", name))
			}
		}
	}
	if g.APIKeyEnabled && g.APIKey == "" {
		return g, gatewayValidationError("启用 API Key 校验时必须设置 gateway.apiKey")
	}
	return g, nil
}

func applyGatewayConfig(g gatewayFileConfig) error {
	g, err := normalizeGatewayConfig(g)
	if err != nil {
		return err
	}
	apiPort := g.APIPort
	if cfg.PortExplicit {
		apiPort = cfg.Port
	}
	if apiPort < 1 || apiPort > 65535 || cfg.WebUI && apiPort == g.WebPort {
		return errors.New("API 监听端口无效或与 Web 监听端口冲突")
	}
	gatewayConfigMu.Lock()
	defer gatewayConfigMu.Unlock()
	gatewaySettings = g
	cfg.Port, cfg.WebPort = apiPort, g.WebPort
	setGatewayKeysLocked(g)
	return nil
}

func setGatewayKeysLocked(g gatewayFileConfig) {
	registerSecrets(g.AdminKey, g.APIKey)
	cfg.AdminKey = g.AdminKey
	cfg.APIKey = ""
	if g.APIKeyEnabled {
		cfg.APIKey = g.APIKey
	}
}

func currentAPIKey() string {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	return cfg.APIKey
}

func currentAdminKey() string {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	return cfg.AdminKey
}

// readGatewayDocumentLocked 保留其他配置段，不将提示词/账号规则重新构造为默认值。
func readGatewayDocumentLocked() (map[string]json.RawMessage, gatewayFileConfig, []byte, error) {
	data, err := os.ReadFile(runtimeConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		g, _ := normalizeGatewayConfig(gatewayFileConfig{})
		return map[string]json.RawMessage{}, g, nil, nil
	}
	if err != nil {
		return nil, gatewayFileConfig{}, nil, err
	}
	typed, err := parseRuntimeConfig(data)
	if err != nil {
		return nil, gatewayFileConfig{}, nil, fmt.Errorf("配置文件不可解析，拒绝覆盖: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return nil, gatewayFileConfig{}, nil, errors.New("config.json 必须是 JSON 对象")
	}
	g, err := normalizeGatewayConfig(typed.Gateway)
	return doc, g, data, err
}

// writeConfigPreservingFile 原位写入以保留现有文件 ACL；失败时尽力恢复原文。
// 不创建含密钥的临时替换文件，不声称提供断电事务保证。
func writeConfigPreservingFile(data, previous []byte) error {
	f, err := os.OpenFile(runtimeConfigFile, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	write := func(content []byte) error {
		n, err := f.WriteAt(content, 0)
		if err == nil && n != len(content) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = f.Truncate(int64(len(content)))
		}
		if err == nil {
			err = f.Sync()
		}
		return err
	}
	if err := write(data); err != nil {
		if previous != nil {
			if restoreErr := write(previous); restoreErr != nil {
				return fmt.Errorf("配置写入失败: %v；恢复原文件也失败，请检查文件: %w", err, restoreErr)
			}
		}
		return err
	}
	return nil
}

func persistGatewayConfigLocked(doc map[string]json.RawMessage, g gatewayFileConfig, old []byte, trace string) error {
	raw, err := json.Marshal(g)
	if err != nil {
		return err
	}
	doc["gateway"] = raw
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	log.Printf("[管理配置] traceId=%s 阶段=写入配置 文件=%s API鉴权启用=%t Key已隐藏=true 说明=先落盘再切换运行时配置，其他配置段保留", trace, runtimeConfigFile, g.APIKeyEnabled)
	if err := writeConfigPreservingFile(append(data, '\n'), old); err != nil {
		log.Printf("[管理配置] traceId=%s 阶段=写入配置 结果=失败 原因=%v 业务影响=运行时配置未切换", trace, err)
		return err
	}
	gatewaySettings = g
	setGatewayKeysLocked(g)
	// 本函数只应用 gateway 段；强制下轮完整重载其他外部编辑的配置。
	runtimeHasAppliedHash = false
	log.Printf("[管理配置] traceId=%s 结果=保存成功 API鉴权启用=%t 说明=后续请求使用新鉴权状态，管理鉴权始终独立启用，监听端口不热切换", trace, g.APIKeyEnabled)
	return nil
}

func initializeAdminKey(trace string) (string, error) {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	defer gatewayConfigMu.Unlock()
	if cfg.AdminKey != "" {
		return "", errSetupDone
	}
	doc, g, old, err := readGatewayDocumentLocked()
	if err != nil {
		return "", err
	}
	if g.AdminKey != "" {
		return "", errSetupDone
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("生成管理 Key 失败: %w", err)
	}
	g.AdminKey = hex.EncodeToString(entropy[:]) // 精确 32 字符，128 位随机熵。
	registerSecrets(g.AdminKey)
	if err := persistGatewayConfigLocked(doc, g, old, trace); err != nil {
		return "", err
	}
	return g.AdminKey, nil
}

func saveAPIKeySettings(enabled bool, key, trace string) error {
	return saveGatewaySettings(&enabled, key, nil, nil, trace)
}

func saveGatewaySettings(enabled *bool, key string, apiPort, webPort *int, trace string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	defer gatewayConfigMu.Unlock()
	doc, g, old, err := readGatewayDocumentLocked()
	if err != nil {
		return err
	}
	if cfg.AdminKey == "" || g.AdminKey != cfg.AdminKey {
		return errConfigChange
	}
	if enabled != nil {
		g.APIKeyEnabled = *enabled
	}
	if apiPort != nil {
		g.APIPort = *apiPort
	}
	if webPort != nil {
		g.WebPort = *webPort
	}
	// 空输入表示保留已配置 Key；关闭校验不删除原 Key。
	if key != "" {
		g.APIKey = key
	}
	g, err = normalizeGatewayConfig(g)
	if err != nil {
		return err
	}
	effectiveAPIPort := g.APIPort
	if cfg.PortExplicit {
		effectiveAPIPort = cfg.Port
	}
	if cfg.WebUI && effectiveAPIPort == g.WebPort {
		return gatewayValidationError("Web 端口不能与实际 API 端口冲突（含 -port 命令行覆盖）")
	}
	log.Printf("[管理端口设置] traceId=%s 配置API端口=%d 配置Web端口=%d 当前API端口=%d 当前Web端口=%d 说明=保存等效于编辑gateway配置，监听端口重启后改变",
		trace, g.APIPort, g.WebPort, cfg.Port, cfg.WebPort)
	registerSecrets(g.APIKey)
	return persistGatewayConfigLocked(doc, g, old, trace)
}

func gatewaySettingsView() map[string]any {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	return map[string]any{
		"apiKeyEnabled":      cfg.APIKey != "",
		"apiKeyConfigured":   gatewaySettings.APIKey != "",
		"adminKeyConfigured": cfg.AdminKey != "",
		"apiPort":            cfg.Port, "webPort": cfg.WebPort,
		"configuredApiPort": gatewaySettings.APIPort,
		"configuredWebPort": gatewaySettings.WebPort,
		"apiPortOverridden": cfg.PortExplicit,
		"restartRequired":   gatewaySettings.WebPort != cfg.WebPort || !cfg.PortExplicit && gatewaySettings.APIPort != cfg.Port,
	}
}

// ignoreLegacyAPIKeyFlag 只是吞掉旧 systemd 单元/脚本里残留的 -api-key，避免升级后启动失败。
//
// 该参数在新版本中**完全静默、不产生任何作用**：不写日志、不写盘、不修改 config.json、
// 不影响模型 API 鉴权；模型 API 鉴权一律只由 config.json 的 gateway.apiKeyEnabled / apiKey 决定。
//
// TODO(remove): 这是纯兼容占位，计划在后续版本彻底删除该参数与 main.go 中的注册代码；
// 删除后旧启动命令会因未知参数而启动失败，届时应在发布说明中提示用户先移除该参数。
func ignoreLegacyAPIKeyFlag(_ string) {
}
