package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetModelsStateT(t *testing.T) {
	t.Helper()
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{}
	dynamicSource = ""
	modelsMu.Unlock()
}

func TestParseCredits(t *testing.T) {
	cases := map[string]struct {
		want float64
		ok   bool
	}{
		"x0.29 credits": {0.29, true},
		"x0.00 credits": {0, true},
		"x2.00 credit":  {2.0, true},
		"0.79":          {0.79, true},
		"":              {0, false},
		"n/a":           {0, false},
	}
	for in, want := range cases {
		got, ok := parseCredits(in)
		if ok != want.ok || got != want.want {
			t.Errorf("parseCredits(%q)=%v/%v want %v/%v", in, got, ok, want.want, want.ok)
		}
	}
}

func TestParseLiveCatalogAppliesPromotion(t *testing.T) {
	now := time.Now()
	body := map[string]any{
		"models": []any{
			map[string]any{"id": "hy3", "name": "Hy3", "credits": "x0.20 credits"},
			map[string]any{"id": "deepseek-v4.1-flash", "name": "DS", "credits": "x0.03 credits"},
			map[string]any{"id": "half", "name": "Half", "credits": "x1.00 credits"},
			map[string]any{"id": "expired", "name": "Expired", "credits": "x0.50 credits"},
		},
		"modelPromotions": []any{
			map[string]any{
				"id": "free-hy3", "kind": "discount", "enabled": true, "modelIds": []string{"hy3"},
				"badge":    map[string]any{"label": "Free now"},
				"discount": map[string]any{"factor": 0, "discountedCredits": "0x", "displayMode": "replace"},
				"schedule": map[string]any{"validFrom": now.Add(-24 * time.Hour).Format(time.RFC3339), "validUntil": now.Add(24 * time.Hour).Format(time.RFC3339)},
			},
			map[string]any{
				"id": "half-off", "kind": "discount", "enabled": true, "modelIds": []string{"half"},
				"badge":    map[string]any{"label": "50% off"},
				"discount": map[string]any{"factor": 0.5},
				"schedule": map[string]any{"validFrom": now.Add(-time.Hour).Format(time.RFC3339), "validUntil": now.Add(time.Hour).Format(time.RFC3339)},
			},
			map[string]any{
				"id": "expired-promo", "kind": "discount", "enabled": true, "modelIds": []string{"expired"},
				"discount": map[string]any{"factor": 0},
				"schedule": map[string]any{"validFrom": now.Add(-72 * time.Hour).Format(time.RFC3339), "validUntil": now.Add(-time.Hour).Format(time.RFC3339)},
			},
		},
	}
	data, _ := json.Marshal(body)
	models, activePromos, err := parseLiveCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalogModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if activePromos != 2 {
		t.Fatalf("expected 2 active promos, got %d", activePromos)
	}
	if m := byID["hy3"]; !m.PromoFree || !m.HasMultiplier || m.Multiplier != 0 || m.BaseMultiplier != 0.20 {
		t.Fatalf("hy3 promotion not applied: %+v", m)
	}
	if m := byID["half"]; m.Multiplier != 0.5 || m.PromoFree {
		t.Fatalf("50%% promo should halve multiplier: %+v", m)
	}
	if m := byID["expired"]; m.Multiplier != 0.5 || m.PromoFree {
		t.Fatalf("expired promo must be ignored: %+v", m)
	}
	if m := byID["deepseek-v4.1-flash"]; m.Multiplier != 0.03 {
		t.Fatalf("no promo should keep base: %+v", m)
	}
}

func TestParseLiveCatalogRejectsEmpty(t *testing.T) {
	for _, bad := range []string{`{}`, `{"models":[]}`, `not json`} {
		if _, _, err := parseLiveCatalog([]byte(bad)); err == nil {
			t.Fatalf("expected error for %s", bad)
		}
	}
}

func TestFetchLiveCatalogFromServer(t *testing.T) {
	chdirTemp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != modelsPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"models":[{"id":"hy3","credits":"x0.20 credits"}],"modelPromotions":[]}}`))
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{{Path: "cn.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}}
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	models, _, err := fetchLiveCatalog("cn")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "hy3" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestModelsCacheRoundTripAndMerge(t *testing.T) {
	resetModelsStateT(t)
	chdirTemp(t)
	catalogs := map[string][]catalogModel{
		"cn":   {{ID: "cn-only", Multiplier: 0.23, HasMultiplier: true}, {ID: "shared", Multiplier: 0.06, HasMultiplier: true}},
		"intl": {{ID: "shared", Multiplier: 0.51, HasMultiplier: true}},
	}
	if err := saveModelsCache("2026-09-17 13:24", catalogs, map[string]modelPriceProbe{}); err != nil {
		t.Fatal(err)
	}
	loadModelsCache()
	ids, source := mergedModelIDs()
	if source != "2026-09-17 13:24" {
		t.Fatalf("source=%s", source)
	}
	count := 0
	for _, id := range ids {
		if id == "shared" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected dedupe of shared model, ids=%v", ids)
	}
	if v, ok := modelMultiplier("cn", "cn-only"); !ok || v != 0.23 {
		t.Fatalf("cn multiplier missing: %v/%v", v, ok)
	}
	if v, ok := modelMultiplier("intl", "cn-only"); ok {
		t.Fatalf("cn-only model must not have intl multiplier: %v", v)
	}
	dir, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(dir, modelsCacheFile))
	if err != nil {
		t.Fatal(err)
	}
	var cache modelsCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Schema != modelsCacheSchema {
		t.Fatalf("cache=%+v err=%v", cache, err)
	}
	if len(cache.Catalogs["cn"]) != 2 || len(cache.Catalogs["intl"]) != 1 {
		t.Fatalf("per-site catalogs not cached: %+v", cache.Catalogs)
	}
}

func TestModelSourceLabel(t *testing.T) {
	if got := modelSourceLabel(""); got != "unavailable" {
		t.Fatalf("empty source label=%s", got)
	}
	if got := modelSourceLabel("2026-09-17 13:24"); !strings.Contains(got, "live-api@") {
		t.Fatalf("unexpected label %s", got)
	}
}

func TestMergeCatalogsPrefersLiveAndFillsNPM(t *testing.T) {
	live := []catalogModel{{ID: "hy3", Multiplier: 0.2, HasMultiplier: true, FromLive: true}}
	npm := []catalogModel{
		{ID: "hy3", Multiplier: 0.99, HasMultiplier: true}, // 接口已有 → 保留接口值
		{ID: "legacy-only", Multiplier: 0.5, HasMultiplier: true},
	}
	merged := mergeCatalogs(live, npm)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged models, got %+v", merged)
	}
	if merged[0].ID != "hy3" || merged[0].Multiplier != 0.2 || !merged[0].FromLive {
		t.Fatalf("live entry must win: %+v", merged[0])
	}
	if merged[1].ID != "legacy-only" {
		t.Fatalf("npm-only model should be kept: %+v", merged[1])
	}
}

func TestParseLiveCatalogMarksExpiredPromo(t *testing.T) {
	now := time.Now()
	body := map[string]any{
		"models": []any{map[string]any{"id": "hy4-preview", "credits": "x0.00 credits"}},
		"modelPromotions": []any{
			map[string]any{
				"id": "hy4-free", "enabled": true, "modelIds": []string{"hy4-preview"},
				"badge":    map[string]any{"label": "Free now"},
				"discount": map[string]any{"factor": 0},
				"schedule": map[string]any{"validFrom": now.Add(-720 * time.Hour).Format(time.RFC3339), "validUntil": now.Add(-1 * time.Hour).Format(time.RFC3339)},
			},
		},
	}
	data, _ := json.Marshal(body)
	models, active, err := parseLiveCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("expired promo must not count as active, got %d", active)
	}
	m := models[0]
	if !m.PromoExpired || m.PromoFree {
		t.Fatalf("expired promo should be marked expired, not free: %+v", m)
	}
}

func TestModelDisplayMultiplierWithProbeVerdict(t *testing.T) {
	resetModelsStateT(t)
	resetModelStats()
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn": {{ID: "x", Multiplier: 0.29, HasMultiplier: true, FromLive: true}},
	}
	modelProbes = map[string]modelPriceProbe{}
	modelsMu.Unlock()
	defer func() {
		modelsMu.Lock()
		modelProbes = map[string]modelPriceProbe{}
		modelsMu.Unlock()
	}()

	if got := modelDisplayMultiplier("cn", "x", "-"); got != "0.29x" {
		t.Fatalf("live price expected, got %s", got)
	}
	modelsMu.Lock()
	modelProbes[probeKey("cn", "x")] = modelPriceProbe{Verdict: "paid"}
	modelsMu.Unlock()
	if got := modelDisplayMultiplier("cn", "x", "-"); got != "收费(倍率未知)" {
		t.Fatalf("probed paid should show 收费(倍率未知), got %s", got)
	}
	modelsMu.Lock()
	modelProbes[probeKey("cn", "x")] = modelPriceProbe{Verdict: "free"}
	modelsMu.Unlock()
	if got := modelDisplayMultiplier("cn", "x", "-"); got != "0.00x" {
		t.Fatalf("probed free should show 0.00x, got %s", got)
	}
}

// 促销过期只表示折扣结束，倍率回落到 credits 原价，不应显示为「价格未知」。
func TestExpiredPromoFallsBackToBasePrice(t *testing.T) {
	resetModelsStateT(t)
	resetModelStats()
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"intl": {{ID: "hy4-preview", Credits: "x0.00", BaseMultiplier: 0, Multiplier: 0, HasMultiplier: true, PromoExpired: true, FromLive: true}},
		"cn":   {{ID: "hy4-preview", Credits: "x0.29", BaseMultiplier: 0.29, Multiplier: 0.29, HasMultiplier: true, FromLive: true}},
	}
	modelProbes = map[string]modelPriceProbe{}
	modelsMu.Unlock()

	// 促销过期时接口 credits 不可信（上游会把促销价固化在 credits 里），
	// 探测出结果前按完全未知处理。
	if got := modelDisplayMultiplier("intl", "hy4-preview", "-"); got != "-" {
		t.Fatalf("expired promo must show unknown before probing, got %s", got)
	}
	// 探测确认收费后显示收费(倍率未知)
	modelsMu.Lock()
	modelProbes[probeKey("intl", "hy4-preview")] = modelPriceProbe{Verdict: "paid"}
	modelsMu.Unlock()
	if got := modelDisplayMultiplier("intl", "hy4-preview", "-"); got != "收费(倍率未知)" {
		t.Fatalf("probed paid should show 收费(倍率未知), got %s", got)
	}
	// 探测确认免费后显示 0.00x
	modelsMu.Lock()
	modelProbes[probeKey("intl", "hy4-preview")] = modelPriceProbe{Verdict: "free"}
	modelsMu.Unlock()
	if got := modelDisplayMultiplier("intl", "hy4-preview", "-"); got != "0.00x" {
		t.Fatalf("probed free should show 0.00x, got %s", got)
	}
	if got := modelDisplayMultiplier("cn", "hy4-preview", "-"); got != "0.29x" {
		t.Fatalf("cn should show 0.29x, got %s", got)
	}
}

// 一个站点免费、另一个收费时，优先返回免费站点账号；都收费则不搞优先。
func TestPreferredFreeSites(t *testing.T) {
	resetModelsStateT(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"cn":   {{ID: "m1", Multiplier: 0.03, HasMultiplier: true, FromLive: true}},
		"intl": {{ID: "m1", Multiplier: 0, HasMultiplier: true, PromoFree: true, FromLive: true}},
	}
	modelProbes = map[string]modelPriceProbe{}
	modelsMu.Unlock()

	oldAccounts := accounts
	accountMu.Lock()
	accounts = []*Account{
		{Path: "cn.json", Auth: &StoredAuth{Edition: "cn"}},
		{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}},
	}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	pref := preferredFreeSites("m1")
	if pref == nil || !pref["intl"] || pref["cn"] {
		t.Fatalf("expected intl to be preferred, got %v", pref)
	}

	// 两边都收费 → 不优先
	modelsMu.Lock()
	catalogModels["intl"] = []catalogModel{{ID: "m2", Multiplier: 0.5, HasMultiplier: true, FromLive: true}}
	catalogModels["cn"] = []catalogModel{{ID: "m2", Multiplier: 0.03, HasMultiplier: true, FromLive: true}}
	modelsMu.Unlock()
	if pref := preferredFreeSites("m2"); pref != nil {
		t.Fatalf("both paid must not prefer any site, got %v", pref)
	}
}

func TestModelProbeBatchSizeFirstRunIsLarger(t *testing.T) {
	resetModelsStateT(t)
	modelsMu.Lock()
	modelProbes = map[string]modelPriceProbe{}
	modelsMu.Unlock()
	if got := modelProbeBatchSize(); got != modelPriceProbeInitialBatch {
		t.Fatalf("first run should use initial batch %d, got %d", modelPriceProbeInitialBatch, got)
	}
	modelsMu.Lock()
	modelProbes[probeKey("cn", "x")] = modelPriceProbe{LastProbeAt: time.Now().Unix()}
	modelsMu.Unlock()
	if got := modelProbeBatchSize(); got != modelPriceProbeBatch {
		t.Fatalf("after first probe should use regular batch %d, got %d", modelPriceProbeBatch, got)
	}
}

func TestPickProbeAccountPrefersFundedThenUnknown(t *testing.T) {
	oldAccounts := accounts
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	// 只有额度未知的账号 → 仍可用于探测（14018 会被忽略，不污染判定）
	accountMu.Lock()
	accounts = []*Account{
		{Path: "intl-unknown.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "t"}}},
	}
	accountMu.Unlock()
	if acc := pickProbeAccount("intl"); acc == nil || acc.Path != "intl-unknown.json" {
		t.Fatalf("unknown-quota account should be usable for probing, got %v", acc)
	}

	// 已知耗尽 → 跳过；已知有余额 → 优先
	accountMu.Lock()
	accounts = []*Account{
		{Path: "exhausted.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "t"}}, QuotaKnown: true, QuotaRemaining: 0},
		{Path: "funded.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "t"}}, QuotaKnown: true, QuotaRemaining: 100},
		{Path: "unknown.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "t"}}},
	}
	accountMu.Unlock()
	if acc := pickProbeAccount("intl"); acc == nil || acc.Path != "funded.json" {
		t.Fatalf("funded account should be preferred, got %v", acc)
	}
	accountMu.Lock()
	accounts = []*Account{
		{Path: "exhausted.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "t"}}, QuotaKnown: true, QuotaRemaining: 0},
	}
	accountMu.Unlock()
	if acc := pickProbeAccount("intl"); acc != nil {
		t.Fatalf("known-exhausted account must be skipped, got %v", acc)
	}
}
