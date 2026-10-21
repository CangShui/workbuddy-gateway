package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolatePolicies(t *testing.T) {
	t.Helper()
	isolatedGatewayConfig(t)
	resetModelsStateT(t)
	resetModelStats()
	oldHeader, oldIdle, oldRetries := upstreamSettings()
	upstreamConfigMu.RLock()
	oldKeepalive, oldFirstByteRetry := streamKeepalive, enableFirstByteRetry
	upstreamConfigMu.RUnlock()
	oldAccounts := accounts
	oldRetention := logRetentionDays.Load()
	runtimeConfigMu.Lock()
	oldHash, oldHasHash := runtimeAppliedHash, runtimeHasAppliedHash
	runtimeHasAppliedHash = false
	runtimeConfigMu.Unlock()
	t.Cleanup(func() {
		logRetentionDays.Store(oldRetention)
		runtimeConfigMu.Lock()
		runtimeAppliedHash, runtimeHasAppliedHash = oldHash, oldHasHash
		runtimeConfigMu.Unlock()
		_ = switchDebugLogging(false)
		setModelFilter(nil, nil)
		_ = setModelAccountFilter(nil)
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
		upstreamConfigMu.Lock()
		upstreamHeaderTimeout, upstreamIdleTimeout, upstreamTransientRetries = oldHeader, oldIdle, oldRetries
		streamKeepalive, enableFirstByteRetry = oldKeepalive, oldFirstByteRetry
		upstreamConfigMu.Unlock()
	})
}

func writeRuntimeFixture(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(runtimeConfigFile, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeReloadLastGoodAndDefaults(t *testing.T) {
	isolatePolicies(t)
	watcher := runtimeConfigWatcher{}
	good := `{"gateway":{"adminKey":"admin","apiKeyEnabled":true,"apiKey":"key"},"systemPrompt":{"force":"synthetic"},"upstream":{"headerTimeoutSeconds":41,"idleTimeoutSeconds":17,"transientRetries":0},"models":{"blocklist":["off"],"accounts":{"all":{"blocklist":["b.json"]}}}}`
	writeRuntimeFixture(t, good)
	if !watcher.reload() {
		t.Fatal("有效配置必须加载")
	}
	if watcher.reload() {
		t.Fatal("相同内容不重复应用")
	}
	header, idle, retries := upstreamSettings()
	if header != 41*time.Second || idle != 17*time.Second || retries != 0 {
		t.Fatal("上游配置未生效")
	}
	for _, bad := range []string{
		``, `null`, `[]`, `{`, `{} {}`, `{"unknown":true}`,
		`{"gateway":{"adminKey":"changed","apiKeyEnabled":true},"models":{}}`,
		`{"gateway":{"adminKey":"changed"},"models":{"accounts":{"all":{"blocklist":["../b.json"]}}}}`,
		`{"models":{"allowlist":["one"],"blocklist":["two"]}}`,
		`{"upstream":{"headerTimeoutSeconds":9223372036854775807}}`,
	} {
		writeRuntimeFixture(t, bad)
		if watcher.reload() {
			t.Fatalf("无效配置被接受: %q", bad)
		}
		if currentAdminKey() != "admin" || currentAPIKey() != "key" || configuredSystemPrompts().force != "synthetic" {
			t.Fatal("无效配置部分覆盖了有效配置")
		}
		if disabled, _ := modelDisabled("off"); !disabled {
			t.Fatal("无效配置清除了名单")
		}
	}
	if err := os.Remove(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if watcher.reload() || currentAdminKey() != "admin" {
		t.Fatal("短暂删除不能清空生效配置")
	}
	writeRuntimeFixture(t, `{}`)
	if !watcher.reload() {
		t.Fatal("修复后必须重新应用")
	}
	if currentAPIKey() != "" || currentAdminKey() != "" || configuredSystemPrompts().force != "" {
		t.Fatal("省略配置不能残留旧值")
	}
	header, idle, retries = upstreamSettings()
	if header != upstreamHeaderTimeoutDefault || idle != upstreamIdleTimeoutDefault || retries != upstreamTransientRetriesDefault {
		t.Fatal("省略超时不能残留旧值")
	}
	if ok, _ := modelAccountAllowed("new", &Account{Path: "b.json"}, nil); !ok {
		t.Fatal("省略账号规则未恢复")
	}
}

func TestRuntimeReloadDetectsRevertAfterWebWrite(t *testing.T) {
	isolatePolicies(t)
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}}}
	original := `{"gateway":{"adminKey":"admin"},"models":{"blocklist":[]}}`
	writeRuntimeFixture(t, original)
	watcher := runtimeConfigWatcher{}
	if !watcher.reload() {
		t.Fatal("初始加载失败")
	}
	if err := savePolicyToggle("model", "one", false, "revert"); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixture(t, original) // 在扫描前立即撤销网页修改。
	if !watcher.reload() {
		t.Fatal("撤销到旧文件内容被错误地认为未修改")
	}
	if disabled, _ := modelDisabled("one"); disabled {
		t.Fatal("旧内容恢复后仍残留网页禁用规则")
	}
	if err := saveAPIKeySettings(true, "api", "revert"); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixture(t, original)
	if !watcher.reload() || currentAPIKey() != "" {
		t.Fatal("鉴权设置撤销未被扫描识别")
	}
}

func TestOlderConfigEmptyAccountRulesRemainCompatible(t *testing.T) {
	isolatePolicies(t)
	for _, text := range []string{
		`{}`, `{"debug":{"enabled":false}}`,
		`{"models":{"blocklist":[],"allowlist":[],"accounts":{}}}`,
		`{"models":{"accounts":null}}`,
		`{"models":{"accounts":{"all":{}}}}`,
		`{"models":{"accounts":{"all":{"allowlist":null,"blocklist":null}}}}`,
	} {
		writeRuntimeFixture(t, text)
		if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
			t.Fatalf("旧版/空配置不可读取: %s: %v", text, err)
		}
		if ok, _ := modelAccountAllowed("any", &Account{Path: "a.json"}, nil); !ok {
			t.Fatal("旧配置导致默认账号被停用")
		}
		if disabled, _ := modelDisabled("any"); disabled {
			t.Fatal("旧配置导致默认模型被停用")
		}
	}
}

func TestGlobalWhitelistToggleAndProbeSkips(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"models":{"accounts":{" ALL ":{"allowlist":["a.json"]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	accounts = []*Account{{Path: "a.json", Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic"}}}, {Path: "b.json", Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "synthetic"}}}}
	if err := savePolicyToggle("account", "a.json", false, "global-white"); err != nil {
		t.Fatal(err)
	}
	for _, acc := range accounts {
		if ok, _ := modelAccountAllowed("m", acc, accounts); ok {
			t.Fatal("关闭最后一个白名单账号不能放开其他账号")
		}
	}
	if acc := pickProbeAccountForModel("cn", "m"); acc != nil {
		t.Fatal("后台探测不应选择全局关闭的账号")
	}
	result := probeAccountModel(context.Background(), accounts[0], "m")
	if result.Status != "skipped" {
		t.Fatalf("显式探测不应发上游请求: %+v", result)
	}
	if err := savePolicyToggle("account", "b.json", true, "global-white"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := modelAccountAllowed("new-model", accounts[1], accounts); !ok {
		t.Fatal("无法将白名单外账号重新启用")
	}
	data, _ := os.ReadFile(runtimeConfigFile)
	if strings.Contains(string(data), `" ALL "`) {
		t.Fatal("保存后未规范化保留键 all")
	}
	global := decodeObject(t, data)["models"].(map[string]any)["accounts"].(map[string]any)["all"].(map[string]any)
	if _, exists := global["allowlist"]; exists {
		t.Fatal("网页账号操作必须删除 all.allowlist 字段")
	}
	setModelFilter([]string{"off"}, nil)
	result = probeAccountModel(context.Background(), accounts[1], "off")
	if result.Status != "skipped" {
		t.Fatal("关闭的模型不应被显式探测")
	}
}

func TestAccountToggleConvertsGlobalWhitelistPreservingOtherStates(t *testing.T) {
	for _, tc := range []struct {
		name, global, target string
		enable               bool
		blocked              []string
	}{
		{"开启白名单外账号", `{"allowlist":["a.json"]}`, "b.json", true, []string{"c.json"}},
		{"关闭最后一个白名单账号", `{"allowlist":["a.json"]}`, "a.json", false, []string{"a.json", "b.json", "c.json"}},
		{"保留原黑名单和未加载账号", `{"allowlist":["a.json","b.json"],"blocklist":["b.json","offline.json"]}`, "a.json", false, []string{"a.json", "b.json", "c.json", "offline.json"}},
		{"开启白名单内但被黑名单拒绝的账号", `{"allowlist":["a.json","b.json"],"blocklist":["b.json"]}`, "b.json", true, []string{"c.json"}},
		{"空白名单不限制其他账号", `{"allowlist":[],"blocklist":["b.json"]}`, "a.json", false, []string{"a.json", "b.json"}},
		{"null白名单删除字段", `{"allowlist":null,"blocklist":["b.json"]}`, "b.json", true, []string{}},
		{"null全局规则", `null`, "b.json", false, []string{"b.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatePolicies(t)
			content := `{"gateway":{"adminKey":"admin"},"systemPrompt":{"force":"keep"},"models":{"allowlist":["m"],"accounts":{"m":{"allowlist":["a.json"],"blocklist":null}," ALL ":` + tc.global + `}}}`
			writeRuntimeFixture(t, content)
			if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
				t.Fatal(err)
			}
			accounts = []*Account{{Path: "a.json"}, {Path: "b.json"}, {Path: "c.json"}}
			handler := newWebUIHandler()
			body, _ := json.Marshal(map[string]any{"target": tc.target, "enabled": tc.enable})
			response := adminRequest(handler, http.MethodPost, "/admin/api/account-toggle", "admin", string(body))
			if response.Code != http.StatusOK {
				t.Fatalf("开关保存失败: %d %s", response.Code, response.Body.String())
			}
			raw, _ := os.ReadFile(runtimeConfigFile)
			doc := decodeObject(t, raw)
			models := doc["models"].(map[string]any)
			rules := models["accounts"].(map[string]any)
			global := rules["all"].(map[string]any)
			if _, exists := global["allowlist"]; exists || len(global) != 1 {
				t.Fatalf("all必须只写入blocklist: %+v", global)
			}
			if _, exists := rules[" ALL "]; exists {
				t.Fatal("旧all拼写没有删除")
			}
			var typed runtimeFileConfig
			_ = json.Unmarshal(raw, &typed)
			gotBlocks := typed.Models.Accounts["all"].Blocklist
			got, _ := json.Marshal(gotBlocks)
			want, _ := json.Marshal(tc.blocked)
			if string(got) != string(want) {
				t.Fatalf("黑名单=%s，期望=%s", got, want)
			}
			for _, acc := range accounts {
				blocked := false
				for _, name := range tc.blocked {
					blocked = blocked || acc.Path == name
				}
				if allowed, _ := modelAccountAllowed("unconfigured", acc, accounts); allowed == blocked {
					t.Fatalf("账号%s转换后的全局状态错误", acc.Path)
				}
			}
			original := decodeObject(t, []byte(content))["models"].(map[string]any)["accounts"].(map[string]any)["m"]
			before, _ := json.Marshal(original)
			after, _ := json.Marshal(rules["m"])
			if string(before) != string(after) || configuredSystemPrompts().force != "keep" || currentModelPolicyMode() != "allowlist" {
				t.Fatal("转换all改变了模型名单、模型专属账号规则或提示词")
			}
			if ok, _ := modelAccountAllowed("m", accounts[1], accounts); ok {
				t.Fatal("全局启用绕过模型专属白名单")
			}
			fresh := &Account{Path: "new.json"}
			if ok, _ := modelAccountAllowed("unconfigured", fresh, []*Account{fresh}); !ok {
				t.Fatal("转换为黑名单后新账号应默认开启")
			}
			files, _ := filepath.Glob(filepath.Join(logDir, "gateway-*.log"))
			logData, _ := os.ReadFile(files[0])
			if !strings.Contains(string(logData), "[账号开关规则] traceId=gateway-config-test") {
				t.Fatal("转换过程未使用请求TraceID落盘")
			}
		})
	}
}

func TestAccountWhitelistConversionKeepsDuplicateNamesBlocked(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"models":{"accounts":{"all":{"allowlist":["a.json","same.json"]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	accounts = []*Account{{Path: "a.json"}, {Path: "first/same.json"}, {Path: "second/same.json"}}
	if err := savePolicyToggle("account", "a.json", true, "duplicates"); err != nil {
		t.Fatal(err)
	}
	for _, acc := range accounts[1:] {
		if allowed, _ := modelAccountAllowed("m", acc, accounts); allowed {
			t.Fatal("转换后放开了原来被拒绝的重名账号")
		}
	}
	before, _ := os.ReadFile(runtimeConfigFile)
	if err := savePolicyToggle("account", "first/same.json", true, "duplicates"); err == nil {
		t.Fatal("重名账号不能通过网页单独启用")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if string(before) != string(after) {
		t.Fatal("拒绝重名目标时不应写入文件")
	}
}

func TestRuntimeLoopHotKeysDebugAndPorts(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"apiPort":9317,"webPort":9316,"adminKey":"first"}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtimeConfigLoop(ctx) }()
	defer func() { cancel(); <-done }()
	writeRuntimeFixture(t, `{"gateway":{"apiPort":9417,"webPort":9416,"adminKey":"second","apiKeyEnabled":true,"apiKey":"api"},"debug":{"enabled":true}}`)
	deadline := time.Now().Add(4 * time.Second)
	for currentAdminKey() != "second" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if currentAdminKey() != "second" || currentAPIKey() != "api" || !debugLoggingEnabled() {
		t.Fatal("实际循环没有热更新")
	}
	if cfg.Port != 9317 || cfg.WebPort != 9316 {
		t.Fatal("热更新不能修改实际监听端口展示")
	}
	r := httptest.NewRequest("GET", "/", nil)
	debugEvent(r, "debug", "hot_reload_test", nil)
	files, _ := filepath.Glob(filepath.Join(logDir, "debug-*.jsonl"))
	if len(files) != 1 {
		t.Fatal("热开启调试未落盘")
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "hot_reload_test") {
		t.Fatal("热调试事件未写入")
	}
}

func TestGlobalAccountRuleCannotBeOverridden(t *testing.T) {
	isolatePolicies(t)
	a, b := &Account{Path: "a.json", Auth: &StoredAuth{}}, &Account{Path: "b.json", Auth: &StoredAuth{}}
	accounts = []*Account{a, b}
	if err := setModelAccountFilter(map[string]modelAccountFileConfig{
		"all": {Blocklist: []string{"a.json"}},
		"m":   {Allowlist: []string{"a.json"}},
	}); err != nil {
		t.Fatal(err)
	}
	if ok, reason := modelAccountAllowed("m", a, accounts); ok || !strings.Contains(reason, "全局") {
		t.Fatal("模型白名单绕过全局停用")
	}
	if acc, _, err := nextAccountForModel("other", nil); err != nil || acc != b {
		t.Fatalf("未配置模型必须遵守全局规则: %v", err)
	}
	if _, _, err := nextAccountForModel("m", nil); err == nil {
		t.Fatal("应拒绝无法调度的模型")
	}
	if rows := buildAllModelStatSnapshots(time.Now(), accounts); len(rows) != 1 || rows[0].ID != "m" || rows[0].AvailableAccounts != 0 {
		t.Fatalf("all 不应成为虚假模型: %+v", rows)
	}
}

func TestWebTogglePreservesConfigAndConvertsAllowlist(t *testing.T) {
	isolatePolicies(t)
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}, {ID: "two"}, {ID: "three"}}}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"systemPrompt":{"force":"synthetic preserve"},"models":{"allowlist":["one"],"accounts":{"all":{"blocklist":[]},"two":{"allowlist":["a.json"]}}}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	a, b := &Account{Path: "a.json", Auth: &StoredAuth{}}, &Account{Path: "b.json", Auth: &StoredAuth{}}
	accounts = []*Account{a, b}
	handler := newWebUIHandler()
	w := adminRequest(handler, "POST", "/admin/api/account-toggle", "admin", `{"target":"a.json","enabled":false}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if a.Disabled || a.Auth == nil {
		t.Fatal("开关不应修改授权失效或删除凭据")
	}
	for _, model := range []string{"one", "two", "unknown"} {
		if ok, _ := modelAccountAllowed(model, a, accounts); ok {
			t.Fatalf("全局停用未应用到 %s", model)
		}
	}
	rows := []accountSnapshot{{Path: "a.json", State: "active"}, {Path: "b.json", State: "active"}}
	annotateAccountPolicies(rows)
	if *rows[0].SchedulingEnabled || summarizeAccounts(rows)["paused"] != 1 {
		t.Fatal("账号开关/总览未反映停用")
	}
	w = adminRequest(handler, "POST", "/admin/api/model-toggle", "admin", `{"target":"two","enabled":true}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(runtimeConfigFile)
	obj := decodeObject(t, data)
	models := obj["models"].(map[string]any)
	if _, exists := models["allowlist"]; exists {
		t.Fatal("网页写入必须删除白名单键")
	}
	for id, want := range map[string]bool{"one": true, "two": true, "three": false} {
		disabled, _ := modelDisabled(id)
		if disabled == want {
			t.Fatalf("转换丢失模型 %s 原状态", id)
		}
	}
	if configuredSystemPrompts().force != "synthetic preserve" || !strings.Contains(string(data), `"a.json"`) {
		t.Fatal("写入丢失其他配置")
	}
	writeStatusSnapshot()
	w = adminRequest(handler, "GET", "/admin/api/models", "admin", "")
	var view struct {
		Models []modelStatSnapshot `json:"models"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if len(view.Models) != 3 {
		t.Fatalf("停用模型没有保留在管理列表: %s", w.Body.String())
	}
	w = adminRequest(handler, "POST", "/admin/api/model-toggle", "admin", `{"target":"three","enabled":true}`)
	if w.Code != 200 {
		t.Fatalf("无法重新启用: %s", w.Body.String())
	}
	w = adminRequest(handler, "POST", "/admin/api/account-toggle", "admin", `{"target":"a.json","enabled":true}`)
	if w.Code != 200 {
		t.Fatalf("无法重新启用账号: %s", w.Body.String())
	}
	if ok, _ := modelAccountAllowed("two", a, accounts); !ok {
		t.Fatal("恢复后没有参与模型专属调度")
	}
	if ok, _ := modelAccountAllowed("two", b, accounts); ok {
		t.Fatal("开关不应覆盖模型专属规则")
	}
}

func TestWebPolicyWritesGuardedAndNoLostUpdates(t *testing.T) {
	isolatePolicies(t)
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"}}`)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}, {ID: "two"}}}
	accounts = []*Account{{Path: "a.json"}, {Path: "b.json"}}
	handler := newWebUIHandler()
	for _, tc := range []struct {
		key, body string
		status    int
	}{
		{"", `{"target":"one","enabled":false}`, 401},
		{"admin", `{"target":"one"}`, 400},
		{"admin", `{"target":"nonexistent","enabled":false}`, 400},
		{"admin", `{"target":"one","enabled":"false"}`, 400},
	} {
		w := adminRequest(handler, "POST", "/admin/api/model-toggle", tc.key, tc.body)
		if w.Code != tc.status {
			t.Fatalf("状态码=%d want=%d %s", w.Code, tc.status, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "http://localhost/admin/api/model-toggle", strings.NewReader(`{"target":"one","enabled":false}`))
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Workbuddy-Admin", "1")
	r.Header.Set("Origin", "https://other.example")
	wrongOrigin := httptest.NewRecorder()
	handler.ServeHTTP(wrongOrigin, r)
	if wrongOrigin.Code != http.StatusForbidden {
		t.Fatal("跨站写入应在业务前被拦截")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, target := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); errs <- savePolicyToggle("model", id, false, "concurrent") }(target)
	}
	wg.Add(1)
	go func() { defer wg.Done(); errs <- saveAPIKeySettings(true, "api", "concurrent") }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"one", "two"} {
		if off, _ := modelDisabled(id); !off {
			t.Fatal("并发写入丢失模型状态")
		}
	}
	data, _ := os.ReadFile(runtimeConfigFile)
	if !strings.Contains(string(data), `"api"`) || currentAPIKey() != "api" {
		t.Fatal("并发写入丢失鉴权设置")
	}
	// 外部修改管理 Key 后旧会话不能继续覆盖文件。
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"external"}}`)
	w := adminRequest(handler, "POST", "/admin/api/model-toggle", "admin", `{"target":"one","enabled":true}`)
	if w.Code != 409 {
		t.Fatalf("外部配置冲突应拒绝覆盖: %d", w.Code)
	}
}

func TestPolicyWriteAndDebugFailuresPreserveState(t *testing.T) {
	isolatePolicies(t)
	original := `{"gateway":{"adminKey":"admin"},"models":{"blocklist":[]}}`
	writeRuntimeFixture(t, original)
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	catalogModels = map[string][]catalogModel{"cn": {{ID: "one"}}}
	// 无效的磁盘配置不能被页面“顺手修好”或覆盖。
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"admin"},"models":{"accounts":{"all":{"blocklist":["bad/path.json"]}}}}`)
	if err := savePolicyToggle("model", "one", false, "invalid-disk"); err == nil {
		t.Fatal("坏配置不能覆盖")
	}
	if disabled, _ := modelDisabled("one"); disabled {
		t.Fatal("写入失败改变了运行时状态")
	}
	writeRuntimeFixture(t, original)
	// 已有文件占用 logs 路径时，开启调试失败必须保留其他有效配置。
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	if err := os.Chdir(temp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldCwd)
	if err := os.WriteFile(logDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"changed"},"debug":{"enabled":true},"models":{"blocklist":["one"]}}`)
	watcher := runtimeConfigWatcher{}
	if watcher.reload() {
		t.Fatal("调试日志不可写仍然加载了配置")
	}
	if currentAdminKey() != "admin" || debugLoggingEnabled() {
		t.Fatal("调试失败改变了其他运行时状态")
	}
	if disabled, _ := modelDisabled("one"); disabled {
		t.Fatal("调试失败改变了模型规则")
	}
}

func TestHotTransportClonesForChangedHeaderTimeout(t *testing.T) {
	isolatePolicies(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	base := &http.Transport{ResponseHeaderTimeout: time.Second}
	transport := &hotConfigTransport{transport: base}
	defer transport.CloseIdleConnections()
	upstreamConfigMu.Lock()
	upstreamHeaderTimeout = 3 * time.Second
	upstreamConfigMu.Unlock()
	client := &http.Client{Transport: transport}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if base.ResponseHeaderTimeout != time.Second || transport.transport == base || transport.transport.ResponseHeaderTimeout != 3*time.Second {
		t.Fatal("活跃 Transport 被原地修改")
	}
}
