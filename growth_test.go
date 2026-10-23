package main

// 成长任务（growth.go / growthadmin.go）的单元测试。
//
// 风格沿用仓库既有约定：表驱动 + httptest.NewServer 打桩上游 +
// 全局变量替换后 defer 还原（见 main_test.go 的 TestCheckinAccountCNAndSkipIntl）。
//
// ⚠️ 这些用例**刻意不碰真实上游**：成长任务会改变账号当日状态（接取/领奖），
// 真实账号的验证由 e2e 手工跑，不进默认测试。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// growthStub 起一个打桩上游，返回固定的成长任务列表与领取响应。
type growthStub struct {
	server   *httptest.Server
	paths    []string
	authSeen string
	uidSeen  string
	domain   string
	platform string
}

func newGrowthStub(t *testing.T, tasks []growthTaskItem, claimCode int, claimMsg string, claimCredit float64) *growthStub {
	t.Helper()
	stub := &growthStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.paths = append(stub.paths, r.URL.Path)
		stub.authSeen = r.Header.Get("Authorization")
		stub.uidSeen = r.Header.Get("X-User-Id")
		stub.domain = r.Header.Get("X-Domain")
		stub.platform = r.Header.Get("X-Client-Platform")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == growthTasksV2Path:
			// 成长族：只认 task_code + accept_status。
			var v2 []growthTaskItem
			for _, task := range tasks {
				if task.TaskCode != "" {
					v2 = append(v2, task)
				}
			}
			_, _ = w.Write(encodeGrowthBody(t, 0, "", v2))
		case r.URL.Path == growthTasksLegacyPath:
			// 等级族：只认 code + status。
			var legacy []growthTaskItem
			for _, task := range tasks {
				if task.TaskCode == "" {
					legacy = append(legacy, task)
				}
			}
			_, _ = w.Write(encodeGrowthBody(t, 0, "", legacy))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			data := map[string]any{"already_claimed": false, "credit": claimCredit}
			if claimCode != 0 {
				_, _ = w.Write([]byte(`{"code":` + strconv.Itoa(claimCode) + `,"msg":"` + claimMsg + `","data":{}}`))
				return
			}
			_, _ = w.Write(encodeGrowthRaw(t, 0, claimMsg, data))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func encodeGrowthBody(t *testing.T, code int, msg string, tasks []growthTaskItem) []byte {
	t.Helper()
	if tasks == nil {
		tasks = []growthTaskItem{}
	}
	return encodeGrowthRaw(t, code, msg, map[string]any{"tasks": tasks})
}

func encodeGrowthRaw(t *testing.T, code int, msg string, data any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"code": code, "msg": msg, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 删除本地 itoa：仓库已有同名 helper（model_cost_dynamic_test.go），重复声明会编译失败。
// 整数转字符串统一用 strconv.Itoa。

// isolateGrowth 把国内站 Origin 指向打桩服务器，并还原。
func isolateGrowth(t *testing.T, url string) {
	t.Helper()
	oldOrigin := profileCN.Origin
	oldClient := cfg.HttpClient
	profileCN.Origin = url
	cfg.HttpClient = http.DefaultClient
	t.Cleanup(func() {
		profileCN.Origin = oldOrigin
		cfg.HttpClient = oldClient
	})
}

// growthCN 构造一个可用于成长任务的国内站账号。
func growthCN(uid string) *Account {
	return &Account{
		Path: "cn.json",
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "test-access", Domain: "copilot.tencent.com"},
			Account: StoredAccount{UID: uid, Nickname: "tester"},
		},
	}
}

// TestGrowthSkipsIntlAndEnterprise 守卫：国际站与企业版绝不发成长请求。
//
// 这是 !62 复审结论的机器化 —— 国际站后端没有成长中心，照发必然失败且是真实请求。
func TestGrowthSkipsIntlAndEnterprise(t *testing.T) {
	stub := newGrowthStub(t, nil, 0, "", 0)
	isolateGrowth(t, stub.server.URL)

	cases := []struct {
		name string
		acc  *Account
		want string
	}{
		{"国际站", &Account{Path: "intl.json", Auth: &StoredAuth{
			Edition: "intl", Auth: StoredTokens{AccessToken: "test-access"},
		}}, "国际站不支持成长任务"},
		{"企业版", &Account{Path: "ent.json", Auth: &StoredAuth{
			Edition: "cn", Auth: StoredTokens{AccessToken: "test-access"},
			Account: StoredAccount{UID: "u1", EnterpriseID: "ent-1"},
		}}, "企业版不参与个人成长任务"},
		{"无凭据", &Account{Path: "none.json"}, "账号不可用"},
	}
	for _, tc := range cases {
		got := runGrowthForAccount(context.Background(), tc.acc, 0)
		if !strings.Contains(got.Error, tc.want) {
			t.Errorf("%s: error=%q 应含 %q", tc.name, got.Error, tc.want)
		}
		if len(stub.paths) != 0 {
			t.Fatalf("%s 不应发起任何上游请求，实际打到 %v", tc.name, stub.paths)
		}
	}
}

// TestGrowthFetchesBothTaskFamilies 守卫：两族端点都必须打且结果要合并。
//
// 只打 /v2/... 会让「养虾等级」族永远进不了待办 —— 这是插件侧实测过的缺陷。
func TestGrowthFetchesBothTaskFamilies(t *testing.T) {
	stub := newGrowthStub(t, []growthTaskItem{
		{TaskCode: "chat_5", AcceptStatus: "not_accepted"},
		{Code: "first_chat", Status: "available"},
	}, 0, "", 0)
	isolateGrowth(t, stub.server.URL)

	query := fetchGrowthTaskCodes(context.Background(), growthAuth{
		accessToken: "test-access", uid: "u1",
	}, &profileCN)
	if !query.ok {
		t.Fatalf("查询失败: %s", query.message)
	}
	if !contains(query.codes, "chat_5") || !contains(query.codes, "first_chat") {
		t.Fatalf("两族任务必须都进入待办，实际=%v", query.codes)
	}
	if !containsPath(stub.paths, growthTasksV2Path) || !containsPath(stub.paths, growthTasksLegacyPath) {
		t.Fatalf("两族端点都必须请求，实际=%v", stub.paths)
	}
}

// TestGrowthSendsExpectedHeaders 守卫：成长请求带齐身份头与 X-Client-Platform。
func TestGrowthSendsExpectedHeaders(t *testing.T) {
	stub := newGrowthStub(t, nil, 0, "", 0)
	isolateGrowth(t, stub.server.URL)

	// 返回值只用于驱动真实请求；这里断言请求本身，故忽略结果。
	_ = fetchGrowthTaskCodes(context.Background(), growthAuth{
		accessToken: "test-access", uid: "user-1",
	}, &profileCN)
	if stub.authSeen != "Bearer test-access" {
		t.Errorf("Authorization=%q", stub.authSeen)
	}
	if stub.uidSeen != "user-1" {
		t.Errorf("X-User-Id=%q", stub.uidSeen)
	}
	// 成长中心前端固定该头（签到端点不带），必须带。
	if stub.platform != "web" {
		t.Errorf("X-Client-Platform=%q，成长端点必须为 web", stub.platform)
	}
	// X-Domain 以产品配置为准，不跟凭据快照走。
	if stub.domain != "copilot.tencent.com" {
		t.Errorf("X-Domain=%q，应取 profile 配置", stub.domain)
	}
}

// TestGrowthClaimSuccessRequiresExplicitZero 守卫：领取成功必须显式 code==0。
//
// 「响应里没有 code」不能被当成成功 —— 否则网关错误体会被谎报成「已领取 +0 积分」。
func TestGrowthClaimSuccessRequiresExplicitZero(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOK     bool
		wantClaim  bool
		wantCredit float64
	}{
		{"无code字段", `{"msg":"Unauthorized"}`, false, false, 0},
		{"空对象", `{}`, false, false, 0},
		{"code非0", `{"code":400,"msg":"task not completed","data":{}}`, false, false, 0},
		{"已领取", `{"code":0,"msg":"OK","data":{"already_claimed":true}}`, true, false, 0},
		{"领取成功", `{"code":0,"msg":"OK","data":{"credit":120}}`, true, true, 120},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.body))
		}))
		isolateGrowth(t, server.URL)
		got := claimGrowthTaskReward(context.Background(), growthAuth{accessToken: "t"}, &profileCN, "chat_5")
		if got.ok != tc.wantOK || got.claimed != tc.wantClaim || got.credit != tc.wantCredit {
			t.Errorf("%s: ok=%v claimed=%v credit=%v，期望 ok=%v claimed=%v credit=%v",
				tc.name, got.ok, got.claimed, got.credit, tc.wantOK, tc.wantClaim, tc.wantCredit)
		}
		server.Close()
	}
}

// TestGrowthNoObjectWhitelistStaysNarrow 守卫：code 400 只有命中白名单才算「无可领对象」。
//
// 放宽会把「任务真没做完」说成「已领到积分」，与实际相反。
func TestGrowthNoObjectWhitelistStaysNarrow(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"no unclaimed travel", true},
		{"insufficient energy", true},
		{"please accept buddy adoption agreement first", true},
		{"task not completed", false},
		{"brand new server message", false},
	}
	for _, tc := range cases {
		got := isGrowthNoObject(growthEnvelope{Code: intPtr(400), Msg: tc.msg})
		if got != tc.want {
			t.Errorf("isGrowthNoObject(%q)=%v，期望 %v", tc.msg, got, tc.want)
		}
	}
	// 非 400 一律不算。
	if isGrowthNoObject(growthEnvelope{Code: intPtr(0), Msg: "no unclaimed travel"}) {
		t.Error("code==0 不应被判为无可领对象")
	}
}

// TestGrowthExpiredCredentialDetected 守卫：凭据失效要被识别且不计入 failed。
func TestGrowthExpiredCredentialDetected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html>unauthorized</html>"))
	}))
	defer server.Close()
	isolateGrowth(t, server.URL)

	got := runGrowthForAccount(context.Background(), growthCN("u1"), 0)
	// 僵尸账号的处置是「重新登录」，与「任务推不动」不同：不计 failed，给 error。
	if got.Failed != 0 {
		t.Errorf("凭据失效不应计入 failed，实际=%d", got.Failed)
	}
	if !strings.Contains(got.Error, "凭据已失效") {
		t.Errorf("error=%q 应提示凭据已失效", got.Error)
	}
	if len(got.Tasks) == 0 {
		t.Error("失败原因必须进 results，否则「请重新登录」会被说成「没有任务」")
	}
	found := false
	for _, item := range got.Tasks {
		if strings.Contains(item.Message, "凭据已失效") {
			found = true
		}
	}
	if !found {
		t.Errorf("results 里应含「凭据已失效」，实际=%+v", got.Tasks)
	}
}

// TestGrowthBudgetTimeoutMarksNotExecuted 守卫：预算耗尽要标 timedOut 且文案含「未执行」。
func TestGrowthBudgetTimeoutMarksNotExecuted(t *testing.T) {
	stub := newGrowthStub(t, []growthTaskItem{
		{TaskCode: "chat_5", AcceptStatus: "not_accepted"},
	}, 0, "", 0)
	isolateGrowth(t, stub.server.URL)

	// deadline 已过 ⇒ 所有完成动作都不执行。
	report := runGrowthTasks(context.Background(), growthAuth{accessToken: "t", uid: "u1"},
		&profileCN, time.Now().Add(-time.Minute))
	if !report.timedOut {
		t.Error("预算耗尽必须置 timedOut=true")
	}
	found := false
	for _, item := range report.results {
		if strings.Contains(item.Message, "未执行") {
			found = true
		}
	}
	if !found {
		t.Errorf("文案必须含「未执行」，实际=%+v", report.results)
	}
}

// TestGrowthRunAllDisabledByDefault 守卫：默认关闭，不许偷偷发请求。
//
// 这是安全属性（会发真实写请求），不是可随手改的旋钮。
func TestGrowthRunAllDisabledByDefault(t *testing.T) {
	old := growthTasksEnabled.Swap(false)
	defer growthTasksEnabled.Store(old)

	if _, err := runAllGrowthTasks(context.Background()); err == nil {
		t.Fatal("默认关闭时必须返回错误，实际允许执行")
	}
}

// TestGrowthTogglePersists 守卫：开关写入后运行时生效并落盘。
func TestGrowthTogglePersists(t *testing.T) {
	isolateAdminFeatures(t)
	old := growthTasksEnabled.Swap(false)
	defer growthTasksEnabled.Store(old)

	if err := saveGrowthEnabled(true, newTraceID()); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !growthTasksEnabled.Load() {
		t.Fatal("写入后运行时未生效")
	}
	raw, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"growth"`) {
		t.Errorf("config.json 未落盘 growth 段: %s", raw)
	}
}

func intPtr(n int) *int { return &n }

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func containsPath(list []string, want string) bool { return contains(list, want) }
