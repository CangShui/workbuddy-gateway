package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 上游报错里可能带用户标识或令牌片段，写日志前统一隐去。
var (
	uuidLikeRe   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	bearerLikeRe = regexp.MustCompile(`(?i)(bearer\s+)?eyJ[A-Za-z0-9_\-]{10,}(\.[A-Za-z0-9_\-]+){1,2}`)
)

// 主动续期（保活）参数。参考 ckldy/workbuddy2api 的 keepalive_hours 调度方式：
// 在固定本地时刻主动刷新全部账号，不等访问令牌临近过期。
const (
	// keepaliveAccountGap 账号之间的串行间隔。批量并发刷新登录态是明显的机器特征，
	// 串行加节流更接近真人逐个使用的节奏。
	keepaliveAccountGap = 1 * time.Second
	// keepaliveCatchUpDelay 启动补跑延迟：先让服务稳定，再补做一次漏掉的续期。
	keepaliveCatchUpDelay = 90 * time.Second
	// keepaliveOverdueAfter 距上次成功续期超过该时长即视为需要补跑。
	keepaliveOverdueAfter = 20 * time.Hour
	// refreshDisableThreshold 连续刷新失败多少次才判定登录态失效。
	// 单次失败可能是上游抖动、风控或网络问题，一次就删凭据属于误杀。
	refreshDisableThreshold = 3
)

// refreshFailureKind 区分「登录态真的没了」和「这次只是没成功」。
type refreshFailureKind int

const (
	refreshFailureTransient refreshFailureKind = iota
	refreshFailureSessionDead
)

func (k refreshFailureKind) String() string {
	if k == refreshFailureSessionDead {
		return "登录态失效"
	}
	return "临时失败"
}

// sessionDeadMarkers 是上游明确表示「刷新令牌已不可用」的标志。
// 只有命中这些标志才按登录态失效处理；普通 401/403 可能是权限或风控，
// 需要连续多次失败才判定，避免一次抖动就删掉用户凭据。
var sessionDeadMarkers = []string{
	"12153",
	"offline user session not found",
	"invalid_grant",
	"token is not active",
	"refresh token failed",
	"session not found",
}

func classifyRefreshFailure(status int, body string) refreshFailureKind {
	low := strings.ToLower(body)
	for _, marker := range sessionDeadMarkers {
		if strings.Contains(low, marker) {
			return refreshFailureSessionDead
		}
	}
	return refreshFailureTransient
}

// hourList 解析 "-keepalive-hours 22" 或 "-keepalive-hours 10,22" 形式的本地小时列表。
// 空值表示关闭主动续期。
type hourList []int

func (h *hourList) String() string {
	if h == nil || len(*h) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*h))
	for _, v := range *h {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
}

func (h *hourList) Set(value string) error {
	var out []int
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		n, err := strconv.Atoi(item)
		if err != nil {
			return fmt.Errorf("续期时刻 %q 不是 0-23 的整数", item)
		}
		if n < 0 || n > 23 {
			return fmt.Errorf("续期时刻 %d 超出 0-23 范围", n)
		}
		out = append(out, n)
	}
	sort.Ints(out)
	*h = out
	return nil
}

// nextKeepaliveTime 返回 now 之后最近的一个配置时刻（本地时区）。
func nextKeepaliveTime(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

func formatKeepaliveHours(hours []int) string {
	parts := make([]string, 0, len(hours))
	for _, h := range hours {
		parts = append(parts, fmt.Sprintf("%02d:00", h))
	}
	return strings.Join(parts, "、")
}

// jwtExpiry 从 JWT 载荷读取 exp，仅用于展示与到期预警，不做签名校验。
// 手工导入的凭据也能借此看到刷新令牌的到期时间。
func jwtExpiry(token string) (time.Time, bool) {
	claims := jwtClaims(token)
	if claims == nil {
		return time.Time{}, false
	}
	switch v := claims["exp"].(type) {
	case float64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(v), 0), true
	case json.Number:
		n, err := v.Int64()
		if err != nil || n <= 0 {
			return time.Time{}, false
		}
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

// refreshTokenExpiry 返回刷新令牌到期时间：优先用续期时记录的字段，
// 其次从未验签的令牌声明读取，都拿不到则未知。
func refreshTokenExpiry(tokens StoredTokens) (time.Time, string, bool) {
	if tokens.RefreshExpiresAt > 0 {
		return time.Unix(tokens.RefreshExpiresAt, 0), "续期接口返回", true
	}
	if exp, ok := jwtExpiry(tokens.RefreshToken); ok {
		return exp, "令牌声明", true
	}
	return time.Time{}, "", false
}

// isKeepaliveOverdue 判断账号是否久未成功续期，需要在启动时补跑一次。
func isKeepaliveOverdue(acc *Account, now time.Time) bool {
	if acc == nil || acc.Auth == nil {
		return false
	}
	last := acc.Auth.Auth.LastRefreshTime
	if last <= 0 {
		return true
	}
	return now.Sub(time.Unix(last, 0)) >= keepaliveOverdueAfter
}

// keepaliveLoop 主动续期调度：到点刷新全部账号，另在启动时补做漏掉的一轮。
// 只走令牌刷新接口，不请求模型、不消耗额度。
func keepaliveLoop() {
	if len(cfg.KeepaliveHours) == 0 {
		log.Printf("[续期保活] 阶段=启动 结果=已关闭 原因=未配置主动续期时刻 业务影响=仅在访问令牌临近过期时才刷新，长期空闲账号可能静默失效")
		return
	}
	log.Printf("[续期保活] 阶段=启动 结果=已启用 续期时刻=%s 说明=到点主动刷新全部账号登录凭据，只调刷新接口，不请求模型、不消耗额度",
		formatKeepaliveHours(cfg.KeepaliveHours))

	go func() {
		time.Sleep(keepaliveCatchUpDelay)
		now := time.Now()
		accountMu.Lock()
		overdue := 0
		for _, acc := range accounts {
			if acc.Disabled || acc.Auth == nil {
				continue
			}
			if isKeepaliveOverdue(acc, now) {
				overdue++
			}
		}
		accountMu.Unlock()
		if overdue == 0 {
			log.Printf("[续期保活] 阶段=启动补跑 结果=跳过 原因=全部账号均在 %v 内成功续期过", keepaliveOverdueAfter)
			return
		}
		log.Printf("[续期保活] 阶段=启动补跑 待续期账号=%d 原因=距上次成功续期超过 %v 或从未续期 说明=服务已稳定，开始补做一轮", overdue, keepaliveOverdueAfter)
		runKeepaliveOnce("启动补跑", true)
	}()

	for {
		next := nextKeepaliveTime(time.Now(), cfg.KeepaliveHours)
		log.Printf("[续期保活] 阶段=等待下一次续期 时间=%s 距离=%s", next.Format("2006-01-02 15:04:05"), time.Until(next).Truncate(time.Second))
		timer := time.NewTimer(time.Until(next))
		<-timer.C
		runKeepaliveOnce("定时", false)
	}
}

// runKeepaliveOnce 串行刷新账号池；onlyOverdue 为真时只处理久未续期的账号。
func runKeepaliveOnce(trigger string, onlyOverdue bool) {
	accountMu.Lock()
	accs := append([]*Account(nil), accounts...)
	accountMu.Unlock()

	now := time.Now()
	log.Printf("[续期保活] 阶段=执行 触发方式=%s 账号总数=%d 只补跑久未续期=%t 结果=开始", trigger, len(accs), onlyOverdue)

	refreshed, skipped, failed, disabled := 0, 0, 0, 0
	first := true
	for _, acc := range accs {
		accountMu.Lock()
		disabledAccount := acc.Disabled
		hasAuth := acc.Auth != nil
		path := acc.Path
		hasRefresh := hasAuth && acc.Auth.Auth.RefreshToken != ""
		overdue := hasAuth && isKeepaliveOverdue(acc, now)
		accountMu.Unlock()

		if disabledAccount {
			skipped++
			log.Printf("[续期保活] 账号=%s 结果=跳过 原因=账号已标记失效，不参与调度", path)
			continue
		}
		if !hasAuth || !hasRefresh {
			skipped++
			log.Printf("[续期保活] 账号=%s 结果=跳过 原因=缺少刷新令牌，无法续期，需要重新登录", path)
			continue
		}
		if onlyOverdue && !overdue {
			skipped++
			continue
		}
		if !first {
			time.Sleep(keepaliveAccountGap) // 串行节流，避免批量并发刷新形成机器特征
		}
		first = false

		if err := refreshAccountToken(acc, "主动续期/"+trigger); err != nil {
			failed++
			accountMu.Lock()
			isDisabled := acc.Disabled
			accountMu.Unlock()
			if isDisabled {
				disabled++
			}
			continue
		}
		refreshed++
	}

	log.Printf("[续期保活] 阶段=执行完成 触发方式=%s 成功=%d 失败=%d 跳过=%d 判定失效=%d 业务影响=成功的账号登录态已顺延，失败的账号保留原凭据等待下次重试",
		trigger, refreshed, failed, skipped, disabled)
	if refreshed > 0 || disabled > 0 {
		writeStatusSnapshot()
	}
}

// refreshFailureSummary 生成不含令牌与上游用户标识的失败摘要，用于日志与状态展示。
func refreshFailureSummary(kind refreshFailureKind, status int, err error) string {
	detail := ""
	if err != nil {
		detail = sanitizeUpstreamText(err.Error())
	}
	if len(detail) > 160 {
		detail = detail[:160] + "..."
	}
	return fmt.Sprintf("%s（HTTP %d）%s", kind, status, detail)
}

// sanitizeUpstreamText 去掉上游报错里可能出现的账号标识与令牌片段。
func sanitizeUpstreamText(text string) string {
	text = uuidLikeRe.ReplaceAllString(text, "[已隐去]")
	text = bearerLikeRe.ReplaceAllString(text, "[已隐去]")
	text = strings.ReplaceAll(text, "\n", " ")
	return strings.TrimSpace(text)
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "已过期"
	}
	if days := int(d.Hours()) / 24; days > 0 {
		return fmt.Sprintf("%d 天", days)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

// formatRenewalStatus 生成 status 命令里的登录续期信息。
// 调用方需持有 accountMu（本函数内部不再加锁）。
func formatRenewalStatus(acc *Account) string {
	if acc == nil || acc.Auth == nil {
		return ""
	}
	tokens := acc.Auth.Auth
	var sb strings.Builder
	if exp, source, ok := refreshTokenExpiry(tokens); ok {
		remain := time.Until(exp)
		state := "有效"
		switch {
		case remain <= 0:
			state = "已过期，需要重新登录"
		case remain < 7*24*time.Hour:
			state = "即将到期，建议尽快重新登录"
		}
		sb.WriteString(fmt.Sprintf("刷新令牌:     至 %s（%s，剩余 %s，%s）\n",
			exp.Local().Format("2006-01-02 15:04:05"), source, humanDuration(remain), state))
	} else {
		sb.WriteString("刷新令牌:     到期时间未知（续期接口未返回，令牌也无法解析）\n")
	}
	if tokens.LastRefreshTime > 0 {
		sb.WriteString(fmt.Sprintf("最近续期:     %s\n", time.Unix(tokens.LastRefreshTime, 0).Format("2006-01-02 15:04:05")))
	} else {
		sb.WriteString("最近续期:     尚无记录（主动续期成功后写入）\n")
	}
	if acc.RefreshFailCount > 0 {
		sb.WriteString(fmt.Sprintf("续期失败:     连续 %d 次，最近原因=%s\n", acc.RefreshFailCount, acc.LastRefreshError))
	} else if acc.LastRefreshError != "" {
		sb.WriteString(fmt.Sprintf("续期告警:     %s\n", acc.LastRefreshError))
	}
	return sb.String()
}
