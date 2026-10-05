package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestFreeModelDisplayCountsUsesStatisticsMultipliers(t *testing.T) {
	rows := []modelStatSnapshot{
		{ID: "both", CNMultiplier: "0.00x", IntlMultiplier: "0.00x"},
		{ID: "cn-only", CNMultiplier: "0.00x", IntlMultiplier: "0.29x"},
		{ID: "intl-only", CNMultiplier: "收费(倍率未知)", IntlMultiplier: "0.00x"},
		{ID: "unknown", CNMultiplier: "-", IntlMultiplier: "-"},
		// 复用统计表的最终倍率，而不是再从免费/收费标签做另一套推导。
		{ID: "mixed", CNFree: "混合", CNMultiplier: "0.29x", IntlFree: "是", IntlMultiplier: "-"},
	}
	cn, intl := freeModelDisplayCounts(rows)
	if cn != 2 || intl != 2 {
		t.Fatalf("counts cn=%d intl=%d, want 2/2", cn, intl)
	}
	if cn, intl := freeModelDisplayCounts(nil); cn != 0 || intl != 0 {
		t.Fatalf("empty rows cn=%d intl=%d", cn, intl)
	}
}

func TestFreeModelDisplaySnapshotKeepsMeasuredLedgerAndScheduling(t *testing.T) {
	chdirTemp(t)
	accountMu.Lock()
	oldAccounts, oldDisplay := accounts, lastFreeModelDisplay
	accountMu.Unlock()
	modelsMu.Lock()
	oldCatalog, oldSource, oldProbes := catalogModels, dynamicSource, modelProbes
	catalogModels = map[string][]catalogModel{
		"cn":   {{ID: "catalog-free", HasMultiplier: true, Multiplier: 0}},
		"intl": {{ID: "catalog-free", HasMultiplier: true, Multiplier: 0.5}},
	}
	dynamicSource = "display-test"
	modelProbes = map[string]modelPriceProbe{}
	modelsMu.Unlock()
	modelStatsMu.Lock()
	oldStats := modelStats
	modelStats = map[string]*modelStat{}
	modelStatsMu.Unlock()
	modelFilterMu.Lock()
	oldFilter := currentFilter
	currentFilter = modelFilter{}
	modelFilterMu.Unlock()
	modelAccountFilterMu.Lock()
	oldRules := modelAccountFilters
	modelAccountFilters = map[string]modelAccountFileRule{}
	modelAccountFilterMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts, lastFreeModelDisplay = oldAccounts, oldDisplay
		accountMu.Unlock()
		modelsMu.Lock()
		catalogModels, dynamicSource, modelProbes = oldCatalog, oldSource, oldProbes
		modelsMu.Unlock()
		modelStatsMu.Lock()
		modelStats = oldStats
		modelStatsMu.Unlock()
		modelFilterMu.Lock()
		currentFilter = oldFilter
		modelFilterMu.Unlock()
		modelAccountFilterMu.Lock()
		modelAccountFilters = oldRules
		modelAccountFilterMu.Unlock()
	})
	empty := &Account{Path: "no-observations.json", Auth: &StoredAuth{Edition: "cn"}}
	measured := &Account{Path: "paid-observation.json", Auth: &StoredAuth{Edition: "cn"}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{
			"catalog-free": {CostClass: modelCostPaid, QuotaBlocked: true, LastReason: "measured paid", ObservedAt: time.Now()},
		},
	}
	intl := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}}
	before := *measured.ModelStates["catalog-free"]
	beforeKind, beforeUsable := usableForModelLocked(measured, "catalog-free", time.Now())
	accountMu.Lock()
	accounts = []*Account{empty, measured, intl}
	accountMu.Unlock()
	writeStatusSnapshot()
	raw, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 3 || snap.Accounts[0].FreeModels != 1 || snap.Accounts[1].FreeModels != 1 || snap.Accounts[2].FreeModels != 0 {
		t.Fatalf("site display counts not applied: %+v", snap.Accounts)
	}
	if empty.ModelStates != nil || len(snap.Accounts[0].ModelStates) != 0 {
		t.Fatal("display populated an unobserved account's business ledger")
	}
	if !reflect.DeepEqual(before, *measured.ModelStates["catalog-free"]) ||
		snap.Accounts[1].ModelStates["catalog-free"].CostClass != modelCostPaid ||
		!snap.Accounts[1].ModelStates["catalog-free"].QuotaBlocked {
		t.Fatal("display changed or failed to persist the measured ledger")
	}
	afterKind, afterUsable := usableForModelLocked(measured, "catalog-free", time.Now())
	if beforeKind != afterKind || beforeUsable != afterUsable || afterUsable {
		t.Fatal("display count changed scheduling eligibility")
	}
	// 全局禁用模型后，它从模型统计表消失，展示数量也应同步变化。
	setModelFilter([]string{"catalog-free"}, nil)
	writeStatusSnapshot()
	raw, err = os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	snap = statusSnapshot{}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Accounts {
		if a.FreeModels != 0 {
			t.Fatalf("hidden model counted for %s", a.Path)
		}
	}
}
