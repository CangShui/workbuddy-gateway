package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testPlanResource(code, name string) planResource {
	return planResource{PackageCode: code, PackageName: name, SubProductCode: "sp_tcaca_codebuddy_ide", Status: json.RawMessage("0")}
}

func TestIdentifyPlanFromEffectiveResources(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	free := testPlanResource("TCACA_code_035_ArVxJcGDsm", "Free Plan Subscription")
	trial := testPlanResource("TCACA_code_039_KRcQj7wUat", "Pro Plan Trial Subscription")
	pro := testPlanResource("TCACA_code_002_AkiJS3ZHF5", "Pro Plan Monthly Subscription")
	expired := trial
	expired.Status = json.RawMessage("2")
	expiredByTime := trial
	expiredByTime.DeductionEndTime = json.RawMessage(strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10))
	exhausted := pro
	exhausted.Status = json.RawMessage(`"3"`)
	exhausted.DeductionEndTime = json.RawMessage(strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10))
	exhausted.ExpiredTime = json.RawMessage(`"2026-09-01 00:00:00"`)
	exhausted.CycleEndTime = json.RawMessage(`"2026-09-30 23:59:59"`)
	future := pro
	future.DeductionStartTime = json.RawMessage(strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10))
	bonus := testPlanResource("TCACA_code_007_nzdH5h4Nl0", "Bonus Pack")
	bonus.SubProductCode = "sp_tcaca_codebuddyide_bonus_pack"
	unknownProduct := testPlanResource("future-product", "Future Subscription")
	unknownProduct.SubProductCode = "unknown-product"
	unknownStatus := pro
	unknownStatus.Status = json.RawMessage("9")
	badTime := pro
	badTime.DeductionEndTime = json.RawMessage(`"changed-format"`)
	missingStatus := pro
	missingStatus.Status = nil
	newTier := testPlanResource("new-tier-2027", "Business Ultra 2027")
	for _, tc := range []struct {
		name    string
		summary quotaSummaryData
		rows    []planResource
		want    string
		wantErr bool
	}{
		{"never_trial", quotaSummaryData{}, []planResource{free}, "免费", false},
		{"stale_trial_flag_free", quotaSummaryData{ProTrialStatus: float64(1)}, []planResource{free}, "免费", false},
		{"trial_expired_status", quotaSummaryData{ProTrialStatus: "1"}, []planResource{free, expired}, "免费", false},
		{"trial_expired_time", quotaSummaryData{ProTrialStatus: 1}, []planResource{free, expiredByTime}, "免费", false},
		{"active_trial", quotaSummaryData{ProTrialStatus: 1}, []planResource{free, trial}, "Pro试用", false},
		{"active_paid", quotaSummaryData{IsPaidUser: true}, []planResource{free, pro}, "pro", false},
		{"ordinary_trial_does_not_override_paid", quotaSummaryData{ProTrialStatus: 1}, []planResource{trial, pro}, "pro", false},
		{"converted_trial_still_active", quotaSummaryData{ProTrialStatus: float64(2)}, []planResource{trial, pro}, "Pro试用", false},
		{"used_up_not_expired", quotaSummaryData{IsPaidUser: true}, []planResource{exhausted}, "pro", false},
		{"future_subscription", quotaSummaryData{}, []planResource{free, future}, "免费", false},
		{"bonus_is_not_subscription", quotaSummaryData{ProTrialStatus: 1}, []planResource{free, bonus}, "免费", false},
		{"empty_valid_list", quotaSummaryData{}, nil, "免费", false},
		{"new_plan_official_name", quotaSummaryData{}, []planResource{free, newTier}, "Business Ultra 2027", false},
		{"explicit_current_subscription", quotaSummaryData{SubscriptionPackageCode: newTier.PackageCode}, []planResource{pro, newTier}, "Business Ultra 2027", false},
		{"unnamed_future_subscription", quotaSummaryData{}, []planResource{free, testPlanResource("new-unnamed", "")}, "未识别订阅", false},
		{"renamed_existing_tier", quotaSummaryData{}, []planResource{testPlanResource(pro.PackageCode, "Pro Plus New")}, "Pro Plus New", false},
		{"paid_summary_without_resource", quotaSummaryData{IsPaidUser: true}, []planResource{free}, "", true},
		{"subscription_summary_without_resource", quotaSummaryData{SubscriptionPackageCode: "missing"}, []planResource{free}, "", true},
		{"new_subproduct_not_silently_free", quotaSummaryData{}, []planResource{free, unknownProduct}, "", true},
		{"unknown_status", quotaSummaryData{}, []planResource{free, unknownStatus}, "", true},
		{"missing_status", quotaSummaryData{}, []planResource{free, missingStatus}, "", true},
		{"invalid_expiry", quotaSummaryData{}, []planResource{free, badTime}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := identifyPlan("intl", tc.summary, tc.rows, now)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got=%q err=%v want=%q error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestRegionalPlanNamesAndFutureTiers(t *testing.T) {
	for _, tc := range []struct {
		site, code, name, want string
	}{
		{"cn", "TCACA_code_008_cfWoLwvjU4", "CodeBuddy个人体验版", "体验版"},
		{"cn", "TCACA_code_002_AkiJS3ZHF5", "Pro Plan Monthly Subscription", "标准版"},
		{"cn", "TCACA_code_026_BaESVICNoi", "", "高级版"},
		{"cn", "TCACA_code_027_0FCGVA6vSa", "CodeBuddy个人旗舰版", "旗舰版"},
		{"intl", "TCACA_code_002_AkiJS3ZHF5", "Pro Plan Monthly Subscription", "pro"},
		{"intl", "TCACA_code_035_ArVxJcGDsm", "Free Plan Subscription", "免费"},
		{"intl", "TCACA_code_039_KRcQj7wUat", "Pro Plan Trial Subscription", "Pro试用"},
		{"cn", "future-cn-tier", "CodeBuddy商务版", "CodeBuddy商务版"},
		{"cn", "new-standard-code", "个人标准版", "标准版"},
		{"cn", "future-unnamed-tier", "", "未识别订阅"},
		{"cn", "TCACA_code_023_4xbGhMrE6q", "CodeBuddy个人青春版", "CodeBuddy个人青春版"},
	} {
		t.Run(tc.site+"/"+tc.code, func(t *testing.T) {
			got := resourcePlanName(tc.site, testPlanResource(tc.code, tc.name))
			if got != tc.want {
				t.Fatalf("site=%s got=%q want=%q", tc.site, got, tc.want)
			}
		})
	}
	free := testPlanResource("TCACA_code_008_cfWoLwvjU4", "CodeBuddy个人体验版")
	for code, label := range cnPlanNames {
		got, err := identifyPlan("cn", quotaSummaryData{}, []planResource{free, testPlanResource(code, "")}, time.Now())
		if err != nil || got != label {
			t.Fatalf("cn tier=%s got=%q err=%v want=%q", code, got, err, label)
		}
	}
	if got, err := identifyPlan("cn", quotaSummaryData{}, nil, time.Now()); err != nil || got != "体验版" {
		t.Fatalf("cn empty entitlements must not use intl Free label: %q %v", got, err)
	}
}

func TestPlanTimestampFormatsAndSanitization(t *testing.T) {
	expected := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for _, value := range []string{
		strconv.FormatInt(expected.UnixMilli(), 10),
		strconv.FormatInt(expected.Unix(), 10),
		`"2026-10-02T00:00:00Z"`, `"2026-10-02 08:00:00"`,
	} {
		got, present, err := planTimestamp(json.RawMessage(value))
		if err != nil || !present || !got.Equal(expected) {
			t.Fatalf("time=%s got=%v present=%v err=%v", value, got, present, err)
		}
	}
	for _, value := range []string{`null`, `""`, `0`, ""} {
		if _, present, err := planTimestamp(json.RawMessage(value)); present || err != nil {
			t.Fatalf("empty time %q should be absent: %v", value, err)
		}
	}
	if got := safePlanName("  New\nTier\x1b\u202e  "); got != "NewTier" {
		t.Fatalf("unsafe name: %q", got)
	}
}

func TestParsePlanPageRejectsSchemaDrift(t *testing.T) {
	for _, value := range []string{`{}`, `null`, `{"Response":{}}`,
		`{"Response":{"Data":{"TotalCount":0}}}`,
		`{"Response":{"Data":{"TotalCount":0,"Accounts":null}}}`,
		`{"Response":{"Data":{"TotalCount":-1,"Accounts":[]}}}`,
		`{"Response":{"Data":{"TotalCount":"bad","Accounts":[]}}}`,
		`{"Response":{"Data":{"Accounts":[]}}}`} {
		if _, _, err := parsePlanPage([]byte(value)); err == nil {
			t.Fatalf("schema drift was treated as free: %s", value)
		}
	}
	if rows, total, err := parsePlanPage([]byte(`{"Response":{"Data":{"TotalCount":"0","Accounts":[]}}}`)); err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("valid empty page: rows=%v total=%d err=%v", rows, total, err)
	}
	for _, id := range []string{`123456789`, `"123456789"`} {
		data := []byte(`{"Response":{"Data":{"TotalCount":1,"Accounts":[{"AccountId":` + id + `,"ResourceId":"res1","PackageCode":"new-code","Status":0}]}}}`)
		if rows, total, err := parsePlanPage(data); err != nil || total != 1 || len(rows) != 1 {
			t.Fatalf("numeric/string AccountId must both work: total=%d err=%v", total, err)
		}
	}
}

func TestFetchPlansUsesAllCodesAndPagination(t *testing.T) {
	oldClient := cfg.HttpClient
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	mode := "ok"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/billing/meter/get-user-resource" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, exists := body["PackageCodes"]; exists {
			t.Error("hardcoded package filter would hide future plans")
		}
		if r.Header.Get("X-Trace-ID") != "plan-test-trace" {
			t.Error("request trace not propagated")
		}
		page := int(body["PageNumber"].(float64))
		if mode == "http-error" {
			http.Error(w, "do not log this secret", 503)
			return
		}
		if mode == "schema-error" {
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		row := testPlanResource(fmt.Sprintf("future-%d", page), fmt.Sprintf("Future Tier %d", page))
		row.ResourceID = fmt.Sprintf("resource-%d", page)
		if mode == "duplicate" {
			row.ResourceID = "same-id"
		}
		var rows []planResource
		if mode != "empty-partial" || page == 1 {
			rows = []planResource{row}
		} else {
			rows = []planResource{}
		}
		total := 2
		if mode == "changing-total" && page == 2 {
			total = 3
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"Response": map[string]any{"Data": map[string]any{"Accounts": rows, "TotalCount": total}},
		}})
	}))
	defer server.Close()
	cfg.HttpClient = server.Client()
	prof := &upstreamProfile{Origin: server.URL}
	headers := func(r *http.Request) { r.Header.Set("X-Trace-ID", "plan-test-trace") }
	for _, testMode := range []string{"ok", "http-error", "schema-error", "duplicate", "empty-partial", "changing-total"} {
		t.Run(testMode, func(t *testing.T) {
			mode, calls = testMode, 0
			rows, err := fetchPlanResources(context.Background(), prof, headers, "plan-test-trace", "test.json", time.Now())
			if mode == "ok" {
				if err != nil || len(rows) != 2 || calls != 2 {
					t.Fatalf("rows=%v calls=%d err=%v", rows, calls, err)
				}
			} else if err == nil {
				t.Fatalf("partial/malformed response accepted: %s", mode)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls
	if _, err := fetchPlanResources(ctx, prof, headers, "plan-test-trace", "test.json", time.Now()); err == nil || calls != before {
		t.Fatal("canceled request must not contact upstream")
	}
}

func TestQuotaRefreshSeparatesQuotaAndPlanFailures(t *testing.T) {
	oldOrigin, oldClient := profileINTL.Origin, cfg.HttpClient
	t.Cleanup(func() { profileINTL.Origin, cfg.HttpClient = oldOrigin, oldClient })
	mode := "ok"
	var audit bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&audit)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	traces := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traces = append(traces, r.Header.Get("X-Trace-ID"))
		if r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("missing authorization")
		}
		if r.URL.Path == "/billing/meter/get-user-resource-summary" {
			if mode == "summary-fail" {
				http.Error(w, "failed", 502)
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[{"CycleTotalCapacity":"100","CycleUsedCapacity":"1","CycleRemainCapacity":"99"}],"IsPaidUser":false,"ProTrialStatus":1}}`))
			return
		}
		if mode != "ok" {
			http.Error(w, "secret upstream account text", 503)
			return
		}
		row := testPlanResource("TCACA_code_035_ArVxJcGDsm", "Free Plan Subscription")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"Response": map[string]any{
			"Data": map[string]any{"TotalCount": 1, "Accounts": []planResource{row}},
		}}})
	}))
	defer server.Close()
	profileINTL.Origin, cfg.HttpClient = server.URL, server.Client()
	acc := &Account{Path: "test.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "private-test-token"}}, PlanLabel: "Pro试用"}
	if err := refreshAccountQuota(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	if acc.PlanLabel != "免费" || acc.PlanCheckedAt == 0 || acc.PlanStale || acc.QuotaRemaining != 99 {
		t.Fatalf("wrong effective plan/quota: %+v", acc)
	}
	if len(traces) != 2 || traces[0] == "" || traces[0] != traces[1] {
		t.Fatalf("trace mismatch: %v", traces)
	}
	mode = "plan-fail"
	if err := refreshAccountQuota(context.Background(), acc); err != nil || acc.PlanLabel != "免费" || !acc.PlanStale || acc.QuotaRemaining != 99 {
		t.Fatalf("plan failure lost verified plan or quota: err=%v", err)
	}
	acc.PlanCheckedAt, acc.PlanLabel = 0, "Pro试用"
	if err := refreshAccountQuota(context.Background(), acc); err != nil || acc.PlanLabel != planUnknown || !acc.PlanStale {
		t.Fatal("legacy unverified plan must not survive a failed check")
	}
	mode = "summary-fail"
	if err := refreshAccountQuota(context.Background(), acc); err == nil || !acc.PlanStale {
		t.Fatal("summary failure must also mark plan stale")
	}
	for _, secret := range []string{"private-test-token", "secret upstream account text"} {
		if strings.Contains(audit.String(), secret) {
			t.Fatal("secret leaked into audit")
		}
	}
	for _, marker := range []string{"traceId=", "套餐查询", "套餐判断", "本轮套餐无法确认"} {
		if !strings.Contains(audit.String(), marker) {
			t.Fatalf("missing audit marker %s", marker)
		}
	}
}

func TestPlanTableShowsFutureNameAndStaleMarker(t *testing.T) {
	out := renderAccountTable([]accountSnapshot{{Path: "test.json", QuotaKnown: true, PlanLabel: "Business Ultra 2027", PlanStale: true}})
	if !strings.Contains(out, "Business Ultra 2027*") || !strings.Contains(out, "权益查询失败") {
		t.Fatalf("future plan/stale status hidden: %s", out)
	}
	out = renderAccountTable([]accountSnapshot{{Path: "test.json", QuotaKnown: true}})
	if !strings.Contains(out, planUnknown) {
		t.Fatal("missing plan must not default to free")
	}
}

func TestRestorePlanSnapshotTrustAndAge(t *testing.T) {
	chdirTemp(t)
	accountMu.Lock()
	oldAccounts := accounts
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	for _, tc := range []struct {
		name    string
		checked int64
		stale   bool
		label   string
	}{
		{"legacy_unverified", 0, true, planUnknown},
		{"recent_verified", time.Now().Unix(), false, "Business Ultra"},
		{"old_verified", time.Now().Add(-10 * time.Minute).Unix(), true, "Business Ultra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(statusSnapshot{Accounts: []accountSnapshot{{Path: "test.json", PlanLabel: "Business Ultra", PlanCheckedAt: tc.checked}}})
			if err := os.WriteFile(statusSnapshotFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			acc := &Account{Path: "test.json"}
			accountMu.Lock()
			accounts = []*Account{acc}
			restoreAccountRuntimeStateLocked()
			accountMu.Unlock()
			if acc.PlanLabel != tc.label || acc.PlanStale != tc.stale {
				t.Fatalf("label=%q stale=%v", acc.PlanLabel, acc.PlanStale)
			}
		})
	}
}
