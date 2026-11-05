package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func rateTestUsage(at time.Time, credit float64) rateUsageObservation {
	class := modelCostPaid
	if credit == 0 {
		class = modelCostFree
	}
	return rateUsageObservation{At: at.UnixMilli(), Class: class, Credit: &credit, Tokens: 1000}
}

func TestRateResolutionDecisionMatrix(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := now.Add(-time.Hour)
	official := catalogModel{
		ID: "m", Credits: "x0.11", HasMultiplier: true, Multiplier: 0.11, BaseMultiplier: 0.11,
		FromLive: true, RateCheckedAt: now.UnixMilli(), RateChangedAt: old.UnixMilli(),
	}
	free := official
	free.Credits, free.Multiplier, free.BaseMultiplier = "x0.00", 0, 0
	newPaid := official
	newPaid.Multiplier, newPaid.BaseMultiplier, newPaid.RateChangedAt = 0.29, 0.29, now.UnixMilli()
	newFree := free
	newFree.RateChangedAt = now.UnixMilli()
	static := newPaid
	static.FromLive = false
	low := official
	low.Multiplier, low.BaseMultiplier, low.Credits = 0.004, 0.004, "x0.004"
	cases := []struct {
		name     string
		catalog  catalogModel
		has      bool
		probe    modelPriceProbe
		usage    rateUsageObservation
		legacy   string
		want     string
		conflict bool
	}{
		{name: "official paid", catalog: official, has: true, want: "0.11x"},
		{name: "small paid rate is not rounded to free", catalog: low, has: true, want: "0.004x"},
		{name: "actual paid does not invent multiplier", catalog: official, has: true, usage: rateTestUsage(now, 13.5), want: "0.11x"},
		{name: "official free actual paid", catalog: free, has: true, usage: rateTestUsage(now, 1), want: "收费(倍率未知)", conflict: true},
		{name: "official paid actual free", catalog: official, has: true, usage: rateTestUsage(now, 0), want: "0.00x", conflict: true},
		{name: "actual paid without catalog", usage: rateTestUsage(now, 1), want: "收费(倍率未知)"},
		{name: "actual free without catalog", usage: rateTestUsage(now, 0), want: "0.00x"},
		{name: "no evidence is not zero", want: "-"},
		{name: "new catalog paid supersedes old free", catalog: newPaid, has: true, usage: rateTestUsage(old, 0), legacy: "是", want: "0.29x"},
		{name: "new catalog free supersedes old paid", catalog: newFree, has: true, usage: rateTestUsage(old, 1), legacy: "混合", want: "0.00x"},
		{name: "paid multiplier changes without new usage", catalog: newPaid, has: true, usage: rateTestUsage(old, 1), want: "0.29x"},
		{name: "fresh actual verifies new catalog conflict", catalog: newFree, has: true, usage: rateTestUsage(now.Add(time.Second), 1), want: "收费(倍率未知)", conflict: true},
		{name: "static package cannot override real usage", catalog: static, has: true, usage: rateTestUsage(old, 0), want: "0.00x", conflict: true},
		{name: "cached free probe cannot override newer paid request", catalog: official, has: true,
			probe: modelPriceProbe{Verdict: "free", VerdictAt: old.Unix()},
			usage: rateTestUsage(now, 1), want: "0.11x"},
		{name: "new successful probe can supersede old request", catalog: official, has: true,
			probe: modelPriceProbe{Verdict: "paid", VerdictAt: now.Unix()},
			usage: rateTestUsage(old, 0), want: "0.11x"},
		{name: "failed probe attempt cannot make old free verdict fresh", catalog: newPaid, has: true,
			probe: modelPriceProbe{Verdict: "free", VerdictAt: old.Unix(), LastProbeAt: now.Unix(), LastError: "timeout"},
			want:  "0.29x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRateEvidence(tc.catalog, tc.has, tc.probe, tc.usage, tc.legacy, now)
			if got.Value != tc.want || got.Conflict != tc.conflict {
				t.Fatalf("got value=%q conflict=%t explain=%q; want %q conflict=%t",
					got.Value, got.Conflict, got.Explain, tc.want, tc.conflict)
			}
		})
	}
}

func TestRateResolutionPromotionBoundariesWithoutRefresh(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	factor := 0.0
	promo := catalogModel{
		ID: "promo", Credits: "x0.29", HasMultiplier: true, Multiplier: 0, BaseMultiplier: 0.29,
		FromLive: true, PromoFactor: &factor, PromoFrom: now.Format(time.RFC3339),
		PromoUntil: now.Add(time.Hour).Format(time.RFC3339), PromoExpired: true,
		RateChangedAt: now.Add(-time.Hour).UnixMilli(),
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"before promotion", now.Add(-time.Second), "0.29x"},
		{"promotion starts", now, "0.00x"},
		{"during promotion", now.Add(time.Minute), "0.00x"},
		{"promotion ends", now.Add(time.Hour), "0.29x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRateEvidence(promo, true, modelPriceProbe{}, rateUsageObservation{}, "-", tc.at)
			if got.Value != tc.want {
				t.Fatalf("at %v got %q want %q", tc.at, got.Value, tc.want)
			}
		})
	}
	promo.Credits, promo.BaseMultiplier = "x0.00", 0
	oldFree := rateTestUsage(now.Add(time.Minute), 0)
	after := now.Add(2 * time.Hour)
	got := resolveRateEvidence(promo, true, modelPriceProbe{}, oldFree, "是", after)
	if got.Value != "-" {
		t.Fatalf("expired zero catalog and old free usage must not remain free: %+v", got)
	}
	got = resolveRateEvidence(promo, true, modelPriceProbe{}, rateTestUsage(after, 0), "-", after)
	if got.Value != "0.00x" {
		t.Fatal("new valid usage after promotion boundary must still be usable")
	}
}

func TestRateCatalogRevisionChangesOnlyForPricingRules(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	before := []catalogModel{{
		ID: "deepseek-v4.1-flash-sg", HasMultiplier: true, Multiplier: 0, FromLive: true,
		RateCheckedAt: now.Add(-2 * time.Hour).UnixMilli(), RateChangedAt: now.Add(-3 * time.Hour).UnixMilli(),
	}}
	same := append([]catalogModel(nil), before...)
	stampCatalogRates("intl", same, before, now)
	if same[0].RateChangedAt != before[0].RateChangedAt || same[0].RateCheckedAt != now.UnixMilli() {
		t.Fatal("unchanged refresh must preserve original price-change time")
	}
	usage := rateTestUsage(now.Add(-time.Hour), 1)
	if got := resolveRateEvidence(same[0], true, modelPriceProbe{}, usage, "-", now); got.Value != "收费(倍率未知)" {
		t.Fatal("unchanged zero price refresh erased a confirmed conflict")
	}
	changed := append([]catalogModel(nil), same...)
	changed[0].Multiplier = 0.11
	stampCatalogRates("intl", changed, same, now.Add(time.Second))
	if changed[0].RateChangedAt != now.Add(time.Second).UnixMilli() {
		t.Fatal("real price update must establish a new revision")
	}
	if got := resolveRateEvidence(changed[0], true, modelPriceProbe{}, usage, "-", now); got.Value != "0.11x" {
		t.Fatal("new official positive price must supersede prior conflict")
	}
	cached := append([]catalogModel(nil), before...)
	cached[0].RateCached = true
	stampCatalogRates("intl", cached, before, now)
	if cached[0].RateCheckedAt != before[0].RateCheckedAt {
		t.Fatal("cache fallback must not pretend to have fetched fresh data")
	}
	missing := []catalogModel{{ID: "deepseek-v4.1-flash-sg", FromLive: true}}
	stampCatalogRates("intl", missing, before, now)
	if got := resolveRateEvidence(missing[0], true, modelPriceProbe{}, usage, "-", now); got.Value != "收费(倍率未知)" {
		t.Fatal("missing directory credits must not erase valid paid usage")
	}
}

func TestRateUsageSurvivesRestartWithoutOrdinaryLogs(t *testing.T) {
	isolatePolicies(t)
	now := time.Now()
	catalog := []catalogModel{{ID: "deepseek-v4.1-flash-sg", Credits: "x0.11", HasMultiplier: true, Multiplier: 0.11, FromLive: true}}
	stampCatalogRates("intl", catalog, nil, now.Add(-time.Minute))
	modelsMu.Lock()
	catalogModels["intl"] = catalog
	modelsMu.Unlock()
	recordRateUsage("intl", "deepseek-v4.1-flash", 1.25, 1000, now, 1, "rate-restart-test")
	recordRateUsage("intl", "deepseek-v4.1-flash", 1.5, 1100, now.Add(time.Second), 2, "rate-restart-test")
	modelsMu.Lock()
	modelRateUsage = map[string]rateUsageObservation{}
	catalogModels = map[string][]catalogModel{}
	modelsMu.Unlock()
	loadModelsCache()
	r := resolveModelRate("intl", "deepseek-v4.1-flash", "-", now.Add(2*time.Second))
	if r.Value != "0.11x" || r.Observation.Request != 2 || r.Observation.Credit == nil || *r.Observation.Credit != 1.5 {
		t.Fatalf("latest repeated observation was not restored: %+v", r)
	}
	keys, _ := currentRateDetails(nil, now)
	if keys["intl|deepseek-v4.1-flash"]["hasSource"] != true {
		t.Fatal("no ordinary log must not remove persisted provenance")
	}
	raw := aggregateRateDetails([]rateLogRecord{{
		Site: "intl", Model: "deepseek-v4.1-flash-sg", Source: "official", UpdatedAt: now.Unix(), Value: "0.11x",
	}})
	details, _ := currentRateDetails(raw, now)
	if details["intl|deepseek-v4.1-flash"]["hasLog"] != true || details["intl|deepseek-v4.1-flash-sg"] != nil {
		t.Fatal("raw evidence and visible model must use the same international alias")
	}
}

func TestRateDetailFreshResolutionIgnoresLogScanCache(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	now := time.Now().Add(-time.Minute)
	catalog := []catalogModel{{ID: "m", HasMultiplier: true, Multiplier: 0, FromLive: true}}
	stampCatalogRates("cn", catalog, nil, now)
	modelsMu.Lock()
	catalogModels["cn"] = catalog
	modelsMu.Unlock()
	handler := newWebUIHandler()
	read := func(want string) {
		t.Helper()
		w := adminRequest(handler, "GET", "/admin/api/rate-detail", "feature-admin-key", "")
		var doc struct{ Rates map[string]map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil || doc.Rates["cn|m"]["value"] != want {
			t.Fatalf("wanted %q, status=%d body=%s", want, w.Code, w.Body.String())
		}
	}
	read("0.00x")
	recordRateUsage("cn", "m", 1, 1000, time.Now(), 1, "rate-cache-test")
	read("收费(倍率未知)")
	next := append([]catalogModel(nil), catalog...)
	next[0].Multiplier = 0.29
	stampCatalogRates("cn", next, catalog, time.Now().Add(time.Second))
	modelsMu.Lock()
	catalogModels["cn"] = next
	modelsMu.Unlock()
	rateSourcesRevision.Add(1)
	read("0.29x")
}

func TestRateDetailSurvivesScanBudgetAndUnreadableLogs(t *testing.T) {
	isolateAdminFeatures(t)
	resetRateDetailCache(t)
	now := time.Now().Add(-time.Minute)
	catalog := []catalogModel{{ID: "m", HasMultiplier: true, Multiplier: 0.11, FromLive: true}}
	stampCatalogRates("cn", catalog, nil, now)
	modelsMu.Lock()
	catalogModels["cn"] = catalog
	modelsMu.Unlock()
	path := filepath.Join(logDir, "gateway-2026-11-05.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("2026/11/05 10:00:00 unrelated local test\n", 4096)
	for n := int64(0); n < rateLogReadBudget+1024; n += int64(len(padding)) {
		if _, err := io.WriteString(f, padding); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	w := adminRequest(newWebUIHandler(), "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	var payload struct {
		Truncated bool
		Rates     map[string]map[string]any
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &payload) != nil ||
		!payload.Truncated || payload.Rates["cn|m"]["hasSource"] != true || payload.Rates["cn|m"]["value"] != "0.11x" {
		t.Fatalf("scan truncation erased authoritative evidence: %d %s", w.Code, w.Body.String())
	}
	// 不可读目录不能让真实倍率来源整体返回失败。
	rateDetailCache.Lock()
	rateDetailCache.payload = nil
	rateDetailCache.Unlock()
	chdirTemp(t)
	if err := os.WriteFile(logDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	w = adminRequest(newWebUIHandler(), "GET", "/admin/api/rate-detail", "feature-admin-key", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "原始普通日志暂不可读") || !strings.Contains(w.Body.String(), "0.11x") {
		t.Fatalf("unreadable raw logs must not hide current rate evidence: %d %s", w.Code, w.Body.String())
	}
}

func TestRateInvalidOrMissingUsageDoesNotOverwriteKnownEvidence(t *testing.T) {
	isolatePolicies(t)
	acc := &Account{Path: "synthetic-rate.json", Auth: &StoredAuth{Edition: "intl"}}
	observeModelCredit(nil, acc, "m", map[string]any{"credit": 1.0, "total_tokens": 1000}, 1)
	for _, usage := range []map[string]any{
		nil, {}, {"total_tokens": 5000}, {"credit": -1.0}, {"credit": math.NaN()},
		{"credit": math.Inf(1)}, {"credit": 0.0, "total_tokens": 2},
	} {
		observeModelCredit(nil, acc, "m", usage, 2)
	}
	got := resolveModelRate("intl", "m", "-", time.Now())
	if got.Value != "收费(倍率未知)" || got.Observation.Request != 1 {
		t.Fatalf("invalid/missing usage overwrote valid history: %+v", got)
	}
}

func TestRateSourcePersistenceFailureRemainsVisible(t *testing.T) {
	isolatePolicies(t)
	if err := os.Mkdir(modelsCacheFile, 0700); err != nil {
		t.Fatal(err)
	}
	recordRateUsage("cn", "m", 0, 1000, time.Now(), 1, "rate-write-failure")
	details, problem := currentRateDetails(nil, time.Now())
	if details["cn|m"]["value"] != "0.00x" || problem == "" {
		t.Fatal("write failure must retain memory evidence and expose a warning")
	}
	if err := os.Remove(modelsCacheFile); err != nil {
		t.Fatal(err)
	}
	recordRateUsage("cn", "m", 0, 1000, time.Now(), 2, "rate-write-recovery")
	if _, problem = currentRateDetails(nil, time.Now()); problem != "" {
		t.Fatal("successful persistence must clear the previous write warning")
	}
}

func TestRateUpgradeRecoversOriginalObservationTime(t *testing.T) {
	isolatePolicies(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	accounts = []*Account{{
		Path: "synthetic-rate.json", Auth: &StoredAuth{Edition: "intl"},
		ModelStates: map[string]*modelRuntimeState{
			"m": {CostClass: modelCostPaid, ObservedAt: at},
		},
	}}
	restoreRateUsageFromAccounts()
	got := resolveModelRate("intl", "m", "-", time.Now())
	if got.Value != "收费(倍率未知)" || got.At != at.Unix() || got.Observation.Credit != nil ||
		!got.Observation.Restored {
		t.Fatalf("upgrade must not invent current timestamps or credit values: %+v", got)
	}
}

func TestRateUsageAllProtocolsAndCompletionModes(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(route+fmtBool(stream), func(t *testing.T) {
				isolatePolicies(t)
				oldBase, oldOrigin, oldClient := profileCN.Base, profileCN.Origin, cfg.HttpClient
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":1.25,\"total_tokens\":1000}}\n\ndata: [DONE]\n\n")
				}))
				defer upstream.Close()
				profileCN.Base, profileCN.Origin, cfg.HttpClient = upstream.URL, upstream.URL, upstream.Client()
				t.Cleanup(func() { profileCN.Base, profileCN.Origin, cfg.HttpClient = oldBase, oldOrigin, oldClient })
				accounts = []*Account{{
					Path: "synthetic-rate.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{
						AccessToken: "synthetic-rate-not-a-token", ExpiresAt: time.Now().Add(time.Hour).Unix(),
					}},
					QuotaKnown: true, QuotaRemaining: 100,
				}}
				payload := map[string]any{"model": "m", "stream": stream, "messages": []any{msg("user", "test")}}
				if route == "/v1/responses" {
					delete(payload, "messages")
					payload["input"] = "test"
				}
				if route == "/v1/messages" {
					payload["max_tokens"] = 100
				}
				body, _ := json.Marshal(payload)
				w := httptest.NewRecorder()
				newAPIHandler().ServeHTTP(w, httptest.NewRequest("POST", route, strings.NewReader(string(body))))
				if w.Code != 200 {
					t.Fatalf("protocol request failed: %d %s", w.Code, w.Body.String())
				}
				got := resolveModelRate("cn", "m", "-", time.Now())
				if got.Value != "收费(倍率未知)" || got.Observation.Credit == nil || *got.Observation.Credit != 1.25 {
					t.Fatalf("protocol completion did not persist usage evidence: %+v", got)
				}
			})
		}
	}
}

func fmtBool(value bool) string {
	if value {
		return "/stream"
	}
	return "/aggregate"
}

func TestRateSourcesConcurrentReadAndWrite(t *testing.T) {
	isolatePolicies(t)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			recordRateUsage("intl", "m", float64(i%2), 1000, time.Now(), uint64(i), "rate-parallel-test")
			for n := 0; n < 10; n++ {
				currentRateDetails(nil, time.Now())
			}
		}(i)
	}
	group.Wait()
	loadModelsCache()
	if !resolveModelRate("intl", "m", "-", time.Now()).HasSource {
		t.Fatal("concurrent updates lost all observations")
	}
}

func TestRateAliasCatalogSourcesKeepAuthorityOrder(t *testing.T) {
	mainCatalog := []catalogModel{{ID: "deepseek-v4.1-flash-sg", HasMultiplier: true, Multiplier: 0.11, FromLive: true}}
	staleSecondary := []catalogModel{{ID: "deepseek-v4.1-flash", HasMultiplier: true, Multiplier: 0, FromLive: true}}
	merged := mergeCatalogs("intl", mainCatalog, staleSecondary)
	if len(merged) != 1 || merged[0].Multiplier != 0.11 || merged[0].ID != mainCatalog[0].ID {
		t.Fatalf("secondary alias must not replace the authoritative rate: %+v", merged)
	}
}

func TestRatePromotionMetadataAndOrdering(t *testing.T) {
	now := time.Now()
	promotion := func(from, until time.Time, factor any) map[string]any {
		return map[string]any{
			"enabled": true, "modelIds": []string{"m"}, "discount": map[string]any{"factor": factor},
			"schedule": map[string]any{"validFrom": from.Format(time.RFC3339), "validUntil": until.Format(time.RFC3339)},
		}
	}
	active := promotion(now.Add(-time.Hour), now.Add(time.Hour), 0)
	expired := promotion(now.Add(-2*time.Hour), now.Add(-time.Hour), 0)
	missing := map[string]any{"enabled": true, "modelIds": []string{"m"}, "discount": map[string]any{}}
	for _, tc := range []struct {
		name    string
		credits any
		promos  []any
		want    string
	}{
		{"active survives later expired", "x0.29", []any{active, expired}, "0.00x"},
		{"active survives earlier expired", "x0.29", []any{expired, active}, "0.00x"},
		{"missing factor is not free", "x0.29", []any{missing}, "0.29x"},
		{"negative factor is ignored", "x0.29", []any{promotion(now.Add(-time.Hour), now.Add(time.Hour), -1)}, "0.29x"},
		{"factor without base cannot invent multiplier", nil, []any{promotion(now.Add(-time.Hour), now.Add(time.Hour), 0.5)}, "-"},
		{"explicit free factor without base", nil, []any{active}, "0.00x"},
		{"invalid base is not zero", "NaN", nil, "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{"models": []any{map[string]any{"id": "m", "credits": tc.credits}}, "modelPromotions": tc.promos}
			raw, _ := json.Marshal(doc)
			models, _, err := parseLiveCatalog(raw)
			if err != nil {
				t.Fatal(err)
			}
			got := resolveRateEvidence(models[0], true, modelPriceProbe{}, rateUsageObservation{}, "-", now)
			if got.Value != tc.want {
				t.Fatalf("got %q want %q; catalog=%+v", got.Value, tc.want, models[0])
			}
		})
	}
}

func TestRatePromotionClockInvalidatesFrontendRevision(t *testing.T) {
	isolatePolicies(t)
	now := time.Now().Truncate(time.Second)
	modelsMu.Lock()
	catalogModels["cn"] = []catalogModel{{
		ID: "m", PromoFrom: now.Format(time.RFC3339), PromoUntil: now.Add(time.Hour).Format(time.RFC3339),
	}}
	modelsMu.Unlock()
	before := rateDisplayRevision(now.Add(-time.Second))
	during := rateDisplayRevision(now)
	after := rateDisplayRevision(now.Add(time.Hour))
	if before == during || during == after {
		t.Fatal("promotion time boundaries must invalidate cached tooltip values without waiting for a fetch")
	}
}

func TestRateExpiredUsageDoesNotResurrectRemovedModelFreePrice(t *testing.T) {
	now := time.Now()
	usage := rateTestUsage(now.Add(-time.Hour), 0)
	usage.FreeUntil = now.Add(-time.Minute).Format(time.RFC3339)
	got := resolveRateEvidence(catalogModel{}, false, modelPriceProbe{}, usage, "-", now)
	if got.Value != "-" || got.Source != "历史实测（已失效）" {
		t.Fatalf("expired historical sample must not remain current free after directory removal: %+v", got)
	}
}

func TestRateHistoricalLogAloneCannotBecomeCurrentPrice(t *testing.T) {
	isolatePolicies(t)
	raw := aggregateRateDetails([]rateLogRecord{{
		Site: "cn", Model: "removed", Value: "0.00x", Source: "old catalog", UpdatedAt: time.Now().Add(-time.Hour).Unix(),
	}})
	details, _ := currentRateDetails(raw, time.Now())
	if details["cn|removed"]["value"] != "-" || details["cn|removed"]["hasLog"] != true {
		t.Fatal("historical evidence must remain accessible without being presented as current price")
	}
}

func TestRateIncompleteProtocolsKeepPreviousEvidence(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(route+fmtBool(stream), func(t *testing.T) {
				isolatePolicies(t)
				recordRateUsage("cn", "m", 1, 1000, time.Now().Add(-time.Minute), 1, "rate-before-incomplete")
				oldBase, oldOrigin, oldClient := profileCN.Base, profileCN.Origin, cfg.HttpClient
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"credit\":0,\"total_tokens\":1000}}\n\ndata: [DONE]\n\n")
				}))
				defer upstream.Close()
				profileCN.Base, profileCN.Origin, cfg.HttpClient = upstream.URL, upstream.URL, upstream.Client()
				t.Cleanup(func() { profileCN.Base, profileCN.Origin, cfg.HttpClient = oldBase, oldOrigin, oldClient })
				accounts = []*Account{{
					Path: "synthetic-rate.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{
						AccessToken: "synthetic-rate-not-a-token", ExpiresAt: time.Now().Add(time.Hour).Unix(),
					}},
					QuotaKnown: true, QuotaRemaining: 100,
				}}
				payload := map[string]any{"model": "m", "stream": stream, "messages": []any{msg("user", "test")}, "max_tokens": 100}
				if route == "/v1/responses" {
					delete(payload, "messages")
					payload["input"] = "test"
				}
				raw, _ := json.Marshal(payload)
				w := httptest.NewRecorder()
				newAPIHandler().ServeHTTP(w, httptest.NewRequest("POST", route, strings.NewReader(string(raw))))
				got := resolveModelRate("cn", "m", "-", time.Now())
				if got.Value != "收费(倍率未知)" || got.Observation.Request != 1 {
					t.Fatalf("incomplete response changed last valid observation: %+v", got)
				}
			})
		}
	}
}
