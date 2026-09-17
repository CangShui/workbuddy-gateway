package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 模型目录与倍率
//
// 列表来源（两路合并去重）：
//  1. 实时接口 GET {Base}/v2/enterprises/personal/models（权威，含促销）
//  2. npm 包内静态目录 product.internal.json / product.cloudhosted.json
//
// 任一路失败用本地缓存；无缓存则用另一路成功的数据先顶上，等下次刷新覆盖；
// 两路都失败且无缓存则不展示模型列表（不影响模型调用，调用始终透传）。
//
// 倍率：
//  1. 优先实时接口生效倍率（credits × 促销 factor）
//  2. 若接口给出 0.00x 但促销已到期，或该模型不在接口目录中，
//     用「余额未耗尽」的同站点账号探测一次（每 12 小时一轮）
//     - 探测为免费 → 展示 0.00x，并继续每 12 小时复测确认
//     - 探测为收费 → 展示 [未知倍率]，直到接口重新给出 0.00x
//     （无促销时间，或仍在促销时间窗口内）才恢复免费
//     - 14018 / 未知 → 不覆盖接口值
// -----------------------------------------------------------------------------

const (
	modelsCacheFile   = "wb-models-cache.json"
	modelsCacheSchema = 5
	modelsMinCount    = 1
	modelsMaxCount    = 500
	modelsPath        = "/v2/enterprises/personal/models"

	npmCatalogMaxBytes = 4 << 20
	npmTarballMaxBytes = 80 << 20

	// modelAPIPriceProbeInterval 同一站点+模型的价格探测间隔。
	modelAPIPriceProbeInterval = 12 * time.Hour
	// 价格探测调度：启动后很快首轮；仍有待探测时用短间隔追赶；收敛后长间隔。
	modelPriceProbeStartDelay   = 20 * time.Second
	modelPriceProbeTick         = 30 * time.Minute
	modelPriceProbeCatchUp      = 2 * time.Minute
	modelPriceProbeBatch        = 5
	modelPriceProbeInitialBatch = 30
)

var catalogSites = []string{"cn", "intl"}

// npm 包内各站点目录文件。
var npmCatalogFiles = map[string]string{
	"cn":   "product.internal.json",
	"intl": "product.cloudhosted.json",
}

var npmBases = []string{
	"https://registry.npmjs.org/@tencent-ai/codebuddy-code",
	"https://registry.npmmirror.com/@tencent-ai/codebuddy-code",
}

type catalogModel struct {
	ID              string  `json:"id"`
	Name            string  `json:"name,omitempty"`
	Credits         string  `json:"credits,omitempty"`
	BaseMultiplier  float64 `json:"baseMultiplier,omitempty"`
	Multiplier      float64 `json:"multiplier,omitempty"` // 生效倍率（含促销）
	HasMultiplier   bool    `json:"hasMultiplier,omitempty"`
	PromoLabel      string  `json:"promoLabel,omitempty"`
	PromoUntil      string  `json:"promoUntil,omitempty"`
	PromoFree       bool    `json:"promoFree,omitempty"`
	PromoExpired    bool    `json:"promoExpired,omitempty"` // 有促销但已过期/未生效
	FromLive        bool    `json:"fromLive,omitempty"`     // 是否来自实时接口
	MaxInputTokens  int     `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

// modelPriceProbe 是某站点某模型的价格探测状态。
type modelPriceProbe struct {
	LastProbeAt int64   `json:"lastProbeAt,omitempty"`
	Verdict     string  `json:"verdict,omitempty"` // free | paid
	Credit      float64 `json:"credit,omitempty"`
	Tokens      int64   `json:"tokens,omitempty"`
	Detail      string  `json:"detail,omitempty"`
}

type modelsCache struct {
	Schema    int                        `json:"schema"`
	Source    string                     `json:"source"`
	FetchedAt int64                      `json:"fetchedAt"`
	Catalogs  map[string][]catalogModel  `json:"catalogs"`
	Probes    map[string]modelPriceProbe `json:"probes,omitempty"`
}

var (
	modelsMu          sync.RWMutex
	catalogModels     = map[string][]catalogModel{}
	modelProbes       = map[string]modelPriceProbe{}
	dynamicSource     string
	modelsScanTrigger = make(chan struct{}, 1)
	modelProbeTrigger = make(chan struct{}, 1)
)

func probeKey(site, model string) string {
	return site + "|" + normalizeModelName(model)
}

// parseCredits 解析 credits 字段为数值倍率，例如 "x0.29 credits" -> 0.29。
func parseCredits(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		s = strings.TrimPrefix(s, "x")
		s = strings.ReplaceAll(s, "credits", "")
		s = strings.ReplaceAll(s, "credit", "")
		s = strings.TrimSpace(s)
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

// -----------------------------------------------------------------------------
// 实时接口
// -----------------------------------------------------------------------------

type livePromotion struct {
	ID       string   `json:"id"`
	Enabled  bool     `json:"enabled"`
	ModelIDs []string `json:"modelIds"`
	Badge    struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount struct {
		Factor float64 `json:"factor"`
	} `json:"discount"`
	Schedule struct {
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

type liveCatalogData struct {
	Models []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Credits         any    `json:"credits"`
		MaxInputTokens  int    `json:"maxInputTokens"`
		MaxOutputTokens int    `json:"maxOutputTokens"`
	} `json:"models"`
	ModelPromotions []livePromotion `json:"modelPromotions"`
}

func pickCatalogAccount(edition string) *Account {
	accountMu.Lock()
	defer accountMu.Unlock()
	for _, acc := range accounts {
		if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
			continue
		}
		if profileForEdition(acc.Auth.Edition).Key == edition {
			return acc
		}
	}
	return nil
}

func fetchLiveCatalog(edition string) ([]catalogModel, int, error) {
	acc := pickCatalogAccount(edition)
	if acc == nil {
		return nil, 0, fmt.Errorf("没有可用的%s账号", profileForEdition(edition).Label)
	}
	accountMu.Lock()
	auth := *acc.Auth
	accountMu.Unlock()
	return fetchLiveCatalogByAuth(&auth)
}

func fetchLiveCatalogByAuth(auth *StoredAuth) ([]catalogModel, int, error) {
	prof := profileForEdition(auth.Edition)
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-ID", prof.ClientID)
		r.Header.Set("X-Client-Version", prof.ClientVer)
		r.Header.Set("X-Product", prof.Product)
		if auth.Account.UID != "" {
			r.Header.Set("X-User-Id", auth.Account.UID)
		}
		if auth.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
		}
		if auth.Auth.Domain != "" {
			r.Header.Set("X-Domain", auth.Auth.Domain)
		}
	}
	data, status, err := doJSON(cfg.HttpClient, http.MethodGet, prof.Base+modelsPath, headers, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP %d: %w", status, err)
	}
	return parseLiveCatalog(data)
}

func promoActive(p livePromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if p.Schedule.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, p.Schedule.ValidFrom); err == nil && now.Before(from) {
			return false
		}
	}
	if p.Schedule.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, p.Schedule.ValidUntil); err == nil && !now.Before(until) {
			return false
		}
	}
	return true
}

func parseLiveCatalog(data []byte) ([]catalogModel, int, error) {
	var doc liveCatalogData
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, fmt.Errorf("解析实时目录失败: %w", err)
	}
	if len(doc.Models) == 0 {
		return nil, 0, errors.New("实时目录 models 为空")
	}
	now := time.Now()

	type hit struct {
		factor  float64
		label   string
		until   string
		expired bool
	}
	hits := map[string]hit{}
	active := 0
	for _, p := range doc.ModelPromotions {
		on := promoActive(p, now)
		if on {
			active++
		}
		label := strings.TrimSpace(p.Badge.Label)
		if label == "" {
			label = "Promo"
		}
		for _, id := range p.ModelIDs {
			if on {
				if old, ok := hits[id]; ok && !old.expired && old.factor <= p.Discount.Factor {
					continue
				}
				hits[id] = hit{factor: p.Discount.Factor, label: label, until: p.Schedule.ValidUntil}
			} else {
				// 记录"存在促销但当前不生效"，用于触发价格探测。
				if old, ok := hits[id]; ok && old.expired {
					continue
				}
				hits[id] = hit{label: label, until: p.Schedule.ValidUntil, expired: true}
			}
		}
	}

	seen := map[string]bool{}
	out := make([]catalogModel, 0, len(doc.Models))
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry := catalogModel{
			ID: id, Name: strings.TrimSpace(m.Name), FromLive: true,
			MaxInputTokens: m.MaxInputTokens, MaxOutputTokens: m.MaxOutputTokens,
		}
		if s, ok := m.Credits.(string); ok {
			entry.Credits = strings.TrimSpace(s)
		}
		base, hasBase := parseCredits(m.Credits)
		entry.BaseMultiplier = base
		entry.HasMultiplier = hasBase
		entry.Multiplier = base
		if h, ok := hits[id]; ok {
			entry.PromoLabel = h.label
			entry.PromoUntil = h.until
			if h.expired {
				entry.PromoExpired = true
			} else {
				entry.Multiplier = base * h.factor
				if !hasBase {
					entry.Multiplier = h.factor
				}
				entry.HasMultiplier = true
				if h.factor == 0 {
					entry.PromoFree = true
				}
			}
		}
		out = append(out, entry)
	}
	if len(out) < modelsMinCount || len(out) > modelsMaxCount {
		return nil, 0, fmt.Errorf("模型数量异常: %d", len(out))
	}
	return out, active, nil
}

// -----------------------------------------------------------------------------
// npm 静态目录（兜底来源之一）
// -----------------------------------------------------------------------------

func fetchNPMCatalogVersion() (string, error) {
	var lastErr error
	for _, base := range npmBases {
		resp, err := cfg.HttpClient.Get(base + "/latest")
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		var m struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &m); err != nil || strings.TrimSpace(m.Version) == "" {
			lastErr = fmt.Errorf("manifest 缺少 version")
			continue
		}
		return strings.TrimSpace(m.Version), nil
	}
	return "", lastErr
}

func parseNPMCatalog(data []byte) ([]catalogModel, error) {
	var doc struct {
		Models *[]struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Credits any    `json:"credits"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 npm 目录失败: %w", err)
	}
	if doc.Models == nil {
		return nil, errors.New("npm 目录缺少 models 数组")
	}
	seen := map[string]bool{}
	out := make([]catalogModel, 0, len(*doc.Models))
	for _, m := range *doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry := catalogModel{ID: id, Name: strings.TrimSpace(m.Name)}
		if s, ok := m.Credits.(string); ok {
			entry.Credits = strings.TrimSpace(s)
		}
		if v, ok := parseCredits(m.Credits); ok {
			entry.BaseMultiplier = v
			entry.Multiplier = v
			entry.HasMultiplier = true
		}
		out = append(out, entry)
	}
	if len(out) < modelsMinCount || len(out) > modelsMaxCount {
		return nil, fmt.Errorf("npm 目录模型数量异常: %d", len(out))
	}
	return out, nil
}

// fetchNPMCatalog 拉取指定站点目录：unpkg 单文件优先，失败回退 npm tgz 流式提取。
func fetchNPMCatalog(version, file string) ([]catalogModel, string, error) {
	url := "https://unpkg.com/@tencent-ai/codebuddy-code@" + version + "/" + file
	models, err := fetchNPMCatalogJSON(url)
	if err == nil {
		return models, "unpkg", nil
	}
	var lastErr error
	for _, base := range npmBases {
		tgz := base + "/-/codebuddy-code-" + version + ".tgz"
		models, err = fetchNPMCatalogTarball(tgz, file)
		if err == nil {
			return models, "npm-tgz", nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

func fetchNPMCatalogJSON(url string) ([]catalogModel, error) {
	resp, err := cfg.HttpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > npmCatalogMaxBytes {
		return nil, fmt.Errorf("目录响应过大: %d bytes", resp.ContentLength)
	}
	data, err := readLimited(resp.Body, npmCatalogMaxBytes)
	if err != nil {
		return nil, err
	}
	return parseNPMCatalog(data)
}

type hardLimitReader struct {
	r         io.Reader
	remaining int64
}

func (r *hardLimitReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, fmt.Errorf("响应超过 %d bytes 上限", npmTarballMaxBytes)
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

func fetchNPMCatalogTarball(url, file string) ([]catalogModel, error) {
	resp, err := cfg.HttpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > npmTarballMaxBytes {
		return nil, fmt.Errorf("tgz 响应超过上限: %d bytes", resp.ContentLength)
	}
	gz, err := gzip.NewReader(&hardLimitReader{r: resp.Body, remaining: npmTarballMaxBytes})
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	want := "package/" + file
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("包内未找到 %s", want)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != want {
			continue
		}
		data, err := readLimited(tr, npmCatalogMaxBytes)
		if err != nil {
			return nil, err
		}
		return parseNPMCatalog(data)
	}
}

// mergeCatalogs 合并两份目录：实时接口优先，npm 仅补齐接口没有的模型。
func mergeCatalogs(live, npm []catalogModel) []catalogModel {
	out := make([]catalogModel, 0, len(live)+len(npm))
	seen := map[string]bool{}
	for _, m := range live {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, m := range npm {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

// -----------------------------------------------------------------------------
// 缓存
// -----------------------------------------------------------------------------

func loadModelsCache() {
	data, err := os.ReadFile(modelsCacheFile)
	if err != nil {
		return
	}
	var cache modelsCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Schema != modelsCacheSchema {
		log.Printf("[Models] 模型缓存无效或格式版本过旧，忽略")
		return
	}
	validated := map[string][]catalogModel{}
	for key, models := range cache.Catalogs {
		if len(models) < modelsMinCount || len(models) > modelsMaxCount {
			continue
		}
		validated[key] = models
	}
	modelsMu.Lock()
	if len(validated) > 0 {
		catalogModels = validated
	}
	if len(cache.Probes) > 0 {
		modelProbes = cache.Probes
	}
	dynamicSource = cache.Source
	modelsMu.Unlock()
	for key, models := range validated {
		log.Printf("[Models] 已加载站点目录缓存：站点=%s，模型数=%d", key, len(models))
	}
}

func saveModelsCache(source string, catalogs map[string][]catalogModel, probes map[string]modelPriceProbe) error {
	data, err := json.MarshalIndent(modelsCache{
		Schema: modelsCacheSchema, Source: source, FetchedAt: time.Now().Unix(),
		Catalogs: catalogs, Probes: probes,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := modelsCacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, modelsCacheFile)
}

func persistModelsLocked(source string) {
	if err := saveModelsCache(source, catalogModels, modelProbes); err != nil {
		log.Printf("[Models] 缓存写入失败: %v", err)
	}
}

func catalogSnapshot() (map[string][]catalogModel, string) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	out := make(map[string][]catalogModel, len(catalogModels))
	for key, models := range catalogModels {
		out[key] = append([]catalogModel(nil), models...)
	}
	return out, dynamicSource
}

func mergedModelIDs() ([]string, string) {
	catalogs, source := catalogSnapshot()
	seen := map[string]bool{}
	ids := make([]string, 0, 64)
	for _, site := range catalogSites {
		for _, m := range catalogs[site] {
			if !seen[m.ID] {
				seen[m.ID] = true
				ids = append(ids, m.ID)
			}
		}
	}
	return ids, source
}

func modelEntry(site, modelID string) (catalogModel, bool) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	modelID = normalizeModelName(modelID)
	for _, m := range catalogModels[site] {
		if m.ID == modelID {
			return m, true
		}
	}
	return catalogModel{}, false
}

func modelMultiplier(site, modelID string) (float64, bool) {
	entry, ok := modelEntry(site, modelID)
	if !ok {
		return 0, false
	}
	return entry.Multiplier, entry.HasMultiplier
}

func modelProbeVerdict(site, modelID string) string {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	return modelProbes[probeKey(site, modelID)].Verdict
}

// modelDisplayMultiplier 生成 monitor 附表的倍率文案。
// 促销过期只表示折扣结束，倍率回落到 credits 原价，并不等于"价格未知"；
// 只有实测确认收费、且接口给不出有效倍率时，才显示「收费(倍率未知)」。
func modelDisplayMultiplier(site, modelID, freeLabel string) string {
	if freeLabel == "是" {
		return "0.00x"
	}
	switch modelProbeVerdict(site, modelID) {
	case "free":
		return "0.00x"
	case "paid":
		return "收费(倍率未知)"
	}
	entry, ok := modelEntry(site, modelID)
	if !ok || !entry.HasMultiplier {
		return "-"
	}
	// 促销已过期时接口的 credits 不可信（上游常把促销价固化在 credits 里），
	// 探测出结果前按完全未知处理，不展示可能过期的价格。
	if entry.PromoExpired {
		return "-"
	}
	return fmt.Sprintf("%.2fx", entry.Multiplier)
}

// siteKnownFree 判断某站点该模型是否已确认免费（实测或接口明确 0.00x）。
func siteKnownFree(site, modelID string) bool {
	if modelProbeVerdict(site, modelID) == "free" {
		return true
	}
	accountMu.Lock()
	for _, acc := range accounts {
		if profileForEdition(acc.Auth.Edition).Key != site && acc.Edition != site {
			continue
		}
		if acc.ModelStates != nil {
			if st := acc.ModelStates[normalizeModelName(modelID)]; st != nil && st.CostClass == modelCostFree {
				accountMu.Unlock()
				return true
			}
		}
	}
	accountMu.Unlock()
	entry, ok := modelEntry(site, modelID)
	return ok && entry.HasMultiplier && entry.Multiplier == 0 && !entry.PromoExpired
}

// siteKnownPaid 判断某站点该模型是否已确认收费。
func siteKnownPaid(site, modelID string) bool {
	if modelProbeVerdict(site, modelID) == "paid" {
		return true
	}
	entry, ok := modelEntry(site, modelID)
	return ok && entry.HasMultiplier && entry.Multiplier > 0
}

// preferredFreeSites 返回"应优先使用的免费站点"集合：
// 仅当出现「一个站点免费、另一个站点收费」时启用优先；都免费或都收费则不搞优先。
func preferredFreeSites(modelID string) map[string]bool {
	var free, paid []string
	for _, site := range catalogSites {
		if siteKnownFree(site, modelID) {
			free = append(free, site)
		}
		if siteKnownPaid(site, modelID) {
			paid = append(paid, site)
		}
	}
	if len(free) == 0 || len(paid) == 0 {
		return nil
	}
	out := make(map[string]bool, len(free))
	for _, site := range free {
		out[site] = true
	}
	return out
}

func modelSourceLabel(source string) string {
	if source == "" {
		return "unavailable"
	}
	if strings.HasPrefix(source, "live-api@") {
		return source
	}
	return "live-api@" + source
}

// modelNeedsProbe 判断某站点该模型是否需要价格探测。
func modelNeedsProbe(site, modelID string, requests int64) bool {
	entry, ok := modelEntry(site, modelID)
	if !ok || !entry.FromLive {
		// 不在实时接口目录中：只探测被实际请求过的模型，避免白白消耗额度。
		return requests > 0
	}
	return entry.HasMultiplier && entry.Multiplier == 0 && entry.PromoExpired
}

// -----------------------------------------------------------------------------
// 同步
// -----------------------------------------------------------------------------

func refreshModelsOnce() {
	version, verErr := fetchNPMCatalogVersion()
	if verErr != nil {
		log.Printf("[Models] npm 版本查询失败（仅影响 npm 兜底来源）: %v", verErr)
	}

	prev, prevSource := catalogSnapshot()
	fetched := map[string][]catalogModel{}
	var failures []string

	for _, site := range catalogSites {
		var live, npm []catalogModel
		liveOK, npmOK := false, false

		if models, promos, err := fetchLiveCatalog(site); err != nil {
			failures = append(failures, fmt.Sprintf("%s/接口: %v", site, err))
			log.Printf("[Models] 实时目录失败：站点=%s，原因=%v", site, err)
		} else {
			live, liveOK = models, true
			log.Printf("[Models] 实时目录成功：站点=%s，模型数=%d，生效促销=%d", site, len(models), promos)
		}

		if version != "" {
			if models, transport, err := fetchNPMCatalog(version, npmCatalogFiles[site]); err != nil {
				failures = append(failures, fmt.Sprintf("%s/npm: %v", site, err))
				log.Printf("[Models] npm 目录失败：站点=%s，原因=%v", site, err)
			} else {
				npm, npmOK = models, true
				log.Printf("[Models] npm 目录成功：站点=%s，模型数=%d，来源=%s", site, len(models), transport)
			}
		}

		switch {
		case liveOK && npmOK:
			fetched[site] = mergeCatalogs(live, npm)
		case liveOK:
			fetched[site] = live
		case npmOK:
			fetched[site] = npm
		default:
			if cached, ok := prev[site]; ok && len(cached) > 0 {
				fetched[site] = cached
				log.Printf("[Models] 站点=%s 两路来源均失败，沿用缓存（模型数=%d）", site, len(cached))
			} else {
				log.Printf("[Models] 站点=%s 两路来源均失败且无缓存，该站点本次不展示模型", site)
			}
		}
	}

	if len(fetched) == 0 {
		log.Printf("[Models] 无任何可用目录数据，保留原状态；失败详情: %s", strings.Join(failures, " | "))
		return
	}

	allCached := true
	for site := range fetched {
		if _, ok := prev[site]; !ok || len(prev[site]) == 0 {
			allCached = false
		}
	}

	source := time.Now().Format("2006-01-02 15:04")
	if allCached && prevSource != "" {
		source = prevSource
	}

	modelsMu.Lock()
	catalogModels = fetched
	dynamicSource = source
	// 接口重新给出 0.00x 且促销未过期（或本就无促销）时，清除"收费"判定恢复免费。
	for site, models := range fetched {
		for _, m := range models {
			if m.HasMultiplier && m.Multiplier == 0 && !m.PromoExpired {
				key := probeKey(site, m.ID)
				if p, ok := modelProbes[key]; ok && p.Verdict == "paid" {
					delete(modelProbes, key)
					log.Printf("[Models] 站点=%s 模型=%s 接口重新给出 0.00x，清除收费判定", site, m.ID)
				}
			}
		}
	}
	persistModelsLocked(source)
	modelsMu.Unlock()
	requestModelsProbe()
}

func requestModelsScan() {
	select {
	case modelsScanTrigger <- struct{}{}:
	default:
	}
}

// requestModelsProbe 触发一轮价格探测（目录变化、凭据变化、重置后调用）。
func requestModelsProbe() {
	select {
	case modelProbeTrigger <- struct{}{}:
	default:
	}
}

func modelsRefreshLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	refreshModelsOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			refreshModelsOnce()
		case <-modelsScanTrigger:
			log.Printf("[Models] 账号池发生变化，立即刷新模型目录")
			refreshModelsOnce()
		}
	}
}

// modelPriceProbeLoop 周期性为「需要确认价格」的模型做真实探测。
// modelPriceProbeLoop 周期性执行价格探测。
// 首轮在启动后很快执行；只要仍有待探测模型就用较短间隔追赶，收敛后回到长间隔。
func modelPriceProbeLoop() {
	timer := time.NewTimer(modelPriceProbeStartDelay)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-modelProbeTrigger:
		}
		modelPriceProbeOnce()
		next := modelPriceProbeTick
		if modelPendingProbeCount() > 0 {
			next = modelPriceProbeCatchUp
		}
		timer.Reset(next)
	}
}

// modelPendingProbeCount 统计当前仍需要探测（且已过探测间隔）的 站点+模型 数量。
func modelPendingProbeCount() int {
	ids, _ := mergedModelIDs()
	now := time.Now()
	pending := 0
	for _, site := range catalogSites {
		for _, id := range ids {
			if !modelNeedsProbe(site, id, modelRequestCount(id)) {
				continue
			}
			modelsMu.RLock()
			last := modelProbes[probeKey(site, id)].LastProbeAt
			modelsMu.RUnlock()
			if last > 0 && now.Sub(time.Unix(last, 0)) < modelAPIPriceProbeInterval {
				continue
			}
			pending++
		}
	}
	return pending
}

// modelProbeBatchSize 返回本轮探测数量：首次（尚无任何探测记录）时放大，
// 让新安装 / 重置后能尽快得到结论；之后回到常规批量。
func modelProbeBatchSize() int {
	modelsMu.RLock()
	n := len(modelProbes)
	modelsMu.RUnlock()
	if n == 0 {
		return modelPriceProbeInitialBatch
	}
	return modelPriceProbeBatch
}

func modelPriceProbeOnce() {
	ids, _ := mergedModelIDs()
	if len(ids) == 0 {
		return
	}
	now := time.Now()
	batch := modelProbeBatchSize()
	done := 0
	for _, site := range catalogSites {
		if done >= batch {
			break
		}
		acc := pickProbeAccount(site)
		if acc == nil {
			continue
		}
		for _, id := range ids {
			if done >= batch {
				break
			}
			if !modelNeedsProbe(site, id, modelRequestCount(id)) {
				continue
			}
			modelsMu.RLock()
			last := modelProbes[probeKey(site, id)].LastProbeAt
			modelsMu.RUnlock()
			if last > 0 && now.Sub(time.Unix(last, 0)) < modelAPIPriceProbeInterval {
				continue
			}
			verdict, credit, tokens, detail := probeModelPrice(acc, id)
			modelsMu.Lock()
			p := modelProbes[probeKey(site, id)]
			p.LastProbeAt = now.Unix()
			p.Credit, p.Tokens, p.Detail = credit, tokens, detail
			if verdict != "" {
				p.Verdict = verdict
			}
			modelProbes[probeKey(site, id)] = p
			persistModelsLocked(dynamicSource)
			modelsMu.Unlock()
			log.Printf("[ModelPrice] 站点=%s 账号=%s 模型=%s -> verdict=%s credit=%s tokens=%d (%s)",
				site, acc.Path, id, p.Verdict, formatQuota(credit), tokens, detail)
			done++
		}
	}
	if done > 0 {
		writeStatusSnapshot()
	}
}

// pickProbeAccount 选择同站点账号用于价格探测。
// 优先「已知有余额」的账号；没有时退而使用「额度未知」的账号（例如额度接口
// 返回 20017 无权限的账号）——探测若命中 14018 只会被忽略，不会污染判定。
// 仅跳过「已知耗尽」与失效账号。
func pickProbeAccount(site string) *Account {
	accountMu.Lock()
	defer accountMu.Unlock()
	var unknownQuota *Account
	for _, acc := range accounts {
		if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
			continue
		}
		if profileForEdition(acc.Auth.Edition).Key != site {
			continue
		}
		if acc.QuotaKnown {
			if acc.QuotaRemaining > 0 {
				return acc
			}
			continue // 已知耗尽，跳过
		}
		if unknownQuota == nil {
			unknownQuota = acc
		}
	}
	return unknownQuota
}

// probeModelPrice 发一次最小请求判定免费/收费；不修改账号调度状态。
// 返回 verdict（free|paid|""）、credit、tokens、说明。
func probeModelPrice(acc *Account, model string) (string, float64, int64, string) {
	if !lockAccountWithContext(context.Background(), &acc.lock) {
		return "", 0, 0, "等待账号锁失败"
	}
	defer acc.lock.Unlock()

	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil {
		accountMu.Unlock()
		return "", 0, 0, "账号不可用"
	}
	auth := *acc.Auth
	prof := acc.Profile()
	accountMu.Unlock()

	payload, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "max_tokens": 300,
		"messages": []any{
			map[string]any{"role": "system", "content": defaultSystemPrompt},
			map[string]any{"role": "user", "content": probePrompt},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prof.chatURL(), strings.NewReader(string(payload)))
	if err != nil {
		return "", 0, 0, err.Error()
	}
	backendHeaders(req, &auth, prof)
	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return "", 0, 0, "请求失败: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		s := string(body)
		switch {
		case isQuotaExhausted(resp.StatusCode, s):
			return "", 0, 0, "14018 额度耗尽，不覆盖"
		case isModelRateLimited(s):
			return "", 0, 0, "6004 模型限流，不覆盖"
		default:
			return "", 0, 0, fmt.Sprintf("HTTP %d，不覆盖", resp.StatusCode)
		}
	}
	usage := readUsageFromSSE(resp.Body)
	credit, ok := usageCredit(usage)
	if !ok {
		return "", 0, 0, "无 usage.credit，不覆盖"
	}
	tokens, _ := usageTotalTokens(usage)
	if credit > 0 {
		return "paid", credit, tokens, "usage.credit>0 收费"
	}
	if tokens < modelFreeMinTokens {
		return "", credit, tokens, "credit=0 但样本过小，不判定"
	}
	return "free", credit, tokens, "usage.credit=0 免费"
}

// sortModelsForDisplay 保持稳定顺序：先实时接口顺序，再按 ID。
func sortModelsForDisplay(models []catalogModel) []catalogModel {
	out := append([]catalogModel(nil), models...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FromLive != out[j].FromLive {
			return out[i].FromLive
		}
		return out[i].ID < out[j].ID
	})
	return out
}
