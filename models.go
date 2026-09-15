package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	modelsCacheFile       = "wb-models-cache.json"
	officialCatalogPath   = "product.cloudhosted.json"
	officialCatalogTar    = "package/" + officialCatalogPath
	modelsCatalogMaxBytes = 4 << 20
	modelsTarballMaxBytes = 80 << 20
	modelsMinCount        = 1
	modelsMaxCount        = 500
	modelsCacheSchema     = 2
)

var (
	npmPackageURLs = []string{
		"https://registry.npmjs.org/@tencent-ai/codebuddy-code",
		"https://registry.npmmirror.com/@tencent-ai/codebuddy-code",
	}
	unpkgCatalogURL = func(version string) string {
		return "https://unpkg.com/@tencent-ai/codebuddy-code@" + version + "/" + officialCatalogPath
	}

	staticFallbackModels = []string{
		"hy4-preview",
		"hy3-preview-agent",
		"hy3-preview",
		"hy3",
		"glm-5.2",
		"glm-5.1",
		"kimi-k2.7",
		"deepseek-v4.1-flash",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"minimax-m3-pay",
	}

	modelsMu         sync.RWMutex
	dynamicModels    []catalogModel
	dynamicSource    string
	modelsHTTPClient *http.Client
	failedVersions   = map[string]modelFailure{}
)

type catalogModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type modelsCache struct {
	Schema    int            `json:"schema"`
	Source    string         `json:"source"`
	FetchedAt int64          `json:"fetchedAt"`
	Models    []catalogModel `json:"models"`
}

type modelFailure struct {
	Attempts  int
	NextRetry time.Time
}

func initModelsHTTPClient() {
	modelsHTTPClient = &http.Client{Timeout: 2 * time.Minute, Transport: cfg.HttpClient.Transport}
}

func fetchLatestCLIVersion() (string, error) {
	var lastErr error
	for _, base := range npmPackageURLs {
		version, err := fetchLatestCLIVersionFrom(base)
		if err == nil {
			return version, nil
		}
		lastErr = err
		log.Printf("[Models] npm 版本查询源失败，源=%s，原因=%v，准备尝试下一源", base, err)
	}
	return "", lastErr
}

func fetchLatestCLIVersionFrom(base string) (string, error) {
	resp, err := modelsHTTPClient.Get(base + "/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest); err != nil {
		return "", fmt.Errorf("解析 npm manifest 失败: %w", err)
	}
	manifest.Version = strings.TrimSpace(manifest.Version)
	if manifest.Version == "" {
		return "", errors.New("npm manifest 缺少 version")
	}
	return manifest.Version, nil
}

func fetchOfficialCatalog(version string) ([]catalogModel, string, error) {
	models, err := fetchCatalogJSON(unpkgCatalogURL(version))
	if err == nil {
		return models, "unpkg", nil
	}
	log.Printf("[Models] unpkg 单文件目录下载失败，版本=%s，原因=%v；回退 npm tgz", version, err)

	var lastErr error
	for _, base := range npmPackageURLs {
		url := base + "/-/codebuddy-code-" + version + ".tgz"
		models, err = fetchCatalogTarball(url)
		if err == nil {
			return models, "npm-tgz", nil
		}
		lastErr = err
		log.Printf("[Models] npm tgz 目录提取失败，源=%s，版本=%s，原因=%v", base, version, err)
	}
	return nil, "", lastErr
}

func fetchCatalogJSON(url string) ([]catalogModel, error) {
	resp, err := modelsHTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > modelsCatalogMaxBytes {
		return nil, fmt.Errorf("目录响应过大: %d bytes", resp.ContentLength)
	}
	data, err := readLimited(resp.Body, modelsCatalogMaxBytes)
	if err != nil {
		return nil, err
	}
	return parseOfficialCatalog(data)
}

func fetchCatalogTarball(url string) ([]catalogModel, error) {
	resp, err := modelsHTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > modelsTarballMaxBytes {
		return nil, fmt.Errorf("tgz 响应超过上限: %d > %d bytes", resp.ContentLength, modelsTarballMaxBytes)
	}
	return extractCatalogFromTarGz(&hardLimitReader{r: resp.Body, remaining: modelsTarballMaxBytes})
}

type hardLimitReader struct {
	r         io.Reader
	remaining int64
}

func (r *hardLimitReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, fmt.Errorf("响应超过 %d bytes 上限", modelsTarballMaxBytes)
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("响应超过 %d bytes 上限", limit)
	}
	return data, nil
}

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
			return nil, fmt.Errorf("包内未找到 %s", officialCatalogTar)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != officialCatalogTar {
			continue
		}
		data, err := readLimited(tr, modelsCatalogMaxBytes)
		if err != nil {
			return nil, err
		}
		return parseOfficialCatalog(data)
	}
}

func parseOfficialCatalog(data []byte) ([]catalogModel, error) {
	var doc struct {
		Models *[]struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if doc.Models == nil {
		return nil, errors.New("目录缺少 models 数组")
	}
	seen := make(map[string]bool)
	models := make([]catalogModel, 0, len(*doc.Models))
	for _, model := range *doc.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, catalogModel{ID: id, Name: strings.TrimSpace(model.Name)})
	}
	if len(models) < modelsMinCount || len(models) > modelsMaxCount {
		return nil, fmt.Errorf("有效模型数量异常: %d（允许 %d-%d）", len(models), modelsMinCount, modelsMaxCount)
	}
	return models, nil
}

func loadModelsCache() {
	data, err := os.ReadFile(modelsCacheFile)
	if err != nil {
		return
	}
	var cache modelsCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Schema != modelsCacheSchema || strings.TrimSpace(cache.Source) == "" {
		log.Printf("[Models] 模型缓存无效，忽略并使用静态列表")
		return
	}
	models, err := validateCachedModels(cache.Models)
	if err != nil {
		log.Printf("[Models] 模型缓存校验失败，忽略并使用静态列表: %v", err)
		return
	}
	modelsMu.Lock()
	dynamicModels = models
	dynamicSource = cache.Source
	modelsMu.Unlock()
	log.Printf("[Models] 已加载模型目录缓存，版本=%s，模型数=%d", cache.Source, len(models))
}

func validateCachedModels(input []catalogModel) ([]catalogModel, error) {
	if len(input) < modelsMinCount || len(input) > modelsMaxCount {
		return nil, fmt.Errorf("模型数量异常: %d", len(input))
	}
	seen := make(map[string]bool)
	out := make([]catalogModel, 0, len(input))
	for _, model := range input {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" || seen[model.ID] {
			return nil, fmt.Errorf("存在空或重复模型 ID: %q", model.ID)
		}
		seen[model.ID] = true
		out = append(out, model)
	}
	return out, nil
}

func saveModelsCache(source string, models []catalogModel) error {
	data, err := json.MarshalIndent(modelsCache{Schema: modelsCacheSchema, Source: source, FetchedAt: time.Now().Unix(), Models: models}, "", "  ")
	if err != nil {
		return err
	}
	tmp := modelsCacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, modelsCacheFile)
}

func modelsSnapshot() ([]catalogModel, string) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	return append([]catalogModel(nil), dynamicModels...), dynamicSource
}

func mergedModelIDs() ([]string, string) {
	dynamic, source := modelsSnapshot()
	seen := make(map[string]bool)
	ids := make([]string, 0, len(dynamic)+len(staticFallbackModels))
	for _, model := range dynamic {
		if !seen[model.ID] {
			seen[model.ID] = true
			ids = append(ids, model.ID)
		}
	}
	for _, id := range staticFallbackModels {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, source
}

func modelSourceLabel(source string) string {
	if source == "" {
		return "static-fallback"
	}
	return "official-cli@" + source
}

func modelRetryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return 6 * time.Hour
	}
	if attempt == 2 {
		return 12 * time.Hour
	}
	return 24 * time.Hour
}

func refreshModelsOnce() {
	if modelsHTTPClient == nil {
		initModelsHTTPClient()
	}
	version, err := fetchLatestCLIVersion()
	if err != nil {
		log.Printf("[Models] 查询官方 CLI 最新版本失败，沿用现有模型列表: %v", err)
		return
	}
	_, current := modelsSnapshot()
	if version == current {
		log.Printf("[Models] 官方 CLI 版本未变化，版本=%s，本轮仅查询 manifest，未下载目录", version)
		return
	}

	modelsMu.Lock()
	failure, failed := failedVersions[version]
	modelsMu.Unlock()
	if failed && time.Now().Before(failure.NextRetry) {
		log.Printf("[Models] 版本 %s 上次同步失败，退避中，本轮不下载；下次允许重试=%s", version, failure.NextRetry.Format("2006-01-02 15:04:05"))
		return
	}

	models, transport, err := fetchOfficialCatalog(version)
	if err != nil {
		modelsMu.Lock()
		failure = failedVersions[version]
		failure.Attempts++
		failure.NextRetry = time.Now().Add(modelRetryDelay(failure.Attempts))
		failedVersions[version] = failure
		modelsMu.Unlock()
		log.Printf("[Models] 官方模型目录同步失败，版本=%s，失败次数=%d，下次允许重试=%s，沿用现有列表: %v", version, failure.Attempts, failure.NextRetry.Format("2006-01-02 15:04:05"), err)
		return
	}
	if err := saveModelsCache(version, models); err != nil {
		log.Printf("[Models] 官方目录已下载但缓存写入失败，不更新内存列表: %v", err)
		return
	}
	modelsMu.Lock()
	dynamicModels = models
	dynamicSource = version
	delete(failedVersions, version)
	modelsMu.Unlock()
	log.Printf("[Models] 模型列表同步成功，版本=%s，模型数=%d，下载方式=%s", version, len(models), transport)
}

func modelsRefreshLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	refreshModelsOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		refreshModelsOnce()
	}
}
