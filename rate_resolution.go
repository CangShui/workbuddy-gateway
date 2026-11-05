package main

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync/atomic"
	"time"
)

// 倍率数值和详情共用此处的决策。usage.credit 只能证明免费/收费，不能换算成模型倍率。
// 最后一次有效实测跟随模型缓存持久化，不依赖普通日志的保留期或扫描窗口。
type rateUsageObservation struct {
	At        int64    `json:"at"`
	Class     string   `json:"class"`
	Credit    *float64 `json:"credit,omitempty"`
	Tokens    int64    `json:"tokens,omitempty"`
	Request   uint64   `json:"request,omitempty"`
	TraceID   string   `json:"traceId,omitempty"`
	Restored  bool     `json:"restored,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	FreeUntil string   `json:"freeUntil,omitempty"`
}

func (r rateUsageObservation) valid() bool {
	if r.At <= 0 || (r.Class != modelCostFree && r.Class != modelCostPaid) {
		return false
	}
	if r.Credit == nil {
		return r.Restored
	}
	return validRateCredit(*r.Credit) &&
		(r.Class == modelCostPaid && *r.Credit > 0 ||
			r.Class == modelCostFree && *r.Credit == 0 && r.Tokens >= modelFreeMinTokens)
}

var rateSourcesRevision atomic.Uint64

func rateDisplayRevision(now time.Time) string {
	var boundary int64
	modelsMu.RLock()
	for _, entries := range catalogModels {
		for _, entry := range entries {
			for _, value := range []string{entry.PromoFrom, entry.PromoUntil} {
				if at, err := time.Parse(time.RFC3339, value); err == nil && !now.Before(at) {
					boundary = max(boundary, at.UnixMilli())
				}
			}
		}
	}
	modelsMu.RUnlock()
	return fmt.Sprintf("%d:%d", rateSourcesRevision.Load(), boundary)
}

func validRateCredit(credit float64) bool {
	return credit >= 0 && !math.IsNaN(credit) && !math.IsInf(credit, 0)
}

// 调用方持有 modelsMu；只绑定样本产生时尚未结束的已知促销。
func catalogFreeUntilLocked(site, model string, at time.Time) string {
	for _, entry := range catalogModels[site] {
		if catalogDisplayModelID(site, entry.ID) != catalogDisplayModelID(site, model) {
			continue
		}
		if until, err := time.Parse(time.RFC3339, entry.PromoUntil); err == nil && at.Before(until) {
			return entry.PromoUntil
		}
		break
	}
	return ""
}

type modelRateResolution struct {
	Value       string
	Unknown     bool
	Source      string
	Explain     string
	At          int64 // 对外更新时间沿用秒，内部比较使用毫秒。
	CheckedAt   int64
	ChangedAt   int64
	HasSource   bool
	Conflict    bool
	Cached      bool
	OriginalID  string
	Observation rateUsageObservation
	Catalog     catalogModel
	HasCatalog  bool
	Probe       modelPriceProbe
}

func recordRateUsage(site, model string, credit float64, tokens int64, at time.Time, reqID uint64, trace string) {
	if trace == "" {
		trace = newTraceID()
	}
	class := modelCostPaid
	if credit == 0 {
		class = modelCostFree
	}
	entry := rateUsageObservation{At: at.UnixMilli(), Class: class, Credit: &credit, Tokens: tokens, Request: reqID, TraceID: trace}
	if reqID == 0 {
		entry.Kind = "manual_probe"
	}
	if !entry.valid() {
		return
	}
	key := probeKey(site, model)
	modelsMu.Lock()
	previous := modelRateUsage[key]
	if previous.At > entry.At {
		modelsMu.Unlock()
		return
	}
	if class == modelCostFree {
		entry.FreeUntil = catalogFreeUntilLocked(site, model, at)
	}
	modelRateUsage[key] = entry
	err := persistModelsLocked(dynamicSource)
	modelsMu.Unlock()
	rateSourcesRevision.Add(1)
	log.Printf("[倍率来源] traceId=%s requestId=%d 阶段=正常请求实测 站点=%s 模型=%s usage.credit=%s tokens=%d 判定=%s 原判定=%s 保存成功=%t 说明=有效实测每次更新，即使收费属性没有变化也保留最新来源；不推算数值倍率",
		trace, reqID, site, catalogDisplayModelID(site, model), formatQuota(credit), tokens, class, previous.Class, err == nil)
}

// 旧缓存没有完整实测事件时，从已有账号账本恢复“免费/收费”结论和真实观测时间。
// 不补造 credit、请求号或日志；新请求产生后自然覆盖此记录。
func restoreRateUsageFromAccounts() {
	recovered := map[string]rateUsageObservation{}
	accountMu.Lock()
	for _, acc := range accounts {
		if acc == nil || acc.Disabled || acc.Auth == nil {
			continue
		}
		for model, state := range acc.ModelStates {
			if state == nil {
				continue
			}
			value := rateUsageObservation{At: unixOrZero(state.ObservedAt) * 1000, Class: state.CostClass, Restored: true}
			if !value.valid() {
				continue
			}
			key := probeKey(acc.Profile().Key, model)
			if recovered[key].At < value.At {
				recovered[key] = value
			}
		}
	}
	accountMu.Unlock()
	count := 0
	modelsMu.Lock()
	for key, value := range recovered {
		if modelRateUsage[key].At == 0 {
			if site, model, ok := strings.Cut(key, "|"); ok && value.Class == modelCostFree {
				value.FreeUntil = catalogFreeUntilLocked(site, model, time.UnixMilli(value.At))
			}
			modelRateUsage[key] = value
			count++
		}
	}
	var err error
	if count > 0 {
		err = persistModelsLocked(dynamicSource)
	}
	modelsMu.Unlock()
	if count > 0 {
		rateSourcesRevision.Add(1)
		log.Printf("[倍率来源] traceId=%s 阶段=旧账本恢复 恢复站点模型数=%d 保存成功=%t 说明=保留原观测时间，缺少的积分和日志不伪造", newTraceID(), count, err == nil)
	}
}

// 比较价格规则本身，而不是刷新时间：同样的官方价格再次抓到，不能冲掉已确认的冲突。
func sameCatalogRate(a, b catalogModel) bool {
	if a.HasMultiplier != b.HasMultiplier || a.Multiplier != b.Multiplier ||
		a.BaseMultiplier != b.BaseMultiplier || a.FromLive != b.FromLive ||
		a.PromoFrom != b.PromoFrom || a.PromoUntil != b.PromoUntil ||
		(a.PromoFrom == "" && a.PromoUntil == "" && a.PromoExpired != b.PromoExpired) ||
		(a.PromoFactor == nil) != (b.PromoFactor == nil) {
		return false
	}
	return a.PromoFactor == nil || *a.PromoFactor == *b.PromoFactor
}

func stampCatalogRates(site string, fresh, previous []catalogModel, now time.Time) {
	old := make(map[string]catalogModel, len(previous))
	for _, model := range previous {
		old[catalogDisplayModelID(site, model.ID)] = model
	}
	for i := range fresh {
		model := &fresh[i]
		if model.RateCached && model.RateCheckedAt > 0 {
			continue
		}
		model.RateCheckedAt = now.UnixMilli()
		model.RateChangedAt = now.UnixMilli()
		_, known, _, _ := catalogRateAt(*model, now)
		before, exists := old[catalogDisplayModelID(site, model.ID)]
		if !known {
			// 缺失或无效数据不是新的计费事实，不能据此抹掉有效实测。
			model.RateChangedAt = before.RateChangedAt
		} else if exists && sameCatalogRate(before, *model) {
			model.RateChangedAt = before.RateChangedAt
		}
	}
}

// catalogRateAt 在读取时检查促销边界，不能等下次定时抓取才结束免费。
// credits=0 可能已是固化的促销价；没有可信正数原价时，过期后按未知处理。
func catalogRateAt(model catalogModel, now time.Time) (value float64, known bool, changed int64, explanation string) {
	value, known, changed = model.Multiplier, model.HasMultiplier, model.RateChangedAt
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		known = false
	}
	outside := model.PromoExpired
	hasBoundary := false
	if model.PromoFrom != "" {
		if at, err := time.Parse(time.RFC3339, model.PromoFrom); err == nil {
			hasBoundary = true
			if now.Before(at) {
				outside = true
			} else {
				changed = max(changed, at.UnixMilli())
				outside = false
			}
		}
	}
	if model.PromoUntil != "" {
		if at, err := time.Parse(time.RFC3339, model.PromoUntil); err == nil {
			hasBoundary = true
			if !now.Before(at) {
				outside = true
				changed = max(changed, at.UnixMilli())
			} else if model.PromoFrom == "" {
				outside = false
			}
		}
	}
	if outside {
		if model.BaseMultiplier > 0 && !math.IsInf(model.BaseMultiplier, 0) {
			return model.BaseMultiplier, true, changed, "促销当前不生效，采用官方目录的正数基础倍率"
		}
		return 0, false, changed, "促销尚未开始或已经结束，目录没有可信的非零原价"
	}
	if model.PromoFactor != nil && hasBoundary {
		base, hasBase := parseCredits(model.Credits)
		factor := *model.PromoFactor
		if factor < 0 || math.IsNaN(factor) || math.IsInf(factor, 0) {
			return 0, false, changed, "官方促销折扣无效"
		}
		if hasBase {
			value, known = base*factor, true
		} else if factor == 0 {
			value, known = 0, true
		} else {
			known = false
		}
	}
	return value, known && validRateCredit(value), changed, ""
}

func resolveModelRate(site, id, legacyClass string, now time.Time) modelRateResolution {
	modelsMu.RLock()
	var catalog catalogModel
	hasCatalog := false
	for _, entry := range catalogModels[site] {
		if catalogDisplayModelID(site, entry.ID) == catalogDisplayModelID(site, id) {
			catalog, hasCatalog = entry, true
			break
		}
	}
	probe := modelProbes[probeKey(site, id)]
	usage := modelRateUsage[probeKey(site, id)]
	modelsMu.RUnlock()
	return resolveRateEvidence(catalog, hasCatalog, probe, usage, legacyClass, now)
}

func resolveRateEvidence(catalog catalogModel, hasCatalog bool, probe modelPriceProbe, usage rateUsageObservation, legacyClass string, now time.Time) modelRateResolution {
	r := modelRateResolution{
		Value: "-", Unknown: true, HasSource: hasCatalog || usage.valid() || probe.Verdict == modelCostFree || probe.Verdict == modelCostPaid,
		CheckedAt: catalog.RateCheckedAt / 1000, ChangedAt: catalog.RateChangedAt / 1000,
		Cached: catalog.RateCached, OriginalID: catalog.ID, Catalog: catalog, HasCatalog: hasCatalog,
		Observation: usage,
		Probe:       probe,
	}
	value, known, priceChangedAt, promoExplanation := catalogRateAt(catalog, now)
	if !catalog.FromLive {
		priceChangedAt = 0 // 静态兜底更新不能取消真实调用的计费证据。
	}
	usageBoundary := priceChangedAt
	if until, err := time.Parse(time.RFC3339, usage.FreeUntil); err == nil && !now.Before(until) {
		usageBoundary = max(usageBoundary, until.UnixMilli())
	}
	if !hasCatalog && usage.valid() && usage.At < usageBoundary {
		r.Source, r.At = "历史实测（已失效）", usage.At/1000
		r.Explain = "旧免费样本关联的促销已经结束，当前没有可信目录倍率，需要新的有效实测"
	}
	if hasCatalog {
		r.Source, r.At = "官方目录（实时接口）", r.CheckedAt
		if !catalog.FromLive {
			r.Source = "npm 静态目录"
		}
		if catalog.RateCached {
			r.Source += " · 缓存"
		}
		if known {
			r.Value, r.Unknown = formatRateMultiplier(value), false
			r.Explain = "采用当前目录倍率；请求积分只能确认免费或收费，不能反推倍率"
		} else {
			r.Explain = "当前目录没有可信倍率，缺失的倍率不当作零"
		}
		if promoExplanation != "" {
			r.Explain = promoExplanation
		} else if catalog.PromoLabel != "" {
			r.Explain = "官方促销 " + catalog.PromoLabel + " 生效后的目录倍率"
		}
	}
	class, at, source := "", int64(0), ""
	if usage.valid() && usage.At >= usageBoundary {
		class, at, source = usage.Class, usage.At, "正常请求实测"
		if usage.Restored {
			source = "历史实测账本（升级恢复）"
		} else if usage.Kind == "manual_probe" {
			source = "手动探测实测"
		}
	}
	probeAt := probe.VerdictAt
	if probeAt == 0 {
		probeAt = probe.LastProbeAt
	}
	probeExpired := false
	if until, err := time.Parse(time.RFC3339, probe.FreeUntil); err == nil && !now.Before(until) && probe.Verdict == modelCostFree {
		probeExpired = true
	}
	if probeExpired && class == "" && !hasCatalog {
		r.Source, r.At = "历史探测（已失效）", probeAt
		r.Explain = "免费探测关联的促销已经结束，当前没有可信目录倍率，需要新的有效实测"
	}
	if !probeExpired && (probe.Verdict == modelCostFree || probe.Verdict == modelCostPaid) &&
		(probeAt == 0 && priceChangedAt == 0 || probeAt*1000 >= priceChangedAt) &&
		(class == "" || probeAt*1000 > at) {
		class, at, source = probe.Verdict, probeAt*1000, "价格探测（缓存结论）"
		if !probe.Cached && probe.VerdictAt > 0 {
			source = "价格探测（最近实测）"
		}
	}
	// 旧统计没有精确事件时保留既有免费/收费信息。只要有新的价格修订，
	// 这些无时间的历史累计就不再覆盖新目录。
	if !usage.valid() && priceChangedAt == 0 && catalog.RateCheckedAt == 0 && legacyClass != "-" && legacyClass != "" {
		source = "历史实测状态（无精确时间）"
		switch legacyClass {
		case "是":
			class = modelCostFree
		case "否", "混合":
			class = modelCostPaid
		}
	}
	switch class {
	case modelCostFree:
		r.Value, r.Unknown, r.HasSource = "0.00x", false, true
		r.Source, r.At = source, at/1000
		r.Conflict = known && value > 0
		r.Explain = "最近有效实测确认免费；只代表该站点本次观测，不推算其他账号的权益"
		if r.Conflict {
			r.Explain += "；与目录 " + formatRateMultiplier(value) + " 不一致，目录记录保留供核对"
		}
	case modelCostPaid:
		r.HasSource = true
		if known && value > 0 {
			r.Explain += "；最近有效实测也确认发生扣费"
		} else {
			r.Value, r.Unknown = "收费(倍率未知)", true
			r.Source, r.At = source, at/1000
			r.Conflict = known && value == 0
			r.Explain = "有效实测已扣费，但当前目录无法提供可信的收费倍率；不会从 usage.credit 猜算倍率"
			if r.Conflict {
				r.Explain += "；目录仍标免费，存在冲突"
			}
		}
	}
	if !r.HasSource {
		r.Source, r.Explain = "", "尚无目录倍率或有效实测来源"
	}
	if usage.valid() && usage.At < usageBoundary {
		r.Explain += "；先前实测早于最近价格规则变化，仅作历史参考"
	}
	if probe.Verdict != "" && probeAt > 0 && probeAt*1000 < priceChangedAt {
		r.Explain += "；先前探测早于最近价格规则变化，仅作历史参考"
	}
	return r
}

func rateResolutionDetail(site, id string, r modelRateResolution, raw map[string]any) map[string]any {
	entry := map[string]any{
		"site": site, "model": catalogDisplayModelID(site, id), "value": r.Value, "unknown": r.Unknown,
		"source": r.Source, "explain": r.Explain, "updatedAt": r.At, "hasSource": r.HasSource,
		"checkedAt": r.CheckedAt, "changedAt": r.ChangedAt, "conflict": r.Conflict, "cached": r.Cached,
		"originalModel": r.OriginalID, "hasLog": false, "records": []rateLogRecord{}, "others": []rateLogRecord{},
	}
	if raw != nil {
		for _, key := range []string{"hasLog", "records", "others"} {
			entry[key] = raw[key]
		}
	}
	if r.Observation.valid() {
		evidence := map[string]any{
			"updatedAt": r.Observation.At / 1000, "class": r.Observation.Class,
			"restored": r.Observation.Restored, "tokens": r.Observation.Tokens,
			"freeUntil": r.Observation.FreeUntil,
		}
		if r.Observation.Credit != nil {
			evidence["credit"] = *r.Observation.Credit
		}
		entry["observation"] = evidence
	}
	if r.HasCatalog {
		entry["catalog"] = map[string]any{
			"model": r.Catalog.ID, "credits": r.Catalog.Credits, "promo": r.Catalog.PromoLabel,
			"promoFrom": r.Catalog.PromoFrom, "promoUntil": r.Catalog.PromoUntil,
		}
	}
	if r.Probe.LastProbeAt > 0 {
		at := r.Probe.VerdictAt
		if at == 0 {
			at = r.Probe.LastProbeAt
		}
		entry["probe"] = map[string]any{
			"updatedAt": at, "lastAttemptAt": r.Probe.LastProbeAt,
			"verdict": r.Probe.Verdict, "lastError": r.Probe.LastError,
		}
	}
	return entry
}

func currentRateDetails(raw map[string]map[string]any, now time.Time) (map[string]map[string]any, string) {
	out := make(map[string]map[string]any, len(raw))
	for key, entry := range raw {
		copy := make(map[string]any, len(entry))
		for name, value := range entry {
			copy[name] = value
		}
		copy["value"], copy["unknown"], copy["hasSource"] = "-", true, true
		copy["source"] = "历史普通日志（不代表当前倍率）"
		copy["explain"] = "仅找到历史日志，当前目录和已保存实测均没有该模型的可信倍率；历史数值不冒充现价"
		out[key] = copy
	}
	keys := map[string]bool{}
	modelsMu.RLock()
	for site, entries := range catalogModels {
		for _, entry := range entries {
			keys[probeKey(site, entry.ID)] = true
		}
	}
	for key := range modelRateUsage {
		keys[key] = true
	}
	for key := range modelProbes {
		keys[key] = true
	}
	problem := rateSourcesProblem
	modelsMu.RUnlock()
	for key := range keys {
		site, id, ok := strings.Cut(key, "|")
		if !ok {
			continue
		}
		key = probeKey(site, id)
		resolution := resolveModelRate(site, id, "-", now)
		out[key] = rateResolutionDetail(site, id, resolution, raw[key])
	}
	return out, problem
}
