package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// 凭据校验：在「覆盖」和「删除」这两个不可逆动作之前，先证明新凭据可用、
// 且要删除的凭据确实已经不能用。目的是避免把还能用的凭据销毁掉。
const (
	// credentialVerifyTimeout 只读校验的超时；校验失败不会阻塞太久。
	credentialVerifyTimeout = 8 * time.Second
)

// jwtClaims 解码 JWT 载荷（不做签名校验），仅用于本地一致性比对与到期展示。
func jwtClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	return claims
}

// shortID 生成不可逆短标识，用于日志里指代账号而不暴露原始标识。
func shortID(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// credentialIdentity 取出访问令牌声明的账号标识与站点 realm，用于比对新旧凭据是否同一账号。
func credentialIdentity(tokens StoredTokens) (accountID, realm string) {
	claims := jwtClaims(tokens.AccessToken)
	if claims == nil {
		return "", ""
	}
	if sub, ok := claims["sub"].(string); ok {
		accountID = sub
	}
	iss, _ := claims["iss"].(string)
	if iss == "" {
		return accountID, ""
	}
	if i := strings.Index(iss, "/auth/realms/"); i >= 0 {
		return accountID, iss[i+len("/auth/realms/"):]
	}
	return accountID, iss
}

// verifyAccountUsable 用只读接口确认凭据是否还能取到账号数据。
//
// 返回 (true, nil)  凭据可用；
// 返回 (false, nil) 上游明确拒绝（401 或正文明确登录失效），凭据已不可用；
// 返回 (false, err) 无法判定（普通 403、网络、超时、上游 5xx），调用方不得据此销毁凭据。
func verifyAccountUsable(ctx context.Context, auth StoredAuth) (bool, error) {
	registerCredentialSecrets(&auth)
	traceID := newTraceID()
	if state, ok := ctx.Value(debugRequestContextKey{}).(*debugRequestContext); ok {
		state.mu.RLock()
		traceID = state.traceID
		state.mu.RUnlock()
	}
	log.Printf("[凭据校验] traceId=%s 阶段=只读校验开始 站点=%s 说明=确认账号接口是否可用，不写入凭据", traceID, auth.Edition)
	if strings.TrimSpace(auth.Auth.AccessToken) == "" {
		log.Printf("[凭据校验] traceId=%s 结果=无法判定 原因=缺少访问令牌 说明=未调用上游", traceID)
		return false, fmt.Errorf("缺少访问令牌，无法校验")
	}
	if cfg.HttpClient == nil {
		log.Printf("[凭据校验] traceId=%s 结果=无法判定 原因=HTTP客户端未就绪 说明=未调用上游", traceID)
		return false, fmt.Errorf("HTTP 客户端未就绪，无法校验")
	}
	prof := profileForEdition(auth.Edition)
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if auth.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
		}
	}
	_, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, prof.quotaSummaryURL(), headers, strings.NewReader("{}"))
	if err != nil {
		// 普通 403 可能只是权限不足或风控拒绝，不能据此确认 Token 失效。
		// 仅修正只读凭据校验，不改变其他调用方的 403 停用规则。
		if status == http.StatusUnauthorized || isAuthFailure(0, err.Error()) {
			log.Printf("[凭据校验] traceId=%s HTTP=%d 结果=不可用 原因=%v 说明=上游明确返回授权失效", traceID, status, err)
			return false, nil // 上游明确拒绝：凭据确实不可用
		}
		log.Printf("[凭据校验] traceId=%s HTTP=%d 结果=无法判定 原因=%v 业务影响=不得仅凭此次结果销毁凭据", traceID, status, err)
		return false, fmt.Errorf("只读校验未完成 HTTP=%d", status)
	}
	log.Printf("[凭据校验] traceId=%s HTTP=%d 结果=可用 说明=已成功取到账号数据", traceID, status)
	return true, nil
}

// validateRefreshedCredential 在把新凭据写回磁盘之前做两道校验：
//  1. 新旧凭据必须属于同一账号、同一站点，防止上游串号或响应错配导致覆盖错人；
//  2. 新访问令牌必须能取到账号数据，防止用「接口 200 但实际不可用」的凭据覆盖掉还能用的旧凭据。
//
// 校验接口本身不可用时按通过处理，避免因为校验抖动而拒绝掉有效的新凭据。
func validateRefreshedCredential(path string, refreshed *StoredAuth, oldTokens StoredTokens) error {
	oldID, oldRealm := credentialIdentity(oldTokens)
	newID, newRealm := credentialIdentity(refreshed.Auth)
	identityNote := "凭据未携带可解析的账号声明，跳过一致性比对"
	if newID != "" {
		identityNote = fmt.Sprintf("账号标识=%s 站点=%s 与旧凭据一致", shortID(newID), newRealm)
	}
	if oldID != "" && newID != "" && oldID != newID {
		return fmt.Errorf("新凭据账号标识与旧凭据不一致（%s -> %s），拒绝覆盖",
			shortID(oldID), shortID(newID))
	}
	if oldRealm != "" && newRealm != "" && oldRealm != newRealm {
		return fmt.Errorf("新凭据站点与旧凭据不一致（%s -> %s），拒绝覆盖", oldRealm, newRealm)
	}

	ctx, cancel := context.WithTimeout(context.Background(), credentialVerifyTimeout)
	defer cancel()
	usable, err := verifyAccountUsable(ctx, *refreshed)
	if err != nil {
		log.Printf("[凭据校验] 账号=%s 阶段=覆盖前校验 结果=无法判定，按通过处理 一致性=%s 原因=%v 说明=刷新接口已成功，不因校验抖动丢弃新凭据",
			path, identityNote, err)
		return nil
	}
	if !usable {
		log.Printf("[凭据校验] 账号=%s 阶段=覆盖前校验 结果=不通过 一致性=%s 原因=新访问令牌取不到账号数据 业务影响=放弃本次覆盖，保留旧凭据继续使用",
			path, identityNote)
		return fmt.Errorf("新凭据取不到账号数据，拒绝覆盖旧凭据")
	}
	log.Printf("[凭据校验] 账号=%s 阶段=覆盖前校验 结果=通过 一致性=%s 只读校验=新访问令牌可取到账号数据 说明=允许覆盖旧凭据",
		path, identityNote)
	return nil
}

// credentialStillUsableForDelete 判断一个即将被删除的凭据是否其实还能用。
// 返回值 keep=true 表示应保留凭据文件。
func credentialStillUsableForDelete(auth *StoredAuth) (keep bool, note string) {
	if auth == nil || strings.TrimSpace(auth.Auth.AccessToken) == "" {
		return false, "无访问令牌可校验，按已失效处理"
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialVerifyTimeout)
	defer cancel()
	usable, err := verifyAccountUsable(ctx, *auth)
	switch {
	case err != nil:
		// 无法判定时保守保留：删除不可逆，宁可留一个失效文件。
		return true, fmt.Sprintf("只读校验无法完成（%v），保守保留凭据", err)
	case usable:
		return true, "只读校验显示凭据仍可取到账号数据，保留凭据不删除"
	default:
		return false, "只读校验确认凭据已不可用"
	}
}
