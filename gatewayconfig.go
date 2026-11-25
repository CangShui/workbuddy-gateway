package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultAPIPort = 8317
	defaultWebPort = 8316
)

const (
	// legacyAPIKeyID 是旧单字段 apiKey 合成为数组记录时使用的稳定 ID。
	legacyAPIKeyID = "legacy"
	// legacyAPIKeyLabel 是旧单字段 apiKey 合成记录的显示名。
	legacyAPIKeyLabel = "默认"
	// maxGatewayAPIKeys 限制模型 Key 条数，避免配置文件被无限放大。
	maxGatewayAPIKeys = 64
)

// gatewayAPIKey 是 gateway.apiKeys 中的一条模型 API Key。
//
// 字段名必须是 apiKey：日志脱敏按字段名识别秘密（logredact.go 的 secretLogField），
// 改成 key/secret 之类会让 Key 原文进入日志与调试事件。
type gatewayAPIKey struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	APIKey    string `json:"apiKey"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// Key 按用户配置以明文落盘；APIKeyEnabled 只影响模型 API，绝不能关闭管理鉴权。
//
// 磁盘上 apiKeys 数组是唯一形态；旧单字段 apiKey 仅为读取兼容保留：
// 读取时若数组为空而 apiKey 非空，会在内存中合成一条 id=legacy 的记录（不写盘），
// 下一次真正写配置时才落成数组并把 apiKey 清空。
type gatewayFileConfig struct {
	APIPort       int             `json:"apiPort"`
	WebPort       int             `json:"webPort"`
	AdminKey      string          `json:"adminKey"`
	APIKeyEnabled bool            `json:"apiKeyEnabled"`
	APIKey        string          `json:"apiKey"`
	APIKeys       []gatewayAPIKey `json:"apiKeys,omitempty"`
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
		if err := validateGatewaySecret(name, value); err != nil {
			return g, err
		}
	}
	if len(g.APIKeys) > maxGatewayAPIKeys {
		return g, gatewayValidationError(fmt.Sprintf("gateway.apiKeys 最多允许 %d 条", maxGatewayAPIKeys))
	}
	for _, k := range g.APIKeys {
		if k.APIKey == "" {
			return g, gatewayValidationError("gateway.apiKeys[].apiKey 不能为空")
		}
		if err := validateGatewaySecret("apiKeys[].apiKey", k.APIKey); err != nil {
			return g, err
		}
		if err := validateGatewayAPIKeyLabel(k.Label); err != nil {
			return g, err
		}
	}
	g = migrateLegacyAPIKey(g)
	g.APIKeys = normalizeGatewayAPIKeyIDs(g.APIKeys)
	if g.APIKeyEnabled && len(g.APIKeys) == 0 {
		return g, gatewayValidationError("启用 API Key 校验时必须至少设置一条 gateway.apiKeys（旧字段 gateway.apiKey 仍可写入）")
	}
	return g, nil
}

// validateGatewaySecret 与旧行为一致：Key 只能是 33～126 的非空白可打印 ASCII。
func validateGatewaySecret(name, value string) error {
	for _, c := range value {
		if c < 33 || c > 126 {
			return gatewayValidationError(fmt.Sprintf("gateway.%s 只能包含非空白可打印 ASCII 字符", name))
		}
	}
	return nil
}

// validateGatewayAPIKeyLabel 只拦控制字符和超长标签，允许为空（前端显示为「未命名」）。
func validateGatewayAPIKeyLabel(label string) error {
	if len([]rune(label)) > 64 {
		return gatewayValidationError("gateway.apiKeys[].label 最多允许 64 个字符")
	}
	for _, c := range label {
		if c < 32 || c == 127 {
			return gatewayValidationError("gateway.apiKeys[].label 不能包含控制字符")
		}
	}
	return nil
}

// migrateLegacyAPIKey 在内存中把旧单字段 apiKey 合成一条记录，不写盘：
// 渠道热加载必须保持只读，迁移推迟到下一次真正的配置写入。
// 数组与旧字段同时存在时数组优先，旧字段被丢弃，避免同一条 Key 出现两次。
func migrateLegacyAPIKey(g gatewayFileConfig) gatewayFileConfig {
	if len(g.APIKeys) > 0 {
		g.APIKey = ""
		return g
	}
	if g.APIKey == "" {
		return g
	}
	g.APIKeys = []gatewayAPIKey{{ID: legacyAPIKeyID, Label: legacyAPIKeyLabel, APIKey: g.APIKey}}
	// 合成后清空旧字段：保留会让 gatewaySettings.APIKey 成为第二个真值来源，
	// 之后的读取方（设置视图、秘密注册）可能拿到陈旧值。
	g.APIKey = ""
	return g
}

// newGatewayAPIKey 生成一条新的 Key 记录；ID 与创建时间由服务端决定，不接受前端指定。
func newGatewayAPIKey(key, label string) gatewayAPIKey {
	if label == "" {
		label = "未命名"
	}
	return gatewayAPIKey{
		ID:        uuid.NewString(),
		Label:     label,
		APIKey:    key,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// apiKeyIDFor 为手写条目（缺 id）推导稳定 ID：同一 Key 每次加载得到同一 ID，
// 否则每次热加载 ID 都会变化，按 ID 删除/显示就失效了。
func apiKeyIDFor(key string) string {
	sum := sha256.Sum256([]byte("workbuddy-gateway/api-key/" + key))
	return hex.EncodeToString(sum[:8])
}

// normalizeGatewayAPIKeyIDs 补齐手写条目缺失的 id，并保证 id 唯一（重复时改用新的随机 id）。
func normalizeGatewayAPIKeyIDs(keys []gatewayAPIKey) []gatewayAPIKey {
	if len(keys) == 0 {
		return keys
	}
	seen := make(map[string]bool, len(keys))
	for i := range keys {
		id := keys[i].ID
		if id == "" {
			id = apiKeyIDFor(keys[i].APIKey)
		}
		for seen[id] {
			id = uuid.NewString()
		}
		seen[id] = true
		keys[i].ID = id
	}
	return keys
}

// gatewayAPIKeyIndex 返回指定 Key 值在数组中的下标，未找到返回 -1。
func gatewayAPIKeyIndex(keys []gatewayAPIKey, key string) int {
	for i := range keys {
		if keys[i].APIKey == key {
			return i
		}
	}
	return -1
}

// randomGatewaySecret 生成 32 字符十六进制随机串（16 字节 / 128 位熵）。
func randomGatewaySecret() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(entropy[:]), nil
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
	secrets := make([]string, 0, len(g.APIKeys)+2)
	secrets = append(secrets, g.AdminKey, g.APIKey)
	for _, k := range g.APIKeys {
		secrets = append(secrets, k.APIKey)
	}
	registerSecrets(secrets...)
	cfg.AdminKey = g.AdminKey
	cfg.APIKey = ""
	cfg.APIKeys = nil
	if g.APIKeyEnabled {
		cfg.APIKeys = make([]string, 0, len(g.APIKeys))
		for _, k := range g.APIKeys {
			if k.APIKey != "" {
				cfg.APIKeys = append(cfg.APIKeys, k.APIKey)
			}
		}
		if len(cfg.APIKeys) > 0 {
			// 兼容只读第一条 Key 的调用方（启动横幅、probe、旧测试）。
			cfg.APIKey = cfg.APIKeys[0]
		}
	}
}

func currentAPIKey() string {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	return cfg.APIKey
}

// currentAPIKeys 返回当前生效的全部模型 Key。
// cfg.APIKey 与 cfg.APIKeys 取并集：正常路径下前者是后者的第一条镜像，
// 但测试与旧代码会直接赋值 cfg.APIKey，并集能让两种写法都生效。
func currentAPIKeys() []string {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	return activeAPIKeysLocked()
}

func activeAPIKeysLocked() []string {
	keys := make([]string, 0, len(cfg.APIKeys)+1)
	if cfg.APIKey != "" {
		keys = append(keys, cfg.APIKey)
	}
	for _, k := range cfg.APIKeys {
		if k != "" && k != cfg.APIKey {
			keys = append(keys, k)
		}
	}
	return keys
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
	// 磁盘上只有 apiKeys 数组；旧字段 apiKey 视为只读输入，写盘时清空。
	written := g
	written.APIKey = ""
	raw, err := json.Marshal(written)
	if err != nil {
		return err
	}
	doc["gateway"] = raw
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	log.Printf("[管理配置] traceId=%s 阶段=写入配置 文件=%s API鉴权启用=%t Key条数=%d Key已隐藏=true 说明=先落盘再切换运行时配置，其他配置段保留", trace, runtimeConfigFile, g.APIKeyEnabled, len(written.APIKeys))
	if err := writeConfigPreservingFile(append(data, '\n'), old); err != nil {
		log.Printf("[管理配置] traceId=%s 阶段=写入配置 结果=失败 原因=%v 业务影响=运行时配置未切换", trace, err)
		return err
	}
	if err := applyGatewayListenerPortsLocked(g, trace); err != nil {
		if restoreErr := writeConfigPreservingFile(old, append(data, '\n')); restoreErr != nil {
			return fmt.Errorf("监听端口切换失败: %v；恢复原配置也失败，请检查服务器日志: %w", err, restoreErr)
		}
		return err
	}
	gatewaySettings = written
	setGatewayKeysLocked(written)
	// 本函数只应用 gateway 段；强制下轮完整重载其他外部编辑的配置。
	runtimeHasAppliedHash = false
	log.Printf("[管理配置] traceId=%s 结果=保存成功 API鉴权启用=%t 实际API端口=%d 实际Web端口=%d 说明=后续请求使用新鉴权状态，管理鉴权始终独立启用；监听端口已热切换，旧连接保留", trace, g.APIKeyEnabled, cfg.Port, cfg.WebPort)
	return nil
}

// Called with gatewayConfigMu held. New listeners bind before old ones retire.
func applyGatewayListenerPortsLocked(g gatewayFileConfig, trace string, beforeCommit ...func() error) error {
	apiPort := g.APIPort
	if cfg.PortExplicit {
		apiPort = cfg.Port
	}
	var prepare func() error
	if len(beforeCommit) > 0 {
		prepare = beforeCommit[0]
	}
	actualAPI, actualWeb, err := gatewayServers.applyWith(g, apiPort, cfg.WebUI, prepare)
	if err != nil {
		log.Printf("[监听热切换] traceId=%s 目标API端口=%d 目标Web端口=%d 结果=失败 原因=%v 业务影响=原监听继续服务", trace, apiPort, g.WebPort, err)
		return gatewayValidationError(err.Error())
	}
	if actualAPI > 0 {
		cfg.Port = actualAPI
	}
	if actualWeb > 0 {
		cfg.WebPort = actualWeb
	}
	return nil
}

func currentGatewayPorts() (int, int) {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	api, web, _ := gatewayServers.ports()
	return api, web
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
	// key 是旧版设置页提交的单 Key 字段，沿用旧版本语义：它表示「当前唯一的模型 Key」。
	// 同值重复提交保持不变（保留既有 id 与标签）；换值则整体替换列表，否则被轮换掉的 Key
	// 会静默继续可用。新页面不再提交该字段，这里只为兼容缓存中的旧控制台与旧脚本。
	if key != "" {
		g.APIKey = ""
		if gatewayAPIKeyIndex(g.APIKeys, key) < 0 {
			replaced := len(g.APIKeys)
			g.APIKeys = []gatewayAPIKey{newGatewayAPIKey(key, "网页新增")}
			if replaced > 0 {
				log.Printf("[管理端口设置] traceId=%s 说明=提交旧单 Key 字段，已按替换语义重建列表 原有条数=%d", trace, replaced)
			}
		}
	}
	g, err = normalizeGatewayConfig(g)
	if err != nil {
		return err
	}
	if err := validateGatewayEffectivePorts(g); err != nil {
		return err
	}
	log.Printf("[管理端口设置] traceId=%s 配置API端口=%d 配置Web端口=%d 当前API端口=%d 当前Web端口=%d 说明=保存等效于编辑gateway配置，监听端口将在校验后热切换",
		trace, g.APIPort, g.WebPort, cfg.Port, cfg.WebPort)
	registerSecrets(g.APIKey)
	return persistGatewayConfigLocked(doc, g, old, trace)
}

// validateGatewayEffectivePorts 校验配置文件端口与 -port 命令行覆盖后的组合不冲突。
func validateGatewayEffectivePorts(g gatewayFileConfig) error {
	effectiveAPIPort := g.APIPort
	if cfg.PortExplicit {
		effectiveAPIPort = cfg.Port
	}
	if cfg.WebUI && effectiveAPIPort == g.WebPort {
		return gatewayValidationError("Web 端口不能与实际 API 端口冲突（含 -port 命令行覆盖）")
	}
	return nil
}

// mutateGatewayAPIKeys 在管理写锁内完成「读取→修改 apiKeys→校验→落盘→切换运行时」，
// 与 saveGatewaySettings 共用同一套前置校验和写入顺序，避免两条路径行为不一致。
func mutateGatewayAPIKeys(trace string, mutate func(*gatewayFileConfig) error) error {
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
	if err := mutate(&g); err != nil {
		return err
	}
	g, err = normalizeGatewayConfig(g)
	if err != nil {
		return err
	}
	if err := validateGatewayEffectivePorts(g); err != nil {
		return err
	}
	return persistGatewayConfigLocked(doc, g, old, trace)
}

func gatewaySettingsView() map[string]any {
	gatewayConfigMu.RLock()
	defer gatewayConfigMu.RUnlock()
	actualAPI, actualWeb, live := gatewayServers.ports()
	if !live {
		actualAPI, actualWeb = cfg.Port, cfg.WebPort
	}
	return map[string]any{
		"apiKeyEnabled":      len(activeAPIKeysLocked()) > 0,
		"apiKeyConfigured":   len(gatewaySettings.APIKeys) > 0 || gatewaySettings.APIKey != "",
		"apiKeyCount":        len(gatewaySettings.APIKeys),
		"apiKeyMaxCount":     maxGatewayAPIKeys,
		"apiKeys":            gatewayAPIKeyViewsLocked(),
		"adminKeyConfigured": cfg.AdminKey != "",
		"apiPort":            actualAPI, "webPort": actualWeb,
		"configuredApiPort": gatewaySettings.APIPort,
		"configuredWebPort": gatewaySettings.WebPort,
		"apiPortOverridden": cfg.PortExplicit,
		"restartRequired":   gatewaySettings.WebPort != actualWeb || !cfg.PortExplicit && gatewaySettings.APIPort != actualAPI,
		"livePortSwitching": live,
	}
}

// gatewayAPIKeyViewsLocked 只输出掩码与元数据，绝不输出 Key 原文；调用方必须持有读锁。
func gatewayAPIKeyViewsLocked() []map[string]any {
	views := make([]map[string]any, 0, len(gatewaySettings.APIKeys))
	for _, k := range gatewaySettings.APIKeys {
		views = append(views, map[string]any{
			"id":           k.ID,
			"label":        k.Label,
			"apiKeyMasked": maskGatewayAPIKey(k.APIKey),
			"createdAt":    k.CreatedAt,
		})
	}
	return views
}

// maskGatewayAPIKey 对用户自填的模型 Key 比 maskToken 更保守：短 Key 全掩码，
// 否则「前 6 位」可能已经暴露大半字符（例如 7 位 Key 会露出 6 位）。
func maskGatewayAPIKey(key string) string {
	if len(key) <= 12 {
		return "****"
	}
	return maskToken(key)
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
