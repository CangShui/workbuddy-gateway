package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withCatalog 在测试期间替换目录快照，结束后恢复。
func withCatalog(t *testing.T, catalogs map[string][]catalogModel) {
	t.Helper()
	modelsMu.Lock()
	oldModels, oldSource := catalogModels, dynamicSource
	catalogModels = catalogs
	dynamicSource = "test"
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		catalogModels, dynamicSource = oldModels, oldSource
		modelsMu.Unlock()
	})
}

func capabilityByID(t *testing.T, body []byte, id string) map[string]any {
	t.Helper()
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	for _, row := range resp.Data {
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("model %s missing from /v1/models", id)
	return nil
}

func callModels(t *testing.T) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	return rec.Body.Bytes()
}

// npm 目录此前只读 id/name/credits，能力字段被整段丢弃；这里锁定已补齐。
func TestParseNPMCatalogRetainsCapabilityFields(t *testing.T) {
	models, err := parseNPMCatalog([]byte(`{"models":[
		{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","credits":"x0.03 credits",
		 "maxInputTokens":1000000,"maxOutputTokens":128000,"maxAllowedSize":1000000,
		 "contextWindow":{"supportedLengths":[1000000,300000],"defaultLength":300000}},
		{"id":"hy3","name":"Hy3","credits":"x0.00 credits","maxInputTokens":192000,"maxOutputTokens":64000}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalogModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	ds := byID["deepseek-v4.1-flash"]
	if ds.MaxInputTokens != 1000000 || ds.MaxOutputTokens != 128000 || ds.MaxAllowedSize != 1000000 {
		t.Fatalf("npm 能力字段丢失: %+v", ds)
	}
	if ds.ContextDefaultLength != 300000 {
		t.Fatalf("npm 默认预算丢失: %+v", ds)
	}
	// 档位必须排序，且默认预算与能力上限是两个字段，不能合并。
	want := []int{300000, 1000000}
	if len(ds.ContextLengths) != 2 || ds.ContextLengths[0] != want[0] || ds.ContextLengths[1] != want[1] {
		t.Fatalf("npm 档位未规范化: %v", ds.ContextLengths)
	}
	if hy := byID["hy3"]; hy.MaxInputTokens != 192000 || hy.ContextDefaultLength != 0 || len(hy.ContextLengths) != 0 {
		t.Fatalf("无 contextWindow 的模型不得编造档位: %+v", hy)
	}
}

// 生产实测：实时接口返回 contextWindow（三档）与 maxAllowedSize，必须保留。
func TestParseLiveCatalogRetainsContextWindow(t *testing.T) {
	models, _, err := parseLiveCatalog([]byte(`{"models":[
		{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","credits":"x0.11 credits",
		 "maxInputTokens":1000000,"maxOutputTokens":128000,"maxAllowedSize":1000000,
		 "contextWindow":{"defaultLength":300000,"supportedLengths":[300000,600000,1000000]}}
	],"modelPromotions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("models=%d", len(models))
	}
	m := models[0]
	if m.MaxInputTokens != 1000000 || m.MaxOutputTokens != 128000 || m.MaxAllowedSize != 1000000 {
		t.Fatalf("实时能力字段丢失: %+v", m)
	}
	if m.ContextDefaultLength != 300000 {
		t.Fatalf("默认预算错误: %d", m.ContextDefaultLength)
	}
	if len(m.ContextLengths) != 3 || m.ContextLengths[2] != 1000000 {
		t.Fatalf("三档未保留: %v", m.ContextLengths)
	}
}

// 合并时 npm 只补齐实时目录缺失的能力字段，绝不覆盖实时值。
func TestMergeCatalogsBackfillsCapabilityWithoutOverwriting(t *testing.T) {
	live := []catalogModel{{ID: "a", FromLive: true, MaxInputTokens: 1000000}}
	npm := []catalogModel{{
		ID: "a", MaxInputTokens: 999, MaxOutputTokens: 128000, MaxAllowedSize: 1000000,
		ContextLengths: []int{300000, 1000000}, ContextDefaultLength: 300000,
	}}
	merged := mergeCatalogs(live, npm)
	if len(merged) != 1 {
		t.Fatalf("merged=%d", len(merged))
	}
	got := merged[0]
	if got.MaxInputTokens != 1000000 {
		t.Fatalf("实时值被覆盖: %d", got.MaxInputTokens)
	}
	if got.MaxOutputTokens != 128000 || got.ContextDefaultLength != 300000 || len(got.ContextLengths) != 2 {
		t.Fatalf("npm 未补齐缺失字段: %+v", got)
	}
}

func TestNormalizeContextLengthsFiltersUnusable(t *testing.T) {
	got := normalizeContextLengths([]int{1000000, 300000, 300000, -1, 0, 5000000}, 1000000)
	if len(got) != 2 || got[0] != 300000 || got[1] != 1000000 {
		t.Fatalf("got %v", got)
	}
	if single := normalizeContextLengths([]int{300000}, 1000000); len(single) != 1 {
		t.Fatalf("单档应保留给调用方判断: %v", single)
	}
}

// 只有单一站点有数据时给出顶层标量，不附分站点明细。
func TestModelCapabilitySingleSiteGivesScalars(t *testing.T) {
	withCatalog(t, map[string][]catalogModel{"cn": {{
		ID: "m", MaxInputTokens: 1000000, MaxOutputTokens: 128000,
		ContextLengths: []int{300000, 1000000}, ContextDefaultLength: 300000,
	}}})
	mc, ok := modelCapabilityFor("m")
	if !ok || mc.Ceiling != 1000000 || mc.MaxOutput != 128000 || mc.Default != 300000 {
		t.Fatalf("单站点能力汇总错误: %+v ok=%t", mc, ok)
	}
	if len(mc.BySite) != 0 {
		t.Fatalf("单站点不应有分站点明细: %+v", mc.BySite)
	}
}

// 站点能力不一致时不得取最大值：顶层留空，改为给出分站点明细。
func TestModelCapabilityCrossSiteDisagreementOmitsScalars(t *testing.T) {
	withCatalog(t, map[string][]catalogModel{
		"cn":   {{ID: "m", MaxInputTokens: 192000, MaxOutputTokens: 64000}},
		"intl": {{ID: "m", MaxInputTokens: 1000000, MaxOutputTokens: 128000}},
	})
	mc, ok := modelCapabilityFor("m")
	if !ok {
		t.Fatal("应识别到能力数据")
	}
	if mc.Ceiling != 0 || mc.MaxOutput != 0 {
		t.Fatalf("不一致时顶层不得给值（尤其不能取 1000000）: %+v", mc)
	}
	if len(mc.BySite) != 2 || mc.BySite["cn"].Ceiling != 192000 || mc.BySite["intl"].Ceiling != 1000000 {
		t.Fatalf("缺少分站点明细: %+v", mc.BySite)
	}
}

// 站点一致时给出顶层标量，且默认预算与能力上限分开返回。
func TestHandleModelsEmitsCapabilityFields(t *testing.T) {
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	withCatalog(t, map[string][]catalogModel{"cn": {{
		ID: "deepseek-v4.1-flash", MaxInputTokens: 1000000, MaxOutputTokens: 128000,
		ContextLengths: []int{300000, 600000, 1000000}, ContextDefaultLength: 300000,
	}}})
	row := capabilityByID(t, callModels(t), "deepseek-v4.1-flash")
	if row["context_length"] != float64(1000000) {
		t.Fatalf("context_length 应为能力上限: %v", row["context_length"])
	}
	if row["default_context_length"] != float64(300000) {
		t.Fatalf("默认预算必须与能力上限分开: %v", row["default_context_length"])
	}
	if row["max_output_tokens"] != float64(128000) {
		t.Fatalf("max_output_tokens=%v", row["max_output_tokens"])
	}
	opts, ok := row["context_length_options"].([]any)
	if !ok || len(opts) != 3 {
		t.Fatalf("档位列表=%v", row["context_length_options"])
	}
	if _, exists := row["capabilities_by_site"]; exists {
		t.Fatal("站点一致时不需要分站点明细")
	}
}

// 未知能力必须省略字段，不得兜底成 1M；也不得往模型列表里塞上游请求参数。
func TestHandleModelsOmitsUnknownCapability(t *testing.T) {
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	withCatalog(t, map[string][]catalogModel{"cn": {{ID: "mystery"}}})
	row := capabilityByID(t, callModels(t), "mystery")
	for _, key := range []string{
		"context_length", "max_output_tokens", "default_context_length",
		"context_length_options", "capabilities_by_site", "context_window",
	} {
		if _, exists := row[key]; exists {
			t.Fatalf("未知能力不得输出 %s：%v", key, row[key])
		}
	}
}
