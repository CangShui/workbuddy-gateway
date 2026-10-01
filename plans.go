package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const planUnknown = "待确认"

// 这些编码只用于旧套餐的短名称，不用于请求过滤。新编码仍会被查询，
// 并优先显示上游 PackageName，避免官网增加套餐后悄悄退回“免费”。
var legacyPlanNames = map[string]string{
	"TCACA_code_001_PqouKr6QWV": "免费",
	"TCACA_code_035_ArVxJcGDsm": "免费",
	"TCACA_code_008_cfWoLwvjU4": "免费",
	"TCACA_code_002_AkiJS3ZHF5": "pro",
	"TCACA_code_003_FAnt7lcmRT": "pro",
	"TCACA_code_039_KRcQj7wUat": "Pro试用",
	"TCACA_code_040_mi9rCYg46x": "Pro试用",
}

// 国内站同一 Pro 编码在官网展示为“标准版”，不能套用国际站名称。
// 此表仅规范已知四档的显示，不用于过滤查询结果；新编码仍显示官方名称。
var cnPlanNames = map[string]string{
	"TCACA_code_008_cfWoLwvjU4": "体验版",
	"TCACA_code_002_AkiJS3ZHF5": "标准版",
	"TCACA_code_026_BaESVICNoi": "高级版",
	"TCACA_code_027_0FCGVA6vSa": "旗舰版",
}

func knownPlanName(site, code string) string {
	if site == "cn" {
		return cnPlanNames[code]
	}
	return legacyPlanNames[code]
}

func freePlanName(site string) string {
	if site == "cn" {
		return "体验版"
	}
	return "免费"
}

var nonPlanCodes = map[string]bool{
	"TCACA_code_006_DbXS0lrypC": true,
	"TCACA_code_007_nzdH5h4Nl0": true,
	"TCACA_code_009_0XmEQc2xOf": true,
	"TCACA_code_036_lupO5WgNdG": true,
	"TCACA_code_037_WxOD3MpI2o": true,
}

type planResource struct {
	AccountID          json.RawMessage `json:"AccountId"` // 官网可返回数字或字符串。
	ResourceID         string          `json:"ResourceId"`
	PackageCode        string          `json:"PackageCode"`
	PackageName        string          `json:"PackageName"`
	SubProductCode     string          `json:"SubProductCode"`
	Status             json.RawMessage `json:"Status"`
	DeductionStartTime json.RawMessage `json:"DeductionStartTime"`
	DeductionEndTime   json.RawMessage `json:"DeductionEndTime"`
	ExpiredTime        json.RawMessage `json:"ExpiredTime"`
	CycleEndTime       json.RawMessage `json:"CycleEndTime"`
}

func planInteger(raw json.RawMessage) (int64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("数值字段缺失或格式发生变化")
	}
	return n, nil
}

func planTimestamp(raw json.RawMessage) (time.Time, bool, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" || s == "0" {
		return time.Time{}, false, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}, false, fmt.Errorf("权益时间无效")
		}
		if n > 1e12 {
			return time.UnixMilli(n), true, nil
		}
		return time.Unix(n, 0), true, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true, nil
	}
	// 用户中心无时区时间与官网展示使用 UTC+8，不依赖部署机时区。
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.FixedZone("UTC+8", 8*3600)); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("无法识别权益时间格式")
}

func activePlanResource(r planResource, now time.Time) (bool, error) {
	status, err := planInteger(r.Status)
	if err != nil {
		return false, err
	}
	switch status {
	case 1, 2: // 退款、过期；积分用尽(3)不等于套餐过期。
		return false, nil
	case 0, 3:
	default:
		return false, fmt.Errorf("未知权益状态 %d", status)
	}
	start, present, err := planTimestamp(r.DeductionStartTime)
	if err != nil {
		return false, err
	}
	if present && now.Before(start) {
		return false, nil
	}
	// 与官网相同的权益截止字段优先级；月度额度周期不是年付套餐到期。
	for _, raw := range []json.RawMessage{r.DeductionEndTime, r.ExpiredTime, r.CycleEndTime} {
		end, present, err := planTimestamp(raw)
		if err != nil {
			return false, err
		}
		if present {
			return now.Before(end), nil
		}
	}
	return true, nil // Status有效且无截止时间的长期权益。
}

func safePlanName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1 // 上游名称不得注入终端转义、换行或方向控制符。
		}
		return r
	}, name)
	runes := []rune(strings.TrimSpace(name))
	if len(runes) > 80 {
		runes = runes[:80]
	}
	return string(runes)
}

func resourcePlanName(site string, r planResource) string {
	if site == "cn" {
		if label := cnPlanNames[r.PackageCode]; label != "" {
			return label
		}
		label := safePlanName(r.PackageName)
		for _, tier := range []string{"体验版", "标准版", "高级版", "旗舰版"} {
			switch label {
			case tier, "个人" + tier, "CodeBuddy个人" + tier:
				return tier
			}
		}
		if label != "" {
			return label // 新套餐及历史青春版等不强行归入当前四档。
		}
		return "未识别订阅"
	}
	if label := safePlanName(r.PackageName); label != "" {
		switch strings.ToLower(label) {
		case "free plan subscription", "free plan":
			return "免费"
		case "pro plan monthly subscription", "pro plan annual subscription":
			return "pro"
		case "pro plan trial subscription":
			return "Pro试用"
		default:
			return label
		}
	}
	if label := legacyPlanNames[r.PackageCode]; label != "" {
		return label
	}
	return "未识别订阅"
}

func identifyPlan(site string, summary quotaSummaryData, resources []planResource, now time.Time) (string, error) {
	var plans []planResource
	ambiguous := false
	for _, r := range resources {
		if nonPlanCodes[r.PackageCode] || r.SubProductCode == "sp_tcaca_codebuddyide_bonus_pack" ||
			r.SubProductCode == "sp_tcaca_codebuddyide_creditplan" {
			continue
		}
		active, err := activePlanResource(r, now)
		if err != nil {
			return "", err
		}
		if !active {
			continue
		}
		if r.PackageCode == "" {
			return "", fmt.Errorf("有效权益缺少套餐编码")
		}
		// 新套餐沿用 IDE 子产品，或 summary 明确指定的当前订阅，都可识别。
		if r.SubProductCode == "sp_tcaca_codebuddy_ide" || knownPlanName(site, r.PackageCode) != "" ||
			r.PackageCode == summary.SubscriptionPackageCode {
			plans = append(plans, r)
		} else {
			ambiguous = true // 新子产品不能静默忽略后假称免费。
		}
	}
	// 官网试用已转付费(2)且试用仍有效时，仍展示试用，付费套餐待生效。
	trialStatus, _ := planInteger(json.RawMessage(fmt.Sprint(summary.ProTrialStatus)))
	if site == "intl" && trialStatus == 2 {
		for _, r := range plans {
			if legacyPlanNames[r.PackageCode] == "Pro试用" {
				return "Pro试用", nil
			}
		}
	}
	if summary.SubscriptionPackageCode != "" {
		for _, r := range plans {
			if r.PackageCode == summary.SubscriptionPackageCode {
				return resourcePlanName(site, r), nil
			}
		}
		return "", fmt.Errorf("订阅摘要与有效权益不一致")
	}
	// 没有摘要指定套餐时，保留所有有效身份包名称；不靠未知套餐的名字猜等级。
	labels := map[string]bool{}
	hasFree := false
	for _, r := range plans {
		label := resourcePlanName(site, r)
		if label == freePlanName(site) {
			hasFree = true
		} else {
			labels[label] = true
		}
	}
	if ambiguous {
		return "", fmt.Errorf("发现未知子产品，无法完整确认套餐")
	}
	if len(labels) > 0 {
		if site == "intl" && len(labels) > 1 {
			delete(labels, "Pro试用") // 已有有效正式订阅时普通试用不覆盖订阅。
		}
		names := make([]string, 0, len(labels))
		for name := range labels {
			names = append(names, name)
		}
		sort.Strings(names)
		return strings.Join(names, " / "), nil
	}
	if summary.IsPaidUser {
		return "", fmt.Errorf("付费标记存在但没有有效订阅权益")
	}
	if hasFree || len(plans) == 0 {
		// 已成功完整查询，只有过期包/赠送包/空列表，且没有付费订阅声明。
		return freePlanName(site), nil
	}
	return "", fmt.Errorf("套餐无法确认")
}

func parsePlanPage(data []byte) ([]planResource, int64, error) {
	var envelope struct {
		Response *struct {
			Data json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Response == nil {
		return nil, 0, fmt.Errorf("套餐接口缺少 Response")
	}
	var page struct {
		Accounts   json.RawMessage
		TotalCount json.RawMessage
	}
	if err := json.Unmarshal(envelope.Response.Data, &page); err != nil {
		return nil, 0, fmt.Errorf("套餐接口 Data 格式无效")
	}
	total, err := planInteger(page.TotalCount)
	if err != nil || total < 0 {
		return nil, 0, fmt.Errorf("套餐接口 TotalCount 缺失或无效")
	}
	if len(bytes.TrimSpace(page.Accounts)) == 0 || bytes.TrimSpace(page.Accounts)[0] != '[' {
		return nil, 0, fmt.Errorf("套餐接口 Accounts 不是数组")
	}
	var rows []planResource
	if err := json.Unmarshal(page.Accounts, &rows); err != nil {
		return nil, 0, fmt.Errorf("套餐接口 Accounts 格式无效")
	}
	return rows, total, nil
}

// fetchPlanResources 查询完整资源，不过滤 PackageCodes，不执行购买/续订/模型请求。
func fetchPlanResources(ctx context.Context, prof *upstreamProfile, headers func(*http.Request), traceID, account string, now time.Time) ([]planResource, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var resources []planResource
	lastTotal := int64(-1)
	seen := map[string]bool{}
	local := now.In(time.FixedZone("UTC+8", 8*3600))
	for page := 1; page <= 10; page++ {
		payload, _ := json.Marshal(map[string]any{
			"PageNumber": page, "PageSize": 200, "ProductCode": "p_tcaca",
			"Status": []int{0, 3}, "OnlyValidPeriod": true,
			"SlicePeriodStartTime": local.Format("2006-01-02") + " 00:00:00",
			"SlicePeriodEndTime":   local.Format("2006-01-02") + " 23:59:59",
		})
		log.Printf("[套餐查询] traceId=%s 账号=%s 阶段=只读资源请求 页=%d 结果=开始 说明=不限定套餐编码，不请求模型", traceID, account, page)
		data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost,
			prof.Origin+"/billing/meter/get-user-resource", headers, bytes.NewReader(payload))
		if err != nil {
			// 不把上游错误正文写入日志：可能包含用户标识或令牌。
			log.Printf("[套餐查询] traceId=%s 账号=%s 页=%d HTTP=%d 结果=失败 业务影响=本轮套餐无法确认", traceID, account, page, status)
			return nil, fmt.Errorf("套餐只读接口失败 HTTP=%d", status)
		}
		rows, total, err := parsePlanPage(data)
		if err != nil {
			return nil, err
		}
		if lastTotal >= 0 && total != lastTotal {
			return nil, fmt.Errorf("查询期间套餐总数变化，等待下一轮")
		}
		lastTotal = total
		for _, row := range rows {
			// 一个订阅资源可有多个计量账户，优先按 AccountId 去重。
			key := strings.Trim(strings.TrimSpace(string(row.AccountID)), `"`)
			if key == "" || key == "null" {
				key = ""
				if row.ResourceID != "" {
					key = "resource:" + row.ResourceID
				}
			} else {
				key = "account:" + key
			}
			if key != "" {
				if seen[key] {
					return nil, fmt.Errorf("套餐分页重复，拒绝使用不完整结果")
				}
				seen[key] = true
			}
		}
		resources = append(resources, rows...)
		log.Printf("[套餐查询] traceId=%s 账号=%s 页=%d HTTP=%d 本页=%d 累计=%d 总数=%d 结果=已解析", traceID, account, page, status, len(rows), len(resources), total)
		if int64(len(resources)) == total {
			return resources, nil
		}
		if len(rows) == 0 || int64(len(resources)) > total {
			return nil, fmt.Errorf("套餐分页不完整")
		}
	}
	return nil, fmt.Errorf("套餐分页超过安全上限，未使用部分结果")
}

func planDisplayLabel(label string, stale bool) string {
	if label == "" {
		return planUnknown
	}
	if stale {
		return label + "*"
	}
	return label
}

func markAccountPlanStale(acc *Account) {
	accountMu.Lock()
	defer accountMu.Unlock()
	if acc.PlanCheckedAt == 0 {
		acc.PlanLabel = planUnknown
	}
	acc.PlanStale = true
}
