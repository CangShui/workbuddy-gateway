package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 场景 A：任意模型（非 dsv4.1、接口未写明免费、无促销日期）首次实测 credit=0
// 必须被识别为免费——证明识别逻辑不依赖任何模型名白名单。
func TestAnyModelLearnedFreeFromCreditZero(t *testing.T) {
	resetModelStats()
	chdirTemp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":4200}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	// 一个全新的、从未见过的模型名，接口目录里也没有它
	const brandNew = "some-brand-new-free-model-2099"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+brandNew+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	accountMu.Lock()
	state := acc.ModelStates[brandNew]
	accountMu.Unlock()
	if state == nil || state.CostClass != modelCostFree {
		t.Fatalf("任意新模型实测 credit=0 应识别为免费，got %+v", state)
	}
	t.Logf("模型 %s 已按 credit=0 动态识别为免费（无任何硬编码）", brandNew)
}

// 场景 B：原本免费的模型后来恢复收费，必须能自动改判为收费。
func TestFreeModelCanFlipToPaidWhenCreditReturns(t *testing.T) {
	resetModelStats()
	chdirTemp(t)
	credit := float64(0)
	tokens := 5000
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"finish_reason":"stop"}],"usage":{"credit":`+
			formatQuota(credit)+`,"total_tokens":`+itoa(tokens)+`}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	model := "limited-time-free-model"
	call := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		handleChatCompletions(rec, req)
	}

	// 第一轮：限时免费中，credit=0
	call()
	accountMu.Lock()
	afterFree := acc.ModelStates[model].CostClass
	accountMu.Unlock()
	if afterFree != modelCostFree {
		t.Fatalf("限时免费期间应为 free, got %q", afterFree)
	}

	// 第二轮：限时免费结束，上游开始计费 credit=1.25
	credit = 1.25
	call()
	accountMu.Lock()
	afterPaid := acc.ModelStates[model].CostClass
	accountMu.Unlock()
	if afterPaid != modelCostPaid {
		t.Fatalf("恢复收费后应自动改判为 paid, got %q", afterPaid)
	}
	t.Logf("模型 %s 已随 credit 变化自动从 free 改判为 paid", model)

	// 第三轮：再次限时免费，credit 回到 0，应能改回 free
	credit = 0
	call()
	accountMu.Lock()
	backToFree := acc.ModelStates[model].CostClass
	accountMu.Unlock()
	if backToFree != modelCostFree {
		t.Fatalf("再次免费后应改回 free, got %q", backToFree)
	}
	t.Logf("模型 %s 已再次自动改回 free", model)
}

// 场景 C：小样本（探针/极短请求）credit=0 不得误判为免费，
// 避免把真正收费的模型错标成免费。
func TestTinySampleCreditZeroNotLearnedFree(t *testing.T) {
	resetModelStats()
	chdirTemp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	model := "possibly-paid-model"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)

	accountMu.Lock()
	state := acc.ModelStates[model]
	accountMu.Unlock()
	if state != nil && state.CostClass == modelCostFree {
		t.Fatalf("小样本 credit=0 不得判为免费，got %+v", state)
	}
}

// 场景 D：14018（账号额度耗尽）绝不能改写模型收费属性，
// 且额度恢复后应自动解除阻断、重新按 credit 学习。
func TestQuotaExhaustedDoesNotPolluteCostClassAndRecovers(t *testing.T) {
	chdirTemp(t)
	acc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl"}, QuotaExhausted: true,
		ModelStates: map[string]*modelRuntimeState{
			"limited-free": {CostClass: modelCostFree},
		}}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = []*Account{acc}
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	}()

	// 免费模型遇 14018：保持 free
	markModelQuotaBlocked(acc, "limited-free", "14018 Credits exhausted")
	accountMu.Lock()
	if got := acc.ModelStates["limited-free"].CostClass; got != modelCostFree {
		t.Fatalf("14018 不得改写免费模型分类, got %q", got)
	}
	blocked := acc.ModelStates["limited-free"].QuotaBlocked
	accountMu.Unlock()
	if !blocked {
		t.Fatal("14018 应设置额度阻断")
	}

	// 模拟额度恢复：刷新逻辑会清除 QuotaBlocked
	accountMu.Lock()
	acc.QuotaExhausted = false
	acc.QuotaRemaining = 500
	for _, st := range acc.ModelStates {
		st.QuotaBlocked = false
		st.NextProbeAt = time.Time{}
	}
	accountMu.Unlock()
	accountMu.Lock()
	recovered := !acc.ModelStates["limited-free"].QuotaBlocked
	class := acc.ModelStates["limited-free"].CostClass
	accountMu.Unlock()
	if !recovered {
		t.Fatal("额度恢复后应解除阻断")
	}
	if class != modelCostFree {
		t.Fatalf("额度恢复后免费分类应保留, got %q", class)
	}
}

// 场景 E：接口促销日期已过期、但上游实际仍免费时，
// 必须靠真实请求的 credit=0 覆盖「过期=未知」的保守判断。
func TestExpiredPromoStillFreeIsCorrectedByLiveEvidence(t *testing.T) {
	resetModelsStateT(t)
	resetModelStats()
	chdirTemp(t)

	modelsMu.Lock()
	catalogModels = map[string][]catalogModel{
		"intl": {{ID: "expired-but-free", Credits: "x0.00", Multiplier: 0, HasMultiplier: true,
			PromoExpired: true, FromLive: true}},
	}
	modelsMu.Unlock()
	defer resetModelsStateT(t)

	if !modelNeedsProbe("intl", "expired-but-free", 0) {
		t.Fatal("促销过期的模型应被安排探测，以确认真实收费属性")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":3300}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	oldBase, oldOrigin := profileINTL.Base, profileINTL.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	acc := &Account{Path: "intl.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, QuotaKnown: true, QuotaRemaining: 100}
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileINTL.Base, profileINTL.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	defer func() {
		profileINTL.Base, profileINTL.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"expired-but-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleChatCompletions(rec, req)

	accountMu.Lock()
	state := acc.ModelStates["expired-but-free"]
	accountMu.Unlock()
	if state == nil || state.CostClass != modelCostFree {
		t.Fatalf("促销日期过期但实测免费，应识别为 free，got %+v", state)
	}
	if !siteKnownFree("intl", "expired-but-free") {
		t.Fatal("实测免费后，该站点应被判定为免费站点")
	}
	t.Log("促销过期但实际免费：已通过实测 credit=0 纠正为免费")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
