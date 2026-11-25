package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// 说明：本文件覆盖多模型 Key 的三条管理接口与旧单 Key 字段的迁移行为。
// 断言重点是「列表只给掩码、只有显式查看返回原文、删除后立即失效」。

func decodeAPIKeyJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v body=%s", err, string(raw))
	}
	return out
}

// apiKeysFromView 取出设置视图里的 apiKeys 数组。
func apiKeysFromView(view map[string]any) []any {
	keys, _ := view["apiKeys"].([]any)
	return keys
}

// settingsViewAPIKeys 直接请求 GET /admin/api/settings 并返回 apiKeys 数组。
func settingsViewAPIKeys(t *testing.T, handler http.Handler, admin string) []any {
	t.Helper()
	w := adminRequest(handler, http.MethodGet, "/admin/api/settings", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("读取设置状态码=%d body=%s", w.Code, w.Body.String())
	}
	return apiKeysFromView(decodeAPIKeyJSON(t, w.Body.Bytes()))
}

func TestGatewayAPIKeyCreateRevealAndDelete(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)
	const clientKey = "synthetic-client-key-0001"

	w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", admin,
		`{"label":"笔记本","apiKey":"`+clientKey+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("新增 Key 状态码=%d body=%s", w.Code, w.Body.String())
	}
	created := decodeAPIKeyJSON(t, w.Body.Bytes())
	if created["apiKey"] != clientKey || created["label"] != "笔记本" {
		t.Fatal("新增响应必须返回一次明文与标签")
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("新增 Key 必须返回服务端生成的 id")
	}
	if masked, _ := created["apiKeyMasked"].(string); !strings.Contains(masked, "****") || masked == clientKey {
		t.Fatalf("列表只能给掩码，实际 %q", masked)
	}

	// 设置视图绝不返回明文。
	if w := adminRequest(handler, http.MethodGet, "/admin/api/settings", admin, ""); strings.Contains(w.Body.String(), clientKey) {
		t.Fatal("设置视图泄漏了 Key 明文")
	}
	if keys := settingsViewAPIKeys(t, handler, admin); len(keys) != 1 {
		t.Fatalf("Key 条数=%d，期望 1", len(keys))
	}

	// 只有显式查看才返回原文，并且禁止缓存。
	w = adminRequest(handler, http.MethodPost, "/admin/api/api-key-reveal", admin, `{"id":"`+id+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("查看明文状态码=%d body=%s", w.Code, w.Body.String())
	}
	if got := decodeAPIKeyJSON(t, w.Body.Bytes())["apiKey"]; got != clientKey {
		t.Fatalf("查看接口必须返回原文，实际 %v", got)
	}
	if cache := w.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Fatalf("明文响应必须禁止缓存，实际 Cache-Control=%q", cache)
	}

	if w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-reveal", admin, `{"id":"does-not-exist"}`); w.Code != http.StatusNotFound {
		t.Fatalf("未知 id 应 404，实际 %d", w.Code)
	}

	if w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-delete", admin, `{"id":"`+id+`"}`); w.Code != http.StatusOK {
		t.Fatalf("删除状态码=%d body=%s", w.Code, w.Body.String())
	}
	if keys := settingsViewAPIKeys(t, handler, admin); len(keys) != 0 {
		t.Fatalf("删除后列表应为空，实际 %d 条", len(keys))
	}
	if w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-reveal", admin, `{"id":"`+id+`"}`); w.Code != http.StatusNotFound {
		t.Fatal("删除后不应还能查看该 Key")
	}
}

func TestGatewayAPIKeyGeneratedKeyEnablesAuth(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)

	w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", admin, `{"label":""}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("自动生成 Key 状态码=%d body=%s", w.Code, w.Body.String())
	}
	created := decodeAPIKeyJSON(t, w.Body.Bytes())
	if created["generated"] != true {
		t.Fatal("留空 Key 值必须由服务端生成")
	}
	generated, _ := created["apiKey"].(string)
	if len(generated) != 32 {
		t.Fatalf("生成的 Key 长度=%d，期望 32 位十六进制", len(generated))
	}

	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("开启校验状态码=%d body=%s", w.Code, w.Body.String())
	}
	api := newAPIHandler()
	if code := adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", "", `{"messages":[]}`).Code; code != http.StatusUnauthorized {
		t.Fatalf("未带 Key 应 401，实际 %d", code)
	}
	if code := adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", generated, `{"messages":[]}`).Code; code != http.StatusOK {
		t.Fatalf("自动生成的 Key 应通过鉴权，实际 %d", code)
	}
	// 管理接口仍然只认管理 Key。
	if code := adminRequest(handler, http.MethodGet, "/admin/api/settings", generated, "").Code; code != http.StatusUnauthorized {
		t.Fatalf("模型 Key 不能登录管理接口，实际 %d", code)
	}
}

func TestGatewayAPIKeyMultipleKeysAllAccepted(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)
	clientKeys := []string{"multi-key-alpha-01", "multi-key-beta-02"}
	for i, key := range clientKeys {
		body := `{"label":"client-` + key[len(key)-2:] + `","apiKey":"` + key + `"}`
		if w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", admin, body); w.Code != http.StatusCreated {
			t.Fatalf("第 %d 条 Key 新增失败: 状态码=%d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("开启校验失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	api := newAPIHandler()
	for _, key := range clientKeys {
		if code := adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", key, `{"messages":[]}`).Code; code != http.StatusOK {
			t.Fatalf("Key %q 应通过鉴权，实际 %d", key, code)
		}
	}
	if code := adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", "unknown-client-key-03", `{"messages":[]}`).Code; code != http.StatusUnauthorized {
		t.Fatalf("未知 Key 应 401，实际 %d", code)
	}
	// 关闭校验不删除数组，重新开启后仍全部可用。
	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("关闭校验失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	if keys := settingsViewAPIKeys(t, handler, admin); len(keys) != len(clientKeys) {
		t.Fatalf("关闭校验不应删除 Key，实际 %d 条", len(keys))
	}
	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("重新开启校验失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	if code := adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", clientKeys[1], `{"messages":[]}`).Code; code != http.StatusOK {
		t.Fatalf("重新开启后 Key 仍应有效，实际 %d", code)
	}
}

func TestGatewayAPIKeyDeleteLastDisablesAuth(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)

	w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", admin, `{"label":"唯一","apiKey":"only-client-key-01"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("新增 Key 状态码=%d body=%s", w.Code, w.Body.String())
	}
	id, _ := decodeAPIKeyJSON(t, w.Body.Bytes())["id"].(string)
	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("开启校验失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	w = adminRequest(handler, http.MethodPost, "/admin/api/api-key-delete", admin, `{"id":"`+id+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("删除最后一条 Key 状态码=%d body=%s", w.Code, w.Body.String())
	}
	result := decodeAPIKeyJSON(t, w.Body.Bytes())
	if result["authDisabledByRemoval"] != true || result["apiKeyEnabled"] != false {
		t.Fatalf("删除最后一条 Key 必须同时关闭校验，实际 %v", result)
	}
	if code := adminRequest(newAPIHandler(), http.MethodPost, "/v1/messages/count_tokens", "", `{"messages":[]}`).Code; code != http.StatusOK {
		t.Fatalf("关闭校验后不带 Key 应可用，实际 %d", code)
	}
}

func TestGatewayAPIKeyDeletionRevokesImmediately(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)
	clientKeys := []string{"revoke-kept-key-0001", "revoke-drop-key-0001"}
	ids := make([]string, 0, len(clientKeys))
	for _, key := range clientKeys {
		w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", admin, `{"label":"轮换","apiKey":"`+key+`"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("新增 Key 状态码=%d body=%s", w.Code, w.Body.String())
		}
		id, _ := decodeAPIKeyJSON(t, w.Body.Bytes())["id"].(string)
		ids = append(ids, id)
	}
	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("开启校验失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	api := newAPIHandler()
	countTokens := func(key string) int {
		return adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", key, `{"messages":[]}`).Code
	}
	for _, key := range clientKeys {
		if code := countTokens(key); code != http.StatusOK {
			t.Fatalf("Key %q 应通过鉴权，实际 %d", key, code)
		}
	}
	if w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-delete", admin, `{"id":"`+ids[1]+`"}`); w.Code != http.StatusOK {
		t.Fatalf("删除状态码=%d body=%s", w.Code, w.Body.String())
	}
	// 吊销必须立即生效：被删的 Key 立刻 401，保留的 Key 不受影响。
	if code := countTokens(clientKeys[1]); code != http.StatusUnauthorized {
		t.Fatalf("被删除的 Key 必须立即失效，实际 %d", code)
	}
	if code := countTokens(clientKeys[0]); code != http.StatusOK {
		t.Fatalf("未被删除的 Key 必须仍然可用，实际 %d", code)
	}
}

func TestGatewayAPIKeyShortValueIsFullyMasked(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)

	w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", admin, `{"label":"短值","apiKey":"short"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("新增短 Key 状态码=%d body=%s", w.Code, w.Body.String())
	}
	if masked, _ := decodeAPIKeyJSON(t, w.Body.Bytes())["apiKeyMasked"].(string); masked != "****" {
		t.Fatalf("过短的 Key 必须整体掩码，实际 %q", masked)
	}
	keys := settingsViewAPIKeys(t, handler, admin)
	if len(keys) != 1 {
		t.Fatalf("Key 条数=%d，期望 1", len(keys))
	}
	first, _ := keys[0].(map[string]any)
	if first["apiKeyMasked"] != "****" {
		t.Fatalf("设置视图对过短的 Key 也必须整体掩码，实际 %v", first["apiKeyMasked"])
	}
}

func TestGatewayAPIKeyInputValidation(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)

	if w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", admin, `{"apiKey":"dup-client-key-01"}`); w.Code != http.StatusCreated {
		t.Fatalf("首次新增失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"重复 Key 值", http.MethodPost, "/admin/api/api-key-create", `{"apiKey":"dup-client-key-01"}`, http.StatusBadRequest},
		{"Key 值含空格", http.MethodPost, "/admin/api/api-key-create", `{"apiKey":"has space"}`, http.StatusBadRequest},
		{"标签含控制字符", http.MethodPost, "/admin/api/api-key-create", `{"label":"bad\nlabel","apiKey":"ctrl-label-key-01"}`, http.StatusBadRequest},
		{"标签超长", http.MethodPost, "/admin/api/api-key-create", `{"label":"` + strings.Repeat("字", 65) + `","apiKey":"long-label-key-01"}`, http.StatusBadRequest},
		{"未知字段", http.MethodPost, "/admin/api/api-key-create", `{"apiKey":"other-key-01","unknown":true}`, http.StatusBadRequest},
		{"GET 新增", http.MethodGet, "/admin/api/api-key-create", "", http.StatusMethodNotAllowed},
		{"查看缺少 id", http.MethodPost, "/admin/api/api-key-reveal", `{"id":""}`, http.StatusBadRequest},
		{"删除缺少 id", http.MethodPost, "/admin/api/api-key-delete", `{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if w := adminRequest(handler, tc.method, tc.path, admin, tc.body); w.Code != tc.want {
			t.Fatalf("%s: 状态码=%d，期望 %d，body=%s", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
	// 模型 Key 不是管理 Key，不能调用任何管理接口。
	if w := adminRequest(handler, http.MethodPost, "/admin/api/api-key-create", "dup-client-key-01", `{"apiKey":"another-key-01"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("模型 Key 不能调用管理接口，实际 %d", w.Code)
	}
}

func TestGatewayLegacySingleKeyMigratesToArray(t *testing.T) {
	isolatedGatewayConfig(t)
	content := `{"gateway":{"apiPort":8317,"webPort":8316,"adminKey":"legacy-admin","apiKeyEnabled":true,"apiKey":"legacy-single-key-1"}}`
	if err := os.WriteFile(runtimeConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if currentAPIKey() != "legacy-single-key-1" {
		t.Fatal("旧字段 apiKey 必须仍然生效")
	}
	viewRaw, err := json.Marshal(gatewaySettingsView())
	if err != nil {
		t.Fatal(err)
	}
	keys := apiKeysFromView(decodeAPIKeyJSON(t, viewRaw))
	if len(keys) != 1 {
		t.Fatalf("内存合成条数=%d，期望 1", len(keys))
	}
	first, _ := keys[0].(map[string]any)
	if first["id"] != legacyAPIKeyID || first["label"] != legacyAPIKeyLabel {
		t.Fatalf("合成记录必须使用稳定的 legacy id 与默认标签，实际 %v", first)
	}
	if masked, _ := first["apiKeyMasked"].(string); masked == "legacy-single-key-1" {
		t.Fatal("旧字段合成后也必须只输出掩码")
	}

	// 热加载是只读的：读时合成不改写磁盘。
	onDisk, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != content {
		t.Fatal("热加载不得改写配置文件")
	}

	// 真正的保存才落成数组，并清空旧字段。
	if err := saveAPIKeySettings(true, "legacy-single-key-1", "test"); err != nil {
		t.Fatal(err)
	}
	onDisk, err = os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Gateway gatewayFileConfig `json:"gateway"`
	}
	if err := json.Unmarshal(onDisk, &doc); err != nil {
		t.Fatalf("保存后的配置不是合法 JSON: %v", err)
	}
	if doc.Gateway.APIKey != "" {
		t.Fatalf("保存后磁盘上的 apiKey 必须为空，实际 %q", doc.Gateway.APIKey)
	}
	if len(doc.Gateway.APIKeys) != 1 || doc.Gateway.APIKeys[0].APIKey != "legacy-single-key-1" {
		t.Fatalf("保存后磁盘上应只有一条 apiKeys 记录，实际 %v", doc.Gateway.APIKeys)
	}
	if doc.Gateway.APIKeys[0].ID != legacyAPIKeyID {
		t.Fatalf("迁移后的记录应保留 legacy id，实际 %q", doc.Gateway.APIKeys[0].ID)
	}
}

func TestGatewayLegacySingleKeyWriteReplacesList(t *testing.T) {
	isolatedGatewayConfig(t)
	handler := newWebUIHandler()
	admin := initTestAdmin(t, handler)
	api := newAPIHandler()
	countTokens := func(key string) int {
		return adminRequest(api, http.MethodPost, "/v1/messages/count_tokens", key, `{"messages":[]}`).Code
	}
	// 缓存里的旧版设置页只会提交 apiKey 字段：它表示「当前唯一的模型 Key」。
	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true,"apiKey":"rotate-old-key-0001"}`); w.Code != http.StatusOK {
		t.Fatalf("写入旧字段失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	if code := countTokens("rotate-old-key-0001"); code != http.StatusOK {
		t.Fatalf("首次写入的 Key 应可用，实际 %d", code)
	}
	if w := adminRequest(handler, http.MethodPost, "/admin/api/settings", admin, `{"apiKeyEnabled":true,"apiKey":"rotate-new-key-0002"}`); w.Code != http.StatusOK {
		t.Fatalf("轮换 Key 失败: 状态码=%d body=%s", w.Code, w.Body.String())
	}
	// 轮换必须整体替换：否则旧 Key 会静默继续可用。
	if code := countTokens("rotate-old-key-0001"); code != http.StatusUnauthorized {
		t.Fatalf("轮换后旧 Key 必须立即失效，实际 %d", code)
	}
	if code := countTokens("rotate-new-key-0002"); code != http.StatusOK {
		t.Fatalf("轮换后的新 Key 应可用，实际 %d", code)
	}
	if keys := settingsViewAPIKeys(t, handler, admin); len(keys) != 1 {
		t.Fatalf("轮换后应只剩一条 Key，实际 %d 条", len(keys))
	}
	// 同值重复提交保持幂等，不重建记录。
	if err := saveAPIKeySettings(true, "rotate-new-key-0002", "test"); err != nil {
		t.Fatal(err)
	}
	if keys := settingsViewAPIKeys(t, handler, admin); len(keys) != 1 {
		t.Fatalf("同值重复提交应保持一条，实际 %d 条", len(keys))
	}
}
