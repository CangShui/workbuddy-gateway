package main

// CodeBuddy 成长任务（成长中心）领取能力。
//
// 移植自 jethub 插件（deepseek-harness 的 dsh-providers-hub）的
// src/buddy-growth.ts，只取「一键领取积分」这条生产链路实际用到的部分。
//
// ## 为什么只做国内站
//
// jethub 侧 contributor 在 !62 复审中实测确认：www.workbuddy.ai（国际站）
// **根本没有成长中心**（无任务列表、无 claim 端点）。插件因此只给 buddy
// （CodeBuddy 中国版）挂成长任务能力，workbuddy 刻意不提供。
// 本网关沿用同一判据：成长任务端点只对 profileCN（国内站）生效，
// 国际站账号一律跳过，绝不用国际版凭据去打国内站的成长接口。
//
// ## 与插件实现的两处刻意差异（域名口径）
//
// 插件把「成长中心 API」打在 product.endpoint（copilot.tencent.com），
// 把「逐任务领奖」打在 product.claimBase（www.workbuddy.cn）。
// 而本网关既有的签到端点拼的是 profile.Origin（www.codebuddy.cn）——
// 实测签到走的就是它。为不引入第二套未在本机验证过的域名口径，
// 这里统一沿用 p.Origin：既与签到同域，也让 httptest 打桩只需改一个变量。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// 成长任务端点路径。
const (
	growthTasksV2Path     = "/v2/activity/growth/tasks" // 成长族
	growthTasksLegacyPath = "/activity/growth/tasks"    // 养虾等级族
	growthTaskAcceptPath   = "/activity/growth/tasks/accept"
	growthTravelClaimPath  = "/activity/growth/buddy/travel/claim"
	growthTaskClaimPathFmt = "/activity/growth/tasks/%s/claim"
)

// growthTasksEnabled 是成长任务定时领取的运行时开关。
//
// ⚠️ 默认关闭：成长任务会向腾讯上游发真实写请求（接取任务 / 领奖），
// 属于会改变账号状态的动作，必须由用户显式开启，不要默认代跑。
var growthTasksEnabled atomic.Bool

// 成长写端点用 HTTP 400 表达多种语义，只有 msg 命中这三条窄白名单时才算
// 「没有可领对象」（正常幂等终态）；其余一律是真失败。
//
// ⚠️ 白名单必须窄：若放宽成「code==400 就算已领取」，那么「任务真没做完」
// （task not completed）也会被说成「已领到积分」，用户看到的与实际相反。
var growthNoObjectMarkers = []string{
	"no unclaimed travel",
	"insufficient energy",
	"accept buddy adoption agreement",
}

// 可自动接取的任务白名单（task_code）。
//
// 只接取纯使用即可达成、奖励为普通 credit/energy 的任务；限定款
// （需要下载桌面端/升级主题解锁的盲盒 Buddy）不进白名单，避免误触发
// 一次性解锁动作。与插件 AUTOMATABLE_TASK_CODES 对齐。
var growthWhitelist = map[string]bool{
	"create_canvas":               true,
	"template_5":                  true,
	"expert_5":                    true,
	"Expert_team_use_3":           true,
	"automation_1":                true,
	"playbook_prompt":             true,
	"Expert_lighthouse":           true,
	"Buddy_App":                   true,
	"Buddy_App_QQ":                true,
	"Hp_Appearance":               true,
	"chat_5":                      true,
	"Model_chat_GLM5.2":           true,
	"first_chat":                  true,
	"template_used":               true,
	"expert_summoned":             true,
	"skill_installed":             true,
	"wechat_linked":               true,
	"black_cat":                   true,
	"Library_read":                true,
	"RichMeow_Chat":               true,
	"skill_1":                     true,
	"wb_wechat_oa_subscribe_task": true,
}

// 这四项是「养虾等级」族：判据由服务端记录的 status 决定，API 事件上报
// 改变不了它（插件实测 2 轮 × 3 账号共 6 次，status 恒为 available）。
// 故直接标为「需客户端完成」，而不是去空跑一轮必然无效的真实会话。
var growthClientOnlyCodes = map[string]bool{
	"first_chat":                  true,
	"template_used":               true,
	"expert_summoned":             true,
	"skill_installed":             true,
	"wechat_linked":               true,
	"wb_wechat_oa_subscribe_task": true,
}

// 成长端点响应信封。code 为 0 表示业务成功。
type growthEnvelope struct {
	// ⚠️ 刻意用指针：必须区分「code 字段缺失」与「code 显式为 0」。
	// 用 int 时二者都是 0，于是网关错误体（如 {"msg":"Unauthorized"}）会被
	// 当成业务成功 —— 「已领取 +0 积分」这种谎报正是插件侧踩过的坑
	// （buddy-growth.ts:1991-1996）。判断成功一律走 growthCodeOf(env)。
	Code *int            `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// growthCodeOf 取业务码；字段缺失时返回 -1（视作失败，与插件 classifyGrowthWrite 同款）。
func growthCodeOf(env growthEnvelope) int {
	if env.Code == nil {
		return -1
	}
	return *env.Code
}

// 任务列表响应体。两族 schema 不同，故两个字段都收。
type growthTasksData struct {
	Tasks []growthTaskItem `json:"tasks"`
}

type growthTaskItem struct {
	TaskCode     string `json:"task_code"`     // 成长族
	Code         string `json:"code"`          // 养虾等级族
	AcceptStatus string `json:"accept_status"` // 成长族：not_accepted / in_progress / accepted / completed
	Status       string `json:"status"`        // 等级族：available 为未达成
	Progress     struct {
		Current float64 `json:"current"`
		Target  float64 `json:"target"`
	} `json:"progress"`
}

// 单个任务的领取结果。
type growthTaskResult struct {
	TaskCode   string `json:"taskCode"`
	OK         bool   `json:"ok"`
	ClientOnly bool   `json:"clientOnly,omitempty"`
	Message    string `json:"message"`
}

// 单个账号的成长任务执行结果。
type growthAccountResult struct {
	Path          string             `json:"path"` // 凭据文件（网关里没有 accountId，用路径标识）
	Nickname      string             `json:"nickname,omitempty"`
	OK            bool               `json:"ok"`
	Completed     int                `json:"completed"`
	ClientOnly    int                `json:"clientOnly"`
	Failed        int                `json:"failed"`
	ClaimedCount  int                `json:"claimedCount"`
	ClaimedCredit float64            `json:"claimedCredit"`
	TimedOut      bool               `json:"timedOut,omitempty"`
	Tasks         []growthTaskResult `json:"tasks"`
	Error         string             `json:"error,omitempty"`
}

// 成长任务端点基址。与既有签到端点同口径（p.Origin）。
func (p *upstreamProfile) growthBase() string {
	return strings.TrimRight(p.Origin, "/")
}

func (p *upstreamProfile) growthTasksURL(path string) string {
	return p.growthBase() + path
}

// growthTaskClaimURL 逐任务领奖端点：task_code 放在 URL 路径里。
func (p *upstreamProfile) growthTaskClaimURL(taskCode string) string {
	return p.growthBase() + fmt.Sprintf(growthTaskClaimPathFmt, url.PathEscape(taskCode))
}

// growthAuth 是一份已拷出的凭据快照，避免在网络 IO 期间持有账号锁。
type growthAuth struct {
	accessToken  string
	uid          string
	enterpriseID string
	domain       string
}

// growthRequest 发一个成长端点请求，返回解析后的信封。
//
// 与 doJSONContext 不同，这里需要「HTTP 200 但业务码非 0」时仍能读到
// code 与 msg（成长端点的幂等终态就藏在这里：HTTP 400 + code 400 + 特定
// msg 表示「没有可领对象」，那是正常终态而不是失败）。doJSONContext 会把
// 这种情况压成一个 error 字符串，故这里自己读原始响应体。
func growthRequest(ctx context.Context, method, fullURL string, auth growthAuth, prof *upstreamProfile, body io.Reader) (growthEnvelope, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return growthEnvelope{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth.accessToken)
	// 成长中心前端固定该头；签到端点不带，故不能复用 checkin 的 header 构造器。
	req.Header.Set("X-Client-Platform", "web")
	// X-Domain 以产品配置为准而非凭据快照，避免旧凭据把身份标识发错区域。
	if prof != nil && prof.Base != "" {
		if host := hostOf(prof.Base); host != "" {
			req.Header.Set("X-Domain", host)
		}
	} else if auth.domain != "" {
		req.Header.Set("X-Domain", auth.domain)
	}
	if auth.uid != "" {
		req.Header.Set("X-User-Id", auth.uid)
	}
	if auth.enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", auth.enterpriseID)
		req.Header.Set("X-Tenant-Id", auth.enterpriseID)
	}
	if prof != nil && prof.Product != "" {
		req.Header.Set("X-Product", prof.Product)
	}
	if prof != nil && prof.ClientUA != "" {
		req.Header.Set("User-Agent", prof.ClientUA)
	}

	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return growthEnvelope{}, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return growthEnvelope{}, resp.StatusCode, err
	}
	var env growthEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// 非 JSON：多半是网关 HTML 错误页（凭据失效的典型表现）。
		return growthEnvelope{}, resp.StatusCode, fmt.Errorf("%s", describeGrowthNonJSON(resp.StatusCode, string(raw)))
	}
	return env, resp.StatusCode, nil
}

// hostOf 取 URL 的主机部分（去掉 scheme 与路径）。
func hostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// describeGrowthNonJSON 描述非 JSON 响应。
//
// ⚠️ 「凭据已失效」这六个字是下游判僵尸账号的唯一判据
// （growthAccountResult 的 expired 判定按它匹配），必须逐字保留。
func describeGrowthNonJSON(status int, text string) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Sprintf("凭据已失效（HTTP %d），请重新登录该账号", status)
	}
	snippet := strings.Join(strings.Fields(text), " ")
	if len(snippet) > 80 {
		snippet = snippet[:80]
	}
	return fmt.Sprintf("服务端返回了非 JSON 响应（HTTP %d）：%s", status, snippet)
}

// isGrowthNoObject 判断 code 400 是否属于「没有可领对象」的幂等终态。
func isGrowthNoObject(env growthEnvelope) bool {
	if growthCodeOf(env) != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(env.Msg)
	for _, marker := range growthNoObjectMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// 任务状态归一：取两族各自的码字段。
func (t growthTaskItem) code() string {
	if t.TaskCode != "" {
		return t.TaskCode
	}
	return t.Code
}

// isPending 判断该任务是否还待完成。
//
// 两族判据不同：
//   - 成长族：accept_status ∈ {not_accepted, in_progress, accepted}
//   - 等级族：status == "available"
func (t growthTaskItem) isPending() bool {
	if t.AcceptStatus != "" {
		switch t.AcceptStatus {
		case "not_accepted", "in_progress", "accepted":
			return true
		}
		return false
	}
	return t.Status == "available"
}

// needsAccept 判断是否还需要接取（成长族 not_accepted 需要先 accept）。
func (t growthTaskItem) needsAccept() bool { return t.AcceptStatus == "not_accepted" }

// growthTaskQuery 是任务列表查询的归一结果。
type growthTaskQuery struct {
	ok       bool
	codes    []string
	total    int
	toAccept map[string]bool
	message  string
}

// fetchGrowthTaskCodes 查询两个族的任务列表并合并。
//
// ⚠️ 必须合并两个端点，不能命中第一个就返回：两族任务 schema 完全不同，
// 只打 /v2/... 会让等级族永远进不了待办。单族失败不中断，只记首个原因；
// 两族全失败才返回 ok=false。
func fetchGrowthTaskCodes(ctx context.Context, auth growthAuth, prof *upstreamProfile) growthTaskQuery {
	result := growthTaskQuery{toAccept: map[string]bool{}}
	seen := map[string]bool{}
	var failure string
	anyOK := false

	for _, path := range []string{growthTasksV2Path, growthTasksLegacyPath} {
		env, status, err := growthRequest(ctx, http.MethodGet, prof.growthTasksURL(path), auth, prof, nil)
		if err != nil {
			if failure == "" {
				failure = err.Error()
			}
			continue
		}
		if status >= 400 && growthCodeOf(env) == 0 {
			if failure == "" {
				failure = fmt.Sprintf("任务列表返回 HTTP %d", status)
			}
			continue
		}
		if growthCodeOf(env) != 0 {
			if failure == "" {
				failure = fmt.Sprintf("任务列表返回 code %d", growthCodeOf(env))
			}
			continue
		}
		anyOK = true
		var data growthTasksData
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		result.total += len(data.Tasks)
		for _, task := range data.Tasks {
			code := task.code()
			if !growthWhitelist[code] {
				continue
			}
			if !task.isPending() {
				continue
			}
			if !seen[code] {
				seen[code] = true
				result.codes = append(result.codes, code)
			}
			if task.needsAccept() {
				result.toAccept[code] = true
			}
		}
	}
	if !anyOK {
		if failure == "" {
			failure = "任务列表查询失败"
		}
		return growthTaskQuery{ok: false, message: failure, toAccept: map[string]bool{}}
	}
	result.ok = true
	return result
}

// growthClaimOutcome 是逐任务领奖的结果。
type growthClaimOutcome struct {
	ok      bool
	claimed bool
	credit  float64
	message string
}

// claimGrowthTaskReward 领取单个任务的奖励。
//
// ⚠️ 成功判据必须是显式 code==0：不能写成「code 存在且非 0 才失败」，
// 那样「响应里根本没有 code」会被当成成功，于是网关错误体
// （如 {msg:"Unauthorized"}）被谎报成「已领取 +0 积分」。
func claimGrowthTaskReward(ctx context.Context, auth growthAuth, prof *upstreamProfile, taskCode string) growthClaimOutcome {
	env, status, err := growthRequest(ctx, http.MethodPost, prof.growthTaskClaimURL(taskCode), auth, prof, strings.NewReader("{}"))
	if err != nil {
		return growthClaimOutcome{message: err.Error()}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return growthClaimOutcome{message: fmt.Sprintf("凭据已失效（HTTP %d）", status)}
	}
	if growthCodeOf(env) != 0 {
		msg := env.Msg
		if msg == "" {
			msg = "无错误信息"
		}
		return growthClaimOutcome{message: fmt.Sprintf("领取 %s 失败：%s", taskCode, msg)}
	}
	var data struct {
		AlreadyClaimed bool    `json:"already_claimed"`
		Credit         float64 `json:"credit"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}
	// 重复领取是幂等终态，不算失败，也不重复计数。
	if data.AlreadyClaimed {
		return growthClaimOutcome{ok: true, claimed: false}
	}
	return growthClaimOutcome{ok: true, claimed: true, credit: data.Credit}
}

// growthRunResult 是单账号一轮执行的结果。
type growthRunResult struct {
	ok            bool
	completed     int
	clientOnly    int
	failed        int
	claimedCount  int
	claimedCredit float64
	timedOut      bool
	results       []growthTaskResult
	message       string
}

// runGrowthTasks 执行单账号的成长任务：接取 → 推进 → 扫尾补领。
//
// 只做低副作用的核心动作（接取 + 旅行领奖 + 扫尾补领）；插件侧那些
// 「消耗 energy / 一次性解锁」的盲盒、抽奖、首 Buddy 默认关闭，这里不移植。
//
// 返回值里 failed==0 才算 ok。预算耗尽标 timedOut（不是失败），
// 提示用户再点一次即可继续。
func runGrowthTasks(ctx context.Context, auth growthAuth, prof *upstreamProfile, deadline time.Time) growthRunResult {
	query := fetchGrowthTaskCodes(ctx, auth, prof)
	if !query.ok {
		reason := query.message
		// ⚠️ 失败原因必须进 results，不能只放在 message 里：
		// 下游靠逐项 message 判「凭据已失效」，空 results 会让僵尸账号
		// 被说成「没有任务」，用户完全无从排查。
		return growthRunResult{
			results: []growthTaskResult{{TaskCode: "", Message: reason}},
			message: reason,
		}
	}

	// 接取仍为 not_accepted 的任务（in_progress 的跳过，幂等）。
	if len(query.toAccept) > 0 {
		codes := make([]string, 0, len(query.toAccept))
		for code := range query.toAccept {
			codes = append(codes, code)
		}
		body, err := json.Marshal(map[string]any{"task_codes": codes})
		if err == nil {
			env, _, reqErr := growthRequest(ctx, http.MethodPost, prof.growthTasksURL(growthTaskAcceptPath), auth, prof, strings.NewReader(string(body)))
			if reqErr != nil || growthCodeOf(env) != 0 {
				// 接取失败只影响本轮新接的任务；已达标未领取的奖励与接取无关，
				// 故不在此 return —— 扫尾补领仍要照常跑。
				log.Printf("[Growth] 接取任务失败，codes=%d 原因=%v", len(codes), reqErr)
			}
		}
	}

	var out growthRunResult
	for _, code := range query.codes {
		if !deadline.IsZero() && time.Now().After(deadline) {
			out.timedOut = true
			out.failed++
			// ⚠️ 文案必须含「未执行」：扫尾阶段用同一个词，回归用例按它判定。
			out.results = append(out.results, growthTaskResult{
				TaskCode: code, Message: "本轮时间预算已用尽，未执行（再点一次即可继续）",
			})
			continue
		}
		// ⚠️ 本网关不模拟用户行为：任务的「完成动作」要么靠上报判据事件、
		// 要么靠真实会话推进，两者都属客户端动作。等级族（判据是服务端
		// 记录的 status，事件上报改变不了它）与客户端专属任务均如实
		// 标为「需客户端完成」，不空跑一轮必然无效的真实请求。
		// 已达标但未领取的奖励由下方扫尾补领兜住。
		out.clientOnly++
		out.results = append(out.results, growthTaskResult{
			TaskCode: code, OK: true, ClientOnly: true,
			Message: "需在客户端完成（API 无法推进此任务进度）",
		})
	}

	// 扫尾补领：已达标但未领取奖励的任务也要逐个领。
	//
	// ⚠️ 扫尾独立于上面的完成动作，且受同一 deadline 约束。
	sweep := fetchGrowthTaskCodes(ctx, auth, prof)
	if sweep.ok {
		for _, code := range sweep.codes {
			if !deadline.IsZero() && time.Now().After(deadline) {
				out.timedOut = true
				out.results = append(out.results, growthTaskResult{
					TaskCode: code,
					Message:  "扫尾补领触及时间预算，本轮未执行；再点一次即可继续",
				})
				break
			}
			claim := claimGrowthTaskReward(ctx, auth, prof, code)
			if claim.ok && claim.claimed {
				out.claimedCount++
				out.claimedCredit += claim.credit
			}
		}
	}

	out.ok = out.failed == 0
	if len(out.results) > 0 {
		out.message = fmt.Sprintf("需客户端 %d 项，失败 %d 项，自动领取 %d 项（+%.0f 积分）",
			out.clientOnly, out.failed, out.claimedCount, out.claimedCredit)
		if out.timedOut {
			out.message += "；本轮时间预算用尽，剩余项未执行，再点一次即可继续"
		}
	}
	return out
}

// runGrowthForAccount 对单个账号跑一轮成长任务，返回可直接展示的结果。
//
// 国际站与企业版账号一律跳过（不发起任何上游请求）。
func runGrowthForAccount(ctx context.Context, acc *Account, budget time.Duration) growthAccountResult {
	if acc == nil {
		return growthAccountResult{OK: true, Error: "账号为空"}
	}
	acc.lock.Lock()
	defer acc.lock.Unlock()

	accountMu.Lock()
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
		accountMu.Unlock()
		return growthAccountResult{Path: acc.Path, Nickname: acc.Nickname, OK: true, Error: "账号不可用"}
	}
	prof := acc.Profile()
	if prof.Key != profileCN.Key {
		// ⚠️ 国际站后端没有成长中心，必须跳过而不是照发国内站请求。
		accountMu.Unlock()
		return growthAccountResult{Path: acc.Path, Nickname: acc.Nickname, OK: true, Error: "国际站不支持成长任务（其后端没有成长中心）"}
	}
	if strings.TrimSpace(acc.Auth.Account.EnterpriseID) != "" {
		accountMu.Unlock()
		return growthAccountResult{Path: acc.Path, Nickname: acc.Nickname, OK: true, Error: "企业版不参与个人成长任务"}
	}
	auth := growthAuth{
		accessToken:  acc.Auth.Auth.AccessToken,
		uid:          acc.Auth.Account.UID,
		enterpriseID: acc.Auth.Account.EnterpriseID,
		domain:       acc.Auth.Auth.Domain,
	}
	nickname := acc.Auth.Account.Nickname
	if nickname == "" {
		nickname = acc.Nickname
	}
	path := acc.Path
	accountMu.Unlock()

	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	deadline := time.Time{}
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}

	report := runGrowthTasks(ctx, auth, prof, deadline)
	// 凭据失效的判据：逐项 message 里出现「凭据已失效」。
	// 它与「任务推不动」是两回事 —— 前者要重新登录，故不计入 failed。
	expired := false
	for _, item := range report.results {
		if strings.Contains(item.Message, "凭据已失效") {
			expired = true
			break
		}
	}
	result := growthAccountResult{
		Path:          path,
		Nickname:      nickname,
		OK:            report.ok || expired,
		Completed:     report.completed,
		ClientOnly:    report.clientOnly,
		Failed:        report.failed,
		ClaimedCount:  report.claimedCount,
		ClaimedCredit: report.claimedCredit,
		TimedOut:      report.timedOut,
		Tasks:         report.results,
	}
	if expired {
		result.Failed = 0
		result.Error = "凭据已失效，请重新登录该账号"
	}
	return result
}

// growthTaskBudget 默认单账号时间预算。
const growthTaskBudget = 60 * time.Second

// ErrGrowthDisabled 表示成长任务功能未启用。
var ErrGrowthDisabled = errors.New("成长任务未启用")

// runAllGrowthTasks 对所有账号跑一轮成长任务（后台定时任务与管理台共用）。
func runAllGrowthTasks(ctx context.Context) ([]growthAccountResult, error) {
	if !growthTasksEnabled.Load() {
		return nil, ErrGrowthDisabled
	}
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()

	results := make([]growthAccountResult, 0, len(accs))
	for _, acc := range accs {
		if err := ctx.Err(); err != nil {
			break
		}
		results = append(results, runGrowthForAccount(ctx, acc, growthTaskBudget))
	}
	return results, nil
}

// growthTaskInterval 后台定时执行的间隔。
const growthTaskInterval = 6 * time.Hour

// backgroundGrowthTasks 定时跑成长任务。默认关闭，由配置 growth.enabled 开启。
//
// 与 backgroundDailyCheckin 同款结构：启动即跑一轮 + 打计划日志 + timer 循环。
func backgroundGrowthTasks() {
	for {
		next := time.Now().Add(growthTaskInterval)
		if !growthTasksEnabled.Load() {
			logScheduledTask("growth_tasks", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "成长任务未开启；配置 growth.enabled=true 后启用")
		} else {
			go runGrowthRound()
		}
		logScheduledTask("growth_tasks", "scheduled", "", newTraceID(), time.Time{}, next, "每6小时；可在管理台手动触发")
		timer := time.NewTimer(time.Until(next))
		<-timer.C
	}
}

// runGrowthRound 跑一轮并写定时任务日志（供管理台「定时任务」页读取）。
func runGrowthRound() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	next := time.Now().Add(growthTaskInterval)
	trace, started := startScheduledTask("growth_tasks", next)

	results, err := runAllGrowthTasks(ctx)
	if err != nil {
		log.Printf("[Growth] 本轮未执行：%v", err)
		logScheduledTask("growth_tasks", "completed", "skipped", trace, started, next, err.Error())
		return
	}
	claimed, credits, failed := 0, 0.0, 0
	for _, r := range results {
		claimed += r.ClaimedCount
		credits += r.ClaimedCredit
		failed += r.Failed
	}
	status := "success"
	switch {
	case failed > 0 && claimed == 0:
		status = "failed"
	case failed > 0:
		status = "partial"
	case claimed == 0:
		status = "skipped"
	}
	log.Printf("[Growth] 本轮成长任务完成，账号=%d 自动领取=%d 积分=%.0f 失败=%d", len(results), claimed, credits, failed)
	logScheduledTask("growth_tasks", "completed", status, trace, started, next,
		fmt.Sprintf("账号=%d 自动领取=%d 积分=%.0f 失败=%d", len(results), claimed, credits, failed))
	requestQuotaScan()
}
