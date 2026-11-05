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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// 模型目录与倍率
//
// 列表来源（按优先级合并，站点内别名去重）：
//  1. 官方桌面目录 GET {Base}/v3/config
//  2. 官方窄表 GET {Base}/v2/enterprises/personal/models
//  3. npm 静态目录 product.internal.json / product.cloudhosted.json
//
// 任一路失败用本地缓存；无缓存则用另一路成功的数据先顶上，等下次刷新覆盖；
// 两路都失败且无缓存则不展示模型列表（不影响模型调用，调用始终透传）。
//
// 倍率数值和来源统一由 rate_resolution.go 决定；实测与目录时间随缓存保存。
// 重复刷新不取消已确认的冲突，真实价格规则变化才使旧实测失效。
// 后台真实探测需要显式开启；目录抓取和查看详情不会自动发起模型生成。
// -----------------------------------------------------------------------------

const (
	modelsCacheFile   = "wb-models-cache.json"
	modelsCacheSchema = 5
	modelsMinCount    = 1
	modelsMaxCount    = 500
	modelsPath        = "/v2/enterprises/personal/models"
	catalogV3Path     = "/v3/config"

	catalogCNUserAgent   = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
	catalogINTLUserAgent = "WorkBuddy/5.8.3 WorkBuddy AI/5.8.3 CLI/2.63.2"

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
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Credits         string   `json:"credits,omitempty"`
	BaseMultiplier  float64  `json:"baseMultiplier,omitempty"`
	Multiplier      float64  `json:"multiplier,omitempty"` // 生效倍率（含促销）
	HasMultiplier   bool     `json:"hasMultiplier,omitempty"`
	PromoLabel      string   `json:"promoLabel,omitempty"`
	PromoUntil      string   `json:"promoUntil,omitempty"`
	PromoFrom       string   `json:"promoFrom,omitempty"`
	PromoFactor     *float64 `json:"promoFactor,omitempty"`
	PromoFree       bool     `json:"promoFree,omitempty"`
	PromoExpired    bool     `json:"promoExpired,omitempty"`  // 有促销但已过期/未生效
	FromLive        bool     `json:"fromLive,omitempty"`      // 是否来自实时接口
	RateCheckedAt   int64    `json:"rateCheckedAt,omitempty"` // 最后成功读取该来源，毫秒
	RateChangedAt   int64    `json:"rateChangedAt,omitempty"` // 价格规则最后变动，毫秒
	RateCached      bool     `json:"-"`                       // 本轮沿用缓存，不能冒充新抓取
	MaxInputTokens  int      `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	MaxAllowedSize  int      `json:"maxAllowedSize,omitempty"` // 上游声明的最大允许上下文
	// ContextLengths / ContextDefaultLength 来自上游 contextWindow：
	// 前者是客户端可选的上下文档位，后者是客户端默认预算。
	// 它们与「模型能力上限」（MaxInputTokens）语义不同，不能合并成一个值：
	// 上限决定上游接受多少输入，默认预算只决定客户端自己从多少 token 开始压缩。
	ContextLengths       []int `json:"contextLengths,omitempty"`
	ContextDefaultLength int   `json:"contextDefaultLength,omitempty"`
}

// modelPriceProbe 是某站点某模型的价格探测状态。
type modelPriceProbe struct {
	LastProbeAt int64   `json:"lastProbeAt,omitempty"`
	VerdictAt   int64   `json:"verdictAt,omitempty"` // 最后一次成功结论，不能被失败的尝试更新时间
	LastError   string  `json:"lastError,omitempty"`
	FreeUntil   string  `json:"freeUntil,omitempty"`
	Cached      bool    `json:"-"`
	Verdict     string  `json:"verdict,omitempty"` // free | paid
	Credit      float64 `json:"credit,omitempty"`
	Tokens      int64   `json:"tokens,omitempty"`
	Detail      string  `json:"detail,omitempty"`
}

type modelsCache struct {
	Schema    int                             `json:"schema"`
	Source    string                          `json:"source"`
	FetchedAt int64                           `json:"fetchedAt"`
	Catalogs  map[string][]catalogModel       `json:"catalogs"`
	Probes    map[string]modelPriceProbe      `json:"probes,omitempty"`
	RateUsage map[string]rateUsageObservation `json:"rateUsage,omitempty"`
}

var (
	modelsMu           sync.RWMutex
	catalogModels      = map[string][]catalogModel{}
	modelProbes        = map[string]modelPriceProbe{}
	dynamicSource      string
	modelRateUsage     = map[string]rateUsageObservation{}
	rateSourcesProblem string
	modelsScanTrigger  = make(chan struct{}, 1)
	modelProbeTrigger  = make(chan struct{}, 1)
)

// rateLogState 记录本进程已经写过的「官方倍率」日志签名。
// 首次刷新全量记录，之后只记录发生变化的模型，避免每小时把整份目录重复写进普通日志。
var rateLogState = struct {
	sync.Mutex
	seen map[string]string
}{seen: map[string]string{}}

// logOfficialRates 把官方目录取回的倍率写入普通日志，供网页控制台展示数据来源与更新时间。
// 只记录摘要字段，不涉及任何凭据。
func logOfficialRates(site string, models []catalogModel, refreshedAt time.Time, trace string) {
	rateLogState.Lock()
	defer rateLogState.Unlock()
	changed := 0
	for _, m := range models {
		key := site + "|" + m.ID
		signature := fmt.Sprintf("%s|%t|%g|%t|%s|%s|%s|%t|%t", m.Credits, m.HasMultiplier, m.Multiplier, m.PromoExpired, m.PromoLabel, m.PromoFrom, m.PromoUntil, m.FromLive, m.RateCached)
		if previous, ok := rateLogState.seen[key]; ok && previous == signature {
			continue
		}
		rateLogState.seen[key] = signature
		changed++
		source := "npm静态目录"
		if m.FromLive {
			source = "实时接口"
		}
		checked := refreshedAt.Format(time.RFC3339)
		if m.RateCheckedAt > 0 {
			checked = time.UnixMilli(m.RateCheckedAt).Format(time.RFC3339)
		}
		if m.RateCached {
			source = "目录缓存"
			if m.RateCheckedAt == 0 {
				checked = "未知"
			}
		}
		log.Printf("[Models] 官方倍率 站点=%s 模型=%s 原始credits=%q 生效倍率=%s 有倍率=%t 促销=%q 促销截止=%q 促销已过期=%t 来源=%s 刷新时间=%s 促销开始=%q traceId=%s",
			site, m.ID, m.Credits, strings.TrimSuffix(formatRateMultiplier(m.Multiplier), "x"), m.HasMultiplier, m.PromoLabel, m.PromoUntil, m.PromoExpired, source, checked, m.PromoFrom, trace)
	}
	if changed > 0 {
		log.Printf("[Models] 官方倍率已记录：站点=%s 本轮新增或变化=%d 说明=未变化的模型不重复写日志", site, changed)
	}
}

func probeKey(site, model string) string {
	return site + "|" + catalogDisplayModelID(site, model)
}

func formatRateMultiplier(value float64) string {
	if value > 0 && value < 0.005 {
		return strconv.FormatFloat(value, 'f', -1, 64) + "x"
	}
	return fmt.Sprintf("%.2fx", value)
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
		if err == nil && validRateCredit(f) {
			return f, true
		}
	case float64:
		if validRateCredit(x) {
			return x, true
		}
	case int:
		if x >= 0 {
			return float64(x), true
		}
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
	Discount *struct {
		Factor *float64 `json:"factor"`
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
		MaxAllowedSize  int    `json:"maxAllowedSize"`
		ContextWindow   *struct {
			SupportedLengths []int `json:"supportedLengths"`
			DefaultLength    int   `json:"defaultLength"`
		} `json:"contextWindow"`
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
	type catalogSource struct {
		path   string
		models []catalogModel
		promos int
	}
	var sources []catalogSource
	var failures []string
	for _, path := range []string{catalogV3Path, modelsPath} {
		data, status, err := fetchCatalogJSON(auth, prof, path)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s HTTP %d: %v", path, status, err))
			log.Printf("[Models] 目录源失败：站点=%s 路径=%s 原因=%v", prof.Key, path, err)
			continue
		}
		models, promos, err := parseLiveCatalog(data)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			log.Printf("[Models] 目录源解析失败：站点=%s 路径=%s 原因=%v", prof.Key, path, err)
			continue
		}
		sources = append(sources, catalogSource{path: path, models: models, promos: promos})
	}
	if len(sources) == 0 {
		return nil, 0, fmt.Errorf("实时模型目录全部失败：%s", strings.Join(failures, " | "))
	}
	merged := sources[0].models
	active := sources[0].promos
	for _, source := range sources[1:] {
		merged = mergeCatalogs(prof.Key, merged, source.models)
		if source.promos > active {
			active = source.promos
		}
	}
	return merged, active, nil
}

// fetchCatalogJSON 按目录端点补齐身份头。官方桌面端的真实目录 /v3/config
// 对 User-Agent 和 X-IDE-* 有要求，企业窄表 /v2/enterprises/personal/models
// 仍沿用原 CLI 头，避免改变既有兼容性。
func fetchCatalogJSON(auth *StoredAuth, prof *upstreamProfile, path string) (json.RawMessage, int, error) {
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		if path == catalogV3Path {
			r.Header.Set("Accept", "application/json")
			if prof.Key == "intl" {
				r.Header.Set("User-Agent", catalogINTLUserAgent)
			} else {
				r.Header.Set("User-Agent", catalogCNUserAgent)
			}
			for name, value := range map[string]string{
				"X-IDE-Type":          "WorkBuddy",
				"X-IDE-Name":          "WorkBuddy",
				"X-IDE-Version":       "5.8.3",
				"X-Private-Data":      "true",
				"X-Codebuddy-Request": "1",
				"X-Agent-Intent":      "craft",
				"X-Agent-Purpose":     "conversation",
				"X-Agent-Type":        "main",
				"X-Product":           "SaaS",
			} {
				r.Header.Set(name, value)
			}
			if auth.Account.UID != "" {
				r.Header.Set("X-User-Id", auth.Account.UID)
			} else {
				r.Header.Set("X-No-User-Id", "1")
			}
			if auth.Account.EnterpriseID != "" && auth.Account.UID != "" {
				r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
			} else {
				r.Header.Set("X-No-Enterprise-Id", "1")
			}
			if auth.Auth.Domain != "" {
				r.Header.Set("X-Domain", auth.Auth.Domain)
			} else {
				r.Header.Set("X-No-Department-Info", "1")
			}
			return
		}
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
	endpoint := strings.TrimRight(strings.TrimSpace(prof.Base), "/") + path
	return doJSON(cfg.HttpClient, http.MethodGet, endpoint, headers, nil)
}

func promoActive(p livePromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if p.Schedule.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, p.Schedule.ValidFrom); err != nil || now.Before(from) {
			return false
		}
	}
	if p.Schedule.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, p.Schedule.ValidUntil); err != nil || !now.Before(until) {
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
		from    string
		expired bool
	}
	hits := map[string]hit{}
	active := 0
	for _, p := range doc.ModelPromotions {
		// /v3/config 里还有只用于展示的“夜间折扣”徽章，可能没有 discount 数值；
		// 这类促销不能当作 factor=0 的限免，否则会把官方基础倍率错误改成 0.00x。
		if p.Discount == nil || p.Discount.Factor == nil || !p.Enabled || !validRateCredit(*p.Discount.Factor) {
			continue
		}
		factor := *p.Discount.Factor
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
				if old, ok := hits[id]; ok && !old.expired && old.factor <= factor {
					continue
				}
				hits[id] = hit{factor: factor, label: label, until: p.Schedule.ValidUntil, from: p.Schedule.ValidFrom}
			} else {
				// 记录"存在促销但当前不生效"，用于触发价格探测。
				if _, ok := hits[id]; ok {
					continue
				}
				hits[id] = hit{factor: factor, label: label, until: p.Schedule.ValidUntil, from: p.Schedule.ValidFrom, expired: true}
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
		var lengths []int
		var defaultLength int
		if m.ContextWindow != nil {
			lengths, defaultLength = m.ContextWindow.SupportedLengths, m.ContextWindow.DefaultLength
		}
		applyCatalogCapability(&entry, m.MaxAllowedSize, lengths, defaultLength)
		if s, ok := m.Credits.(string); ok {
			entry.Credits = strings.TrimSpace(s)
		} else if m.Credits != nil {
			entry.Credits = fmt.Sprint(m.Credits)
		}
		base, hasBase := parseCredits(m.Credits)
		entry.BaseMultiplier = base
		entry.HasMultiplier = hasBase
		entry.Multiplier = base
		if h, ok := hits[id]; ok {
			entry.PromoLabel = h.label
			entry.PromoUntil = h.until
			entry.PromoFrom = h.from
			factor := h.factor
			entry.PromoFactor = &factor
			if h.expired {
				entry.PromoExpired = true
			} else {
				entry.Multiplier = base * h.factor
				if !hasBase {
					entry.Multiplier = 0
				}
				entry.HasMultiplier = hasBase || h.factor == 0
				if h.factor == 0 {
					entry.PromoFree = true
				}
			}
		}
		if !validRateCredit(entry.Multiplier) {
			entry.Multiplier, entry.HasMultiplier = 0, false
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
	traceID := newTraceID()
	log.Printf("[模型目录] traceId=%s 来源=npm 阶段=版本查询开始 镜像数=%d", traceID, len(npmBases))
	var lastErr error
	for _, base := range npmBases {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest", nil)
		if reqErr != nil {
			cancel()
			lastErr = reqErr
			log.Printf("[模型目录] traceId=%s 来源=npm 镜像=%s 阶段=创建请求 结果=失败 原因=%v", traceID, base, reqErr)
			continue
		}
		resp, err := cfg.HttpClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			log.Printf("[模型目录] traceId=%s 来源=npm 镜像=%s 阶段=外部请求 结果=失败 原因=%v", traceID, base, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			log.Printf("[模型目录] traceId=%s 来源=npm 镜像=%s 状态码=%d 阶段=读取响应 结果=失败 原因=%v", traceID, base, resp.StatusCode, readErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			log.Printf("[模型目录] traceId=%s 来源=npm 镜像=%s 结果=失败 原因=%v", traceID, base, lastErr)
			continue
		}
		var m struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &m); err != nil || strings.TrimSpace(m.Version) == "" {
			lastErr = fmt.Errorf("manifest 缺少 version")
			log.Printf("[模型目录] traceId=%s 来源=npm 镜像=%s 阶段=解析版本 结果=失败 原因=%v", traceID, base, lastErr)
			continue
		}
		log.Printf("[模型目录] traceId=%s 来源=npm 结果=成功 版本=%s 说明=响应体已读取并关闭，再释放请求上下文", traceID, strings.TrimSpace(m.Version))
		return strings.TrimSpace(m.Version), nil
	}
	log.Printf("[模型目录] traceId=%s 来源=npm 结果=查询失败 原因=%v 业务影响=继续由现有目录兜底逻辑处理", traceID, lastErr)
	return "", lastErr
}

func parseNPMCatalog(data []byte) ([]catalogModel, error) {
	var doc struct {
		Models *[]struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			Credits         any    `json:"credits"`
			MaxInputTokens  int    `json:"maxInputTokens"`
			MaxOutputTokens int    `json:"maxOutputTokens"`
			MaxAllowedSize  int    `json:"maxAllowedSize"`
			ContextWindow   *struct {
				SupportedLengths []int `json:"supportedLengths"`
				DefaultLength    int   `json:"defaultLength"`
			} `json:"contextWindow"`
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
		entry := catalogModel{
			ID: id, Name: strings.TrimSpace(m.Name),
			MaxInputTokens: m.MaxInputTokens, MaxOutputTokens: m.MaxOutputTokens,
		}
		// npm 目录同样带 maxInputTokens/maxOutputTokens/contextWindow；
		// 此前只读 id/name/credits，能力信息被整段丢弃，这里补齐。
		var lengths []int
		var defaultLength int
		if m.ContextWindow != nil {
			lengths, defaultLength = m.ContextWindow.SupportedLengths, m.ContextWindow.DefaultLength
		}
		applyCatalogCapability(&entry, m.MaxAllowedSize, lengths, defaultLength)
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
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cfg.HttpClient.Do(req)
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
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cfg.HttpClient.Do(req)
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

// normalizeContextLengths 过滤非法档位并按升序去重。
// 超过模型能力上限的档位不可信（客户端也据此过滤），直接丢弃。
func normalizeContextLengths(values []int, ceiling int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(values))
	for _, v := range values {
		if v <= 0 || seen[v] {
			continue
		}
		if ceiling > 0 && v > ceiling {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// applyCatalogCapability 把目录里的能力字段写进条目。
// 能力上限优先取 maxInputTokens，缺失时回落到上游声明的 maxAllowedSize；
// 档位与默认预算只在可信时保留，绝不编造默认值。
func applyCatalogCapability(entry *catalogModel, maxAllowed int, lengths []int, defaultLength int) {
	if entry == nil {
		return
	}
	if maxAllowed > 0 {
		entry.MaxAllowedSize = maxAllowed
	}
	ceiling := entry.MaxInputTokens
	if ceiling <= 0 {
		ceiling = entry.MaxAllowedSize
	}
	if normalized := normalizeContextLengths(lengths, ceiling); len(normalized) >= 2 {
		entry.ContextLengths = normalized
	}
	if defaultLength > 0 {
		if ceiling > 0 && defaultLength > ceiling {
			defaultLength = ceiling
		}
		entry.ContextDefaultLength = defaultLength
	}
}

// backfillCapability 只用次要来源补齐缺失的能力字段；已有值永远不被覆盖。
func backfillCapability(dst *catalogModel, src catalogModel) {
	if dst == nil {
		return
	}
	if dst.MaxInputTokens <= 0 {
		dst.MaxInputTokens = src.MaxInputTokens
	}
	if dst.MaxOutputTokens <= 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
	if dst.MaxAllowedSize <= 0 {
		dst.MaxAllowedSize = src.MaxAllowedSize
	}
	if len(dst.ContextLengths) == 0 {
		dst.ContextLengths = append([]int(nil), src.ContextLengths...)
	}
	if dst.ContextDefaultLength <= 0 {
		dst.ContextDefaultLength = src.ContextDefaultLength
	}
}

// mergeCatalogs 合并两份目录：实时接口优先，npm 仅补齐接口没有的模型。
// 同一模型两边都有时，用 npm 补齐实时目录缺失的能力字段（不覆盖实时值）。
func mergeCatalogs(site string, live, npm []catalogModel) []catalogModel {
	out := make([]catalogModel, 0, len(live)+len(npm))
	index := map[string]int{}
	for _, m := range live {
		key := catalogDisplayModelID(site, m.ID)
		if _, ok := index[key]; ok {
			continue
		}
		index[key] = len(out)
		out = append(out, m)
	}
	for _, m := range npm {
		key := catalogDisplayModelID(site, m.ID)
		if i, ok := index[key]; ok {
			backfillCapability(&out[i], m)
			continue
		}
		index[key] = len(out)
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
		for i := range models {
			models[i].RateCached = true
			if models[i].RateCheckedAt == 0 {
				if at, parseErr := time.ParseInLocation("2006-01-02 15:04", strings.TrimPrefix(cache.Source, "live-api@"), time.Local); parseErr == nil {
					models[i].RateCheckedAt = at.UnixMilli()
				}
			}
		}
		validated[key] = mergeCatalogs(key, models, nil)
	}
	modelsMu.Lock()
	if len(validated) > 0 {
		catalogModels = validated
	}
	modelProbes = map[string]modelPriceProbe{}
	for key, probe := range cache.Probes {
		if site, model, ok := strings.Cut(key, "|"); ok {
			probe.Cached = true
			key = probeKey(site, model)
			if old, exists := modelProbes[key]; !exists || old.LastProbeAt <= probe.LastProbeAt {
				modelProbes[key] = probe
			}
		}
	}
	dynamicSource = cache.Source
	modelRateUsage = map[string]rateUsageObservation{}
	for key, observation := range cache.RateUsage {
		site, model, ok := strings.Cut(key, "|")
		if ok && observation.valid() {
			key = probeKey(site, model)
			if old := modelRateUsage[key]; old.At <= observation.At {
				modelRateUsage[key] = observation
			}
		}
	}
	modelsMu.Unlock()
	rateSourcesRevision.Add(1)
	for key, models := range validated {
		log.Printf("[Models] 已加载站点目录缓存：站点=%s，模型数=%d", key, len(models))
	}
	// 缓存里的探测结论同样写入普通日志：网页控制台据此说明「收费(倍率未知)」的来源与时间。
	// 明确标注为缓存载入，避免被误读成本轮新探测。
	for key, probe := range cache.Probes {
		if probe.Verdict == "" {
			continue
		}
		site, model, found := strings.Cut(key, "|")
		if !found || site == "" || model == "" {
			continue
		}
		probeTime := "未知"
		observedAt := probe.VerdictAt
		if observedAt == 0 {
			observedAt = probe.LastProbeAt
		}
		if observedAt > 0 {
			probeTime = time.Unix(observedAt, 0).Format(time.RFC3339)
		}
		log.Printf("[ModelPrice] 载入缓存结论 站点=%s 模型=%s 判定=%s credit=%s tokens=%d 探测时间=%s 说明=%s 来源=缓存文件（非本轮新探测）",
			site, model, probe.Verdict, formatQuota(probe.Credit), probe.Tokens, probeTime, probe.Detail)
	}
}

func saveModelsCache(source string, catalogs map[string][]catalogModel, probes map[string]modelPriceProbe) error {
	data, err := json.MarshalIndent(modelsCache{
		Schema: modelsCacheSchema, Source: source, FetchedAt: time.Now().Unix(),
		Catalogs: catalogs, Probes: probes,
		RateUsage: modelRateUsage,
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

func persistModelsLocked(source string) error {
	err := saveModelsCache(source, catalogModels, modelProbes)
	rateSourcesProblem = ""
	if err != nil {
		rateSourcesProblem = "倍率来源仅保存在内存，缓存落盘失败；重启后可能需要重新确认"
		log.Printf("[Models] 缓存写入失败: %v", err)
	}
	return err
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
	return collectModelIDs(false)
}

func collectModelIDs(includeDisabled bool) ([]string, string) {
	catalogs, source := catalogSnapshot()
	seen := map[string]bool{}
	ids := make([]string, 0, 64)
	for _, site := range catalogSites {
		for _, m := range catalogs[site] {
			displayID := catalogDisplayModelID(site, m.ID)
			if seen[displayID] {
				continue
			}
			// 被配置禁用的模型不出现在任何模型列表（/v1/models、monitor 统计表、health 计数）。
			if disabled, _ := modelDisabled(displayID); disabled && !includeDisabled {
				continue
			}
			seen[displayID] = true
			ids = append(ids, displayID)
		}
	}
	return ids, source
}

// catalogDisplayModelID maps international catalog variants back to the client model name.
// Official /v3/config returns deepseek-v4.1-flash-sg, while clients send deepseek-v4.1-flash.
func catalogDisplayModelID(site, modelID string) string {
	id := normalizeModelName(modelID)
	if site == "intl" && strings.HasSuffix(id, "-sg") {
		return strings.TrimSuffix(id, "-sg")
	}
	return id
}

func modelEntry(site, modelID string) (catalogModel, bool) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	modelID = normalizeModelName(modelID)
	for _, m := range catalogModels[site] {
		if catalogDisplayModelID(site, m.ID) == catalogDisplayModelID(site, modelID) {
			return m, true
		}
	}
	return catalogModel{}, false
}

// modelCapabilitySite 是单个站点对某模型声明的能力。
type modelCapabilitySite struct {
	Ceiling   int   `json:"context_length,omitempty"`         // 能力上限（maxInputTokens，缺失回落 maxAllowedSize）
	MaxOutput int   `json:"max_output_tokens,omitempty"`      // 输出上限
	Default   int   `json:"default_context_length,omitempty"` // 客户端默认预算（不是能力上限）
	Options   []int `json:"context_length_options,omitempty"` // 客户端可选档位
}

// modelCapability 是 /v1/models 对外暴露的能力汇总。
// 只有各站点一致（或仅一个站点有可信数据）时才填顶层标量；
// 站点之间有差异时顶层留空并填 BySite，避免下游按较大值预留上下文。
type modelCapability struct {
	Ceiling   int
	MaxOutput int
	Default   int
	Options   []int
	BySite    map[string]modelCapabilitySite
}

func sameIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// capabilityScalar 判断某标量能力在各站点是否一致。
// 任一站点缺该值即视为「不可保证」，返回 false（未知不能冒充一致）。
func capabilityScalar(bySite map[string]modelCapabilitySite, pick func(modelCapabilitySite) int) (int, bool) {
	value, found := 0, false
	for _, site := range catalogSites {
		v, ok := bySite[site]
		if !ok {
			continue
		}
		got := pick(v)
		if got <= 0 {
			return 0, false
		}
		if found && got != value {
			return 0, false
		}
		value, found = got, true
	}
	return value, found
}

func capabilityOptions(bySite map[string]modelCapabilitySite) ([]int, bool) {
	var first []int
	for _, site := range catalogSites {
		v, ok := bySite[site]
		if !ok {
			continue
		}
		if len(v.Options) == 0 {
			return nil, false
		}
		if first == nil {
			first = v.Options
			continue
		}
		if !sameIntSlice(first, v.Options) {
			return nil, false
		}
	}
	return first, len(first) >= 2
}

// modelCapabilityFor 汇总各站点目录中该模型的能力信息。
//
// 同名模型可能由国内／国际账号分别服务，能力不一定相同。这里不取最大值：
// 顶层标量只在各站点一致时给出，否则置空并把分站点明细放进 BySite，
// 由调用方决定如何展示。完全没有可信数据的模型返回 false，由调用方省略字段。
func modelCapabilityFor(modelID string) (modelCapability, bool) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	modelID = normalizeModelName(modelID)
	bySite := map[string]modelCapabilitySite{}
	for _, site := range catalogSites {
		for _, m := range catalogModels[site] {
			if m.ID != modelID {
				continue
			}
			ceiling := m.MaxInputTokens
			if ceiling <= 0 {
				ceiling = m.MaxAllowedSize
			}
			site0 := modelCapabilitySite{
				Ceiling: ceiling, MaxOutput: m.MaxOutputTokens,
				Default: m.ContextDefaultLength, Options: append([]int(nil), m.ContextLengths...),
			}
			if site0.Ceiling <= 0 && site0.MaxOutput <= 0 && site0.Default <= 0 && len(site0.Options) == 0 {
				break // 该站点没有可信能力数据，不参与汇总
			}
			bySite[site] = site0
			break
		}
	}
	if len(bySite) == 0 {
		return modelCapability{}, false
	}
	cap := modelCapability{}
	cap.Ceiling, _ = capabilityScalar(bySite, func(v modelCapabilitySite) int { return v.Ceiling })
	cap.MaxOutput, _ = capabilityScalar(bySite, func(v modelCapabilitySite) int { return v.MaxOutput })
	cap.Default, _ = capabilityScalar(bySite, func(v modelCapabilitySite) int { return v.Default })
	if opts, ok := capabilityOptions(bySite); ok {
		cap.Options = opts
	}
	if len(bySite) > 1 {
		// 顶层有任一字段没能统一时，给出分站点明细，避免信息丢失。
		for _, site := range catalogSites {
			v, ok := bySite[site]
			if !ok {
				continue
			}
			if v.Ceiling != cap.Ceiling || v.MaxOutput != cap.MaxOutput ||
				v.Default != cap.Default || !sameIntSlice(v.Options, cap.Options) {
				cap.BySite = bySite
				break
			}
		}
	}
	return cap, true
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
	return resolveModelRate(site, modelID, freeLabel, time.Now()).Value
}

// siteKnownFree 判断某站点该模型是否已确认免费（实测或接口明确 0.00x）。
func siteKnownFree(site, modelID string) bool {
	if modelProbeVerdict(site, modelID) == "free" {
		return true
	}
	accountFree := func() (knownFree bool) {
		accountMu.Lock()
		skipped := 0
		defer func() {
			accountMu.Unlock()
			if skipped > 0 {
				log.Printf("[站点免费判断] 站点=%s 模型=%s 跳过不可调度账号=%d 有效账号免费记录=%t 说明=空账号、失效标记及缺少凭据的账号不参与判断",
					site, modelID, skipped, knownFree)
			}
		}()
		for _, acc := range accounts {
			// 启动时会载入 .disabled 标记形成无 Auth 的占位账号；
			// 它们不能参与调度，也不能因历史免费状态影响站点选择。
			if acc == nil || acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
				skipped++
				continue
			}
			if acc.Profile().Key != site && acc.Edition != site {
				continue
			}
			if st := acc.ModelStates[normalizeModelName(modelID)]; st != nil && st.CostClass == modelCostFree {
				return true
			}
		}
		return false
	}()
	if accountFree {
		return true
	}
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

func refreshModelsOnce(schedule ...time.Time) {
	next := time.Time{}
	if len(schedule) > 0 {
		next = schedule[0]
	}
	trace, started := startScheduledTask("model_catalog", next)
	version, verErr := fetchNPMCatalogVersion()
	if verErr != nil {
		log.Printf("[Models] npm 版本查询失败（仅影响 npm 兜底来源）: %v", verErr)
	}

	prev, prevSource := catalogSnapshot()
	fetched := map[string][]catalogModel{}
	var failures []string
	usedCache := false

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
			fetched[site] = mergeCatalogs(site, live, npm)
		case liveOK:
			fetched[site] = live
		case npmOK:
			if cached := prev[site]; len(cached) > 0 {
				fetched[site] = mergeCatalogs(site, cached, npm)
				cachedIDs := make(map[string]bool, len(cached))
				for _, model := range cached {
					cachedIDs[catalogDisplayModelID(site, model.ID)] = true
				}
				for i := range fetched[site] {
					fetched[site][i].RateCached = cachedIDs[catalogDisplayModelID(site, fetched[site][i].ID)]
				}
				usedCache = true
			} else {
				fetched[site] = npm
			}
		default:
			if cached, ok := prev[site]; ok && len(cached) > 0 {
				fetched[site] = cached
				for i := range fetched[site] {
					fetched[site][i].RateCached = true
				}
				usedCache = true
				log.Printf("[Models] 站点=%s 两路来源均失败，沿用缓存（模型数=%d）", site, len(cached))
			} else {
				log.Printf("[Models] 站点=%s 两路来源均失败且无缓存，该站点本次不展示模型", site)
			}
		}
		stampCatalogRates(site, fetched[site], prev[site], time.Now())
	}

	// 能力信息覆盖情况：只在目录刷新时记录一次，不随 /v1/models 逐次打印，
	// 便于人工核对「哪些模型有可信上下文/输出能力、哪些站点还缺」。
	for _, site := range catalogSites {
		models, ok := fetched[site]
		if !ok {
			continue
		}
		ceiling, options, output := 0, 0, 0
		for _, m := range models {
			if m.MaxInputTokens > 0 || m.MaxAllowedSize > 0 {
				ceiling++
			}
			if len(m.ContextLengths) >= 2 {
				options++
			}
			if m.MaxOutputTokens > 0 {
				output++
			}
		}
		log.Printf("[模型能力] traceId=%s 站点=%s 模型数=%d 有上下文上限=%d 有可选档位=%d 有输出上限=%d 说明=仅记录目录覆盖情况，不注入上游参数、不改变输出预算", trace, site, len(models), ceiling, options, output)
	}

	if len(fetched) == 0 {
		logScheduledTask("model_catalog", "completed", "failed", trace, started, next, "两路目录都不可用，保留原状态")
		log.Printf("[Models] 无任何可用目录数据，保留原状态；失败详情: %s", strings.Join(failures, " | "))
		return
	}

	source := time.Now().Format("2006-01-02 15:04")
	if usedCache && prevSource != "" {
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
	cacheErr := persistModelsLocked(source)
	modelsMu.Unlock()
	rateSourcesRevision.Add(1)
	refreshedAt := time.Now()
	for _, site := range catalogSites {
		if models, ok := fetched[site]; ok && len(models) > 0 {
			logOfficialRates(site, models, refreshedAt, trace)
		}
	}
	requestModelsProbe()
	status := "success"
	if len(failures) > 0 || verErr != nil || cacheErr != nil {
		status = "partial"
	}
	logScheduledTask("model_catalog", "completed", status, trace, started, next, fmt.Sprintf("可用目录=%d，来源失败=%d，npm版本查询失败=%t，缓存写入失败=%t", len(fetched), len(failures), verErr != nil, cacheErr != nil))
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
	next := time.Now().Add(interval)
	logScheduledTask("model_catalog", "scheduled", "", newTraceID(), time.Time{}, next, "目录刷新完成后开始定时间隔")
	for {
		select {
		case tick := <-ticker.C:
			next = tick.Add(interval)
			refreshModelsOnce(next)
		case <-modelsScanTrigger:
			log.Printf("[Models] 账号池发生变化，立即刷新模型目录")
			refreshModelsOnce(next)
		}
	}
}

// modelPriceProbeLoop 周期性为「需要确认价格」的模型做真实探测。
// modelPriceProbeLoop 周期性执行价格探测。
// 首轮在启动后很快执行；只要仍有待探测模型就用较短间隔追赶，收敛后回到长间隔。
func modelPriceProbeLoop() {
	trace := newTraceID()
	if !cfg.automaticPriceProbesAllowed() {
		reason := "默认关闭，未显式指定-allow-price-probes=true"
		if cfg.DisablePriceProbes {
			reason = "兼容旧参数-disable-price-probes=true，禁用优先"
		}
		logScheduledTask("price_probe", "disabled", "disabled", trace, time.Time{}, time.Time{}, reason+"；不会自动请求模型，不消耗后台价格探测积分")
		log.Printf("[ModelPrice] traceId=%s 阶段=自动探测调度 allowPriceProbes=%t legacyDisablePriceProbes=%t 结果=已关闭 原因=%s 业务影响=不会自动请求模型，客户端请求、目录刷新和显式probe不受影响",
			trace, cfg.AllowPriceProbes, cfg.DisablePriceProbes, reason)
		return
	}
	log.Printf("[ModelPrice] traceId=%s 阶段=自动探测调度 allowPriceProbes=%t legacyDisablePriceProbes=%t 结果=已开启 原因=显式指定-allow-price-probes 费用警告=将发起真实模型生成请求，可能消耗积分；没有累计积分预算限制",
		trace, cfg.AllowPriceProbes, cfg.DisablePriceProbes)
	timer := time.NewTimer(modelPriceProbeStartDelay)
	defer timer.Stop()
	logScheduledTask("price_probe", "scheduled", "", trace, time.Time{}, time.Now().Add(modelPriceProbeStartDelay), "显式开启，真实请求可能消耗积分；启动后20秒首轮；无候选模型时不生成请求")
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
		logScheduledTask("price_probe", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(next), "按待探测情况2分钟或30分钟；同站点模型受12小时限制")
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
			if pickProbeAccountForModel(site, id) == nil {
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
	trace, started := startScheduledTask("price_probe", time.Time{})
	ids, _ := mergedModelIDs()
	if len(ids) == 0 {
		logScheduledTask("price_probe", "completed", "skipped", trace, started, time.Time{}, "目录为空，没有探测请求")
		return
	}
	now := time.Now()
	batch := modelProbeBatchSize()
	done := 0
	failed := 0
	for _, site := range catalogSites {
		if done >= batch {
			break
		}
		for _, id := range ids {
			if done >= batch {
				break
			}
			if !modelNeedsProbe(site, id, modelRequestCount(id)) {
				continue
			}
			acc := pickProbeAccountForModel(site, id)
			if acc == nil {
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
			if p.VerdictAt == 0 && p.Verdict != "" {
				p.VerdictAt = p.LastProbeAt
			}
			p.LastProbeAt = now.Unix()
			if verdict != "" {
				p.Verdict = verdict
				p.VerdictAt = now.Unix()
				p.Credit, p.Tokens, p.Detail = credit, tokens, detail
				p.LastError = ""
				p.FreeUntil = ""
				p.Cached = false
				if verdict == modelCostFree {
					p.FreeUntil = catalogFreeUntilLocked(site, id, now)
				}
			} else {
				p.LastError = truncate(redactSensitiveText(detail), 400)
			}
			modelProbes[probeKey(site, id)] = p
			cacheErr := persistModelsLocked(dynamicSource)
			modelsMu.Unlock()
			rateSourcesRevision.Add(1)
			if verdict == "" || cacheErr != nil {
				failed++
			}
			log.Printf("[ModelPrice] 站点=%s 账号=%s 模型=%s -> verdict=%s credit=%s tokens=%d (%s)",
				site, acc.Path, id, verdict, formatQuota(credit), tokens, detail)
			done++
		}
	}
	if done > 0 {
		writeStatusSnapshot()
	}
	status := "success"
	if done == 0 {
		status = "skipped"
	} else if failed == done {
		status = "failed"
	} else if failed > 0 {
		status = "partial"
	}
	logScheduledTask("price_probe", "completed", status, trace, started, time.Time{}, fmt.Sprintf("探测=%d，结论或缓存异常=%d", done, failed))
}

// pickProbeAccount 选择同站点账号用于价格探测。
// 优先「已知有余额」的账号；没有时退而使用「额度未知」的账号（例如额度接口
// 返回 20017 无权限的账号）——探测若命中 14018 只会被忽略，不会污染判定。
// 仅跳过「已知耗尽」与失效账号。
func pickProbeAccount(site string) *Account {
	return pickProbeAccountForModel(site, "")
}

func pickProbeAccountForModel(site, model string) *Account {
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
		if allowed, reason := modelAccountAllowed(model, acc, accounts); !allowed {
			log.Printf("[ModelPrice] 站点=%s 模型=%s 凭据=%s 结果=跳过 原因=账号名单%s", site, model, filepath.Base(acc.Path), reason)
			continue
		}
		if acc.QuotaKnown {
			if acc.QuotaUnlimited || acc.QuotaRemaining > 0 {
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
	if allowed, reason := modelAccountAllowed(model, acc, accounts); !allowed {
		accountMu.Unlock()
		log.Printf("[ModelPrice] traceId=%s 模型=%s 凭据=%s 结果=跳过 原因=%s 说明=等待账号锁后重新检查热配置，未调用上游", newTraceID(), model, filepath.Base(acc.Path), reason)
		return "", 0, 0, "账号名单排除：" + reason
	}
	if acc.Disabled || acc.Auth == nil {
		accountMu.Unlock()
		return "", 0, 0, "账号不可用"
	}
	auth := *acc.Auth
	prof := acc.Profile()
	accountMu.Unlock()
	if disabled, reason := modelDisabled(model); disabled {
		return "", 0, 0, reason
	}

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
	if !ok || !validRateCredit(credit) {
		return "", 0, 0, "无完整回复的有效 usage.credit，不覆盖"
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
