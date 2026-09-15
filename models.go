package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 模型列表动态同步
//
// 腾讯上游没有公开的模型列表 API，官方客户端（CLI / IDE 插件）的模型目录随版本
// 静态打包在 npm 包 @tencent-ai/codebuddy-code 的 product.cloudhosted.json 中
// （官方模型选择器渲染的就是这份目录）。本文件运行时从 npm registry 拉取最新
// 版本的该目录，与静态兜底列表合并后供 /v1/models 输出：
//   - 官方发布新版本（含新模型如 DeepSeek v4.1）后，网关自动跟进，无需更新二进制；
//   - 拉取失败（断网/registry 不可达）时回退静态兜底列表，服务不受影响；
//   - 目录落盘缓存（modelsCacheFile），重启后立即可用，版本未变化时不重复下载。
//
// 注意：缓存文件名不得以 workbuddy 开头 —— isCredentialFile 会把工作目录下所有
// workbuddy*.json 当作登录凭据自动加载进账号池。
// -----------------------------------------------------------------------------

const (
	// 官方目录缓存文件（工作目录下）
	modelsCacheFile = "wb-models-cache.json"

	npmRetryAttempts = 2 // 每个源的重试次数
	npmRetryDelay    = 3 * time.Second

	// tarball 内官方目录的路径，以及解析时的最大字节数（防异常包撑爆内存）
	catalogTarPath  = "package/product.cloudhosted.json"
	catalogMaxBytes = 4 << 20
)

// npmSourceBases npm registry 上官方 CLI 的包名前缀（latest 查询与 tarball 下载均基于此），
// registry.npmjs.org 不可达时（国内网络波动常见）自动切换 npmmirror 镜像。
var npmSourceBases = []string{
	"https://registry.npmjs.org/@tencent-ai/codebuddy-code",
	"https://registry.npmmirror.com/@tencent-ai/codebuddy-code",
}

// staticFallbackModels 静态兜底模型列表：动态目录不可用时保证 /v1/models 始终有值。
// deepseek-v4.1 等上游已上线但官方目录尚未收录的模型在此手工维护。
var staticFallbackModels = []string{
	"hy4-preview",
	"hy3-preview-agent",
	"hy3-preview",
	"hy3",
	"glm-5.2",
	"glm-5.1",
	"kimi-k2.7",
	"deepseek-v4.1",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"minimax-m3-pay",
}

// catalogModel 是官方目录中对外的单个模型条目（/v1/models 仅需 id）。
type catalogModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// modelsCache 是落盘缓存文件的结构。
type modelsCache struct {
	Source    string         `json:"source"` // 来源 npm 包版本号，如 2.151.0
	FetchedAt int64          `json:"fetchedAt"`
	Models    []catalogModel `json:"models"`
}

var (
	modelsMu      sync.RWMutex
	dynamicModels []catalogModel // 最近一次成功拉取的官方目录（nil 表示尚无动态数据）
	dynamicSource string         // 动态目录来源版本号（空表示无）

	modelsHTTPClient *http.Client // 模型目录下载专用客户端（tarball 较大，放宽超时）
)

// initModelsHTTPClient 复用主 HTTP 客户端的 Transport（使 -proxy 同样作用于 npm 下载），
// 仅放宽整体超时以适配大文件慢速链路。
func initModelsHTTPClient() {
	modelsHTTPClient = &http.Client{
		Timeout:   15 * time.Minute,
		Transport: cfg.HttpClient.Transport,
	}
}

// fetchLatestCLIVersion 查询 npm registry（含镜像容错）上官方 CLI 的最新版本号。
func fetchLatestCLIVersion() (string, error) {
	var lastErr error
	for _, base := range npmSourceBases {
		for attempt := 0; attempt < npmRetryAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(npmRetryDelay)
			}
			version, err := fetchLatestCLIVersionFrom(base)
			if err == nil {
				return version, nil
			}
			lastErr = err
		}
		log.Printf("[Models] 版本查询源 %s 失败，尝试下一个源: %v", base, lastErr)
	}
	return "", lastErr
}

// fetchLatestCLIVersionFrom 从单个 npm registry 源查询最新版本号。
func fetchLatestCLIVersionFrom(base string) (string, error) {
	resp, err := modelsHTTPClient.Get(base + "/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry 返回 HTTP %d", resp.StatusCode)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest); err != nil {
		return "", fmt.Errorf("解析 npm manifest 失败: %w", err)
	}
	if manifest.Version == "" {
		return "", fmt.Errorf("npm manifest 中缺少 version 字段")
	}
	return manifest.Version, nil
}

// fetchOfficialCatalog 下载指定版本官方 CLI 包（含镜像容错），流式解包仅提取
// product.cloudhosted.json，避免整包（约 50MB）落盘/驻留内存。
func fetchOfficialCatalog(version string) ([]catalogModel, error) {
	var lastErr error
	for _, base := range npmSourceBases {
		for attempt := 0; attempt < npmRetryAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(npmRetryDelay)
			}
			url := base + "/-/codebuddy-code-" + version + ".tgz"
			models, err := fetchOfficialCatalogFrom(url)
			if err == nil {
				return models, nil
			}
			lastErr = err
			log.Printf("[Models] 目录下载源 %s 失败: %v", url, err)
		}
	}
	return nil, lastErr
}

// fetchOfficialCatalogFrom 从单个 tarball 地址流式提取官方模型目录。
func fetchOfficialCatalogFrom(url string) ([]catalogModel, error) {
	resp, err := modelsHTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载官方包失败 HTTP %d", resp.StatusCode)
	}
	models, err := extractCatalogFromTarGz(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析官方目录失败: %w", err)
	}
	return models, nil
}

// extractCatalogFromTarGz 从 npm tarball 流中提取并解析官方模型目录。
func extractCatalogFromTarGz(r io.Reader) ([]catalogModel, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip 解压失败: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("包内未找到 %s", catalogTarPath)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != catalogTarPath {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, catalogMaxBytes))
		if err != nil {
			return nil, err
		}
		return parseOfficialCatalog(data)
	}
}

// parseOfficialCatalog 解析 product.cloudhosted.json 并过滤不适合对外暴露的条目。
func parseOfficialCatalog(data []byte) ([]catalogModel, error) {
	var doc struct {
		Models []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			SupportsExtra bool   `json:"supportsExtra"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var out []catalogModel
	for _, m := range doc.Models {
		if !isListableModel(m.ID, m.SupportsExtra) {
			continue
		}
		out = append(out, catalogModel{ID: m.ID, Name: m.Name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("官方目录解析结果为空")
	}
	return out, nil
}

// isListableModel 判断官方目录条目是否应出现在 /v1/models：
//   - supportsExtra：官方标记的补全/辅助模型（非聊天）；
//   - id 含 image：图像生成模型（不走聊天补全协议）；
//   - 其余（含 default / auto 官方路由条目）保留，与官方模型选择器保持一致。
func isListableModel(id string, supportsExtra bool) bool {
	if id == "" || supportsExtra {
		return false
	}
	return !strings.Contains(strings.ToLower(id), "image")
}

// loadModelsCache 启动时同步加载磁盘缓存，保证重启后 /v1/models 立即有动态数据。
func loadModelsCache() {
	data, err := os.ReadFile(modelsCacheFile)
	if err != nil {
		return
	}
	var c modelsCache
	if err := json.Unmarshal(data, &c); err != nil || len(c.Models) == 0 || c.Source == "" {
		return
	}
	modelsMu.Lock()
	dynamicModels = c.Models
	dynamicSource = c.Source
	modelsMu.Unlock()
}

// saveModelsCache 原子落盘当前动态目录（先写临时文件再改名）。
func saveModelsCache(source string, models []catalogModel) {
	c := modelsCache{Source: source, FetchedAt: time.Now().Unix(), Models: models}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	tmp := modelsCacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, modelsCacheFile)
}

// modelsSnapshot 返回当前动态目录及其来源版本的只读快照。
func modelsSnapshot() ([]catalogModel, string) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	return dynamicModels, dynamicSource
}

// mergedModelIDs 汇总 /v1/models 最终列表：动态官方目录在前、静态兜底在后，按 ID 去重保序。
// 返回列表与动态来源描述（无动态数据时来源为空串）。
func mergedModelIDs() ([]string, string) {
	dyn, source := modelsSnapshot()
	seen := make(map[string]bool)
	var out []string
	for _, m := range dyn {
		if !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m.ID)
		}
	}
	for _, id := range staticFallbackModels {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, source
}

// refreshModelsOnce 执行一次「查询最新版本 → 版本变化才下载目录」的同步流程。
// quiet=true 时版本未变化等常规情况不打日志。
func refreshModelsOnce(quiet bool) {
	if modelsHTTPClient == nil {
		initModelsHTTPClient()
	}
	version, err := fetchLatestCLIVersion()
	if err != nil {
		if !quiet {
			log.Printf("[Models] 查询官方 CLI 最新版本失败（沿用现有列表）: %v", err)
		}
		return
	}
	_, current := modelsSnapshot()
	if version == current {
		return // 版本未变化，无需重新下载
	}
	models, err := fetchOfficialCatalog(version)
	if err != nil {
		log.Printf("[Models] 拉取官方模型目录 (v%s) 失败（沿用现有列表）: %v", version, err)
		return
	}
	modelsMu.Lock()
	dynamicModels = models
	dynamicSource = version
	modelsMu.Unlock()
	saveModelsCache(version, models)
	log.Printf("[Models] 模型列表已同步官方目录 v%s（%d 个模型）", version, len(models))
}

// modelsRefreshLoop 后台周期同步官方模型目录（interval<=0 时关闭动态同步）。
func modelsRefreshLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	refreshModelsOnce(false)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		refreshModelsOnce(false)
	}
}
