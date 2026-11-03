package main

import (
	"encoding/json"
	"testing"
)

func TestInternationalSGCatalogAlias(t *testing.T) {
	resetModelsStateT(t)
	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"intl": {{ID: "deepseek-v4.1-flash-sg", Multiplier: 0.11, HasMultiplier: true, FromLive: true}},
	}
	modelsMu.Unlock()
	defer resetModelsStateT(t)

	entry, ok := modelEntry("intl", "deepseek-v4.1-flash")
	if !ok || entry.ID != "deepseek-v4.1-flash-sg" || entry.Multiplier != 0.11 {
		t.Fatalf("sg alias not resolved: %+v, ok=%t", entry, ok)
	}
	if got := modelDisplayMultiplier("intl", "deepseek-v4.1-flash", "否"); got != "0.11x" {
		t.Fatalf("paid sg alias should show 0.11x, got %s", got)
	}
	ids, _ := collectModelIDs(false)
	if len(ids) != 1 || ids[0] != "deepseek-v4.1-flash" {
		t.Fatalf("sg alias should display as base model, got %v", ids)
	}
}

func TestDisplayOnlyPromotionWithoutDiscountKeepsBaseRate(t *testing.T) {
	body := map[string]any{
		"models": []any{
			map[string]any{"id": "deepseek-v4.1-flash", "name": "DeepSeek-V4.1-Flash", "credits": "x0.11"},
		},
		"modelPromotions": []any{
			map[string]any{
				"id":       "ds-discount-daytime-badge",
				"enabled":  true,
				"modelIds": []string{"deepseek-v4.1-flash"},
				"badge":    map[string]any{"label": "夜间折扣"},
				"schedule": map[string]any{
					"timezone": "Asia/Shanghai",
					"daily":    []any{map[string]any{"start": "0:00", "end": "23:59"}},
				},
			},
		},
	}
	data, _ := json.Marshal(body)
	models, _, err := parseLiveCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Multiplier != 0.11 || models[0].PromoFree || !models[0].HasMultiplier {
		t.Fatalf("display-only promotion must not zero the base rate: %+v", models)
	}
}
