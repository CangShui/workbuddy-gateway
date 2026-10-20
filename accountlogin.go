package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// accountFileNamePattern 限定新增账号只能填一个文件名，不接受目录或通配符。
var accountFileNamePattern = regexp.MustCompile(`^workbuddy[A-Za-z0-9._-]*\.json$`)

// accountCredentialDir 返回凭据落地目录：-auth-dir 优先，否则就是程序运行目录。
func accountCredentialDir() (string, error) {
	accountMu.Lock()
	dir := strings.TrimSpace(cfg.AuthDir)
	accountMu.Unlock()
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("无法解析凭据目录：%w", err)
	}
	return abs, nil
}

// resolveCredentialPath 把用户填写的文件名解析成运行目录下的绝对路径，绝不接受目录。
func resolveCredentialPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("请填写凭据名")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\*?[]`) {
		return "", errors.New("凭据名只能是文件名，不能包含目录或通配符")
	}
	if !accountFileNamePattern.MatchString(name) {
		return "", errors.New("凭据名需以 workbuddy 开头、以 .json 结尾，只能用字母数字和 . _ -")
	}
	dir, err := accountCredentialDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// suggestCredentialFileName 取目录下第一个没被占用的 workbuddyN.json。
func suggestCredentialFileName() string {
	used := map[int]bool{}
	dir, err := accountCredentialDir()
	if err == nil {
		if entries, readErr := os.ReadDir(dir); readErr == nil {
			for _, e := range entries {
				name := strings.TrimSuffix(e.Name(), ".disabled")
				if !strings.HasPrefix(name, "workbuddy") || !strings.HasSuffix(name, ".json") {
					continue
				}
				if n, convErr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "workbuddy"), ".json")); convErr == nil {
					used[n] = true
				}
			}
		}
	}
	for n := 1; ; n++ {
		if !used[n] {
			return fmt.Sprintf("workbuddy%d.json", n)
		}
	}
}

// loginSession 是一次进行中的扫码登录。CLI 的 login 与网页新增账号共用这条路径，
// 区别只在谁来展示二维码、谁把凭据路径拼出来。
type loginSession struct {
	ID      string
	Site    string
	File    string
	AuthURL string

	state    string
	deadline time.Time
	client   *http.Client
	prof     *upstreamProfile
	cancel   context.CancelFunc

	mu      sync.Mutex
	status  string // pending / done / failed / expired / cancelled
	errText string
	auth    *StoredAuth
	savedTo string
}

var (
	loginSessionsMu sync.Mutex
	loginSessions   = map[string]*loginSession{}
)

// loginSessionRetention 让已结束的登录会话在内存中多保留一段时间。
// CLI 直接持有 *loginSession，不依赖会话表；网页只拿到 id，必须再查一次才能看到
// expired / failed / done。若结束时立刻删除，前端轮询几乎总是拿到 404，表现为
// 「链接失效了但控制台还在等」。
var loginSessionRetention = 2 * time.Minute

func newLoginSessionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("login-%d", time.Now().UnixNano())
	}
	return "login-" + hex.EncodeToString(buf)
}

// qrDataURI 生成二维码图片；失败只影响展示，不影响登录流程本身。
func (s *loginSession) qrDataURI() string {
	code, err := qrcode.New(s.AuthURL, qrcode.Medium)
	if err != nil {
		return ""
	}
	png, err := code.PNG(240)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func (s *loginSession) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"id": s.ID, "site": s.Site, "file": s.File, "authUrl": s.AuthURL,
		"status": s.status, "expiresAt": s.deadline.Unix(),
	}
	if s.status == "pending" {
		remaining := int(time.Until(s.deadline).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		out["remainingSeconds"] = remaining
	} else {
		out["remainingSeconds"] = 0
	}
	if s.errText != "" {
		out["error"] = s.errText
	}
	if s.status == "done" {
		out["savedAs"] = filepath.Base(s.savedTo)
		if s.auth != nil {
			out["account"] = map[string]any{
				"nickname": s.auth.Account.Nickname, "uid": s.auth.Account.UID,
				"enterpriseId": s.auth.Account.EnterpriseID, "tokenExpiresAt": s.auth.Auth.ExpiresAt,
			}
		}
	}
	return out
}

func (s *loginSession) finish(status, errText string, sa *StoredAuth, saved string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != "pending" {
		return
	}
	s.status, s.errText, s.auth, s.savedTo = status, errText, sa, saved
}

func forgetLoginSession(id string) {
	loginSessionsMu.Lock()
	delete(loginSessions, id)
	loginSessionsMu.Unlock()
}

// scheduleForgetLoginSession 延迟清理已结束的会话，给网页轮询留出读取终态的时间。
func scheduleForgetLoginSession(id string) {
	time.AfterFunc(loginSessionRetention, func() {
		forgetLoginSession(id)
	})
}

// beginAccountLogin 复用 CLI login 的同一条流程：先取 state 与二维码，再轮询换取 token 并落地凭据。
func beginAccountLogin(site, targetPath string) (*loginSession, error) {
	prof := profileForEdition(site)
	client := &http.Client{Timeout: 30 * time.Second, Jar: cfg.HttpClient.Jar}
	pollHeaders := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("X-No-Authorization", "1")
	}
	data, _, err := doJSON(client, http.MethodPost, prof.authStateURL(), nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("获取登录状态失败: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, errors.New("上游返回的登录状态信息异常，请重试")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &loginSession{
		ID: newLoginSessionID(), Site: prof.Key, File: filepath.Base(targetPath),
		AuthURL: st.AuthURL, state: st.State, deadline: time.Now().Add(prof.LoginTTL),
		client: client, prof: prof, cancel: cancel, status: "pending",
	}
	go s.run(ctx, targetPath, pollHeaders)
	return s, nil
}

func (s *loginSession) run(ctx context.Context, targetPath string, pollHeaders func(*http.Request)) {
	trace := newTraceID()
	log.Printf("[网页登录] traceId=%s 阶段=开始 站点=%s 凭据=%s 说明=已复用 CLI login 同一流程，等待扫码授权",
		trace, s.Site, filepath.Base(targetPath))
	defer scheduleForgetLoginSession(s.ID)
	defer s.cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish("cancelled", "", nil, "")
			return
		case <-ticker.C:
			if time.Now().After(s.deadline) {
				log.Printf("[网页登录] traceId=%s 阶段=等待扫码 结果=超时 站点=%s 业务影响=未写入凭据，账号池不变", trace, s.Site)
				s.finish("expired", "登录已超时，请重新获取登录接口", nil, "")
				return
			}
			tokRaw, _, errTok := doJSON(s.client, http.MethodGet, s.prof.authTokenURL(s.state), pollHeaders, nil)
			if errTok != nil {
				continue
			}
			var tok tokenData
			if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
				continue
			}
			var acct accountData
			acctHeaders := func(r *http.Request) {
				commonHeaders(r, s.prof)
				r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
			}
			if acctRaw, _, errAcct := doJSON(s.client, http.MethodGet, s.prof.loginAcctURL(s.state), acctHeaders, nil); errAcct == nil {
				_ = json.Unmarshal(acctRaw, &acct)
			}
			sa := &StoredAuth{
				Edition: s.prof.Key,
				Auth: StoredTokens{
					AccessToken:  tok.AccessToken,
					RefreshToken: tok.RefreshToken,
					ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
					Domain:       tok.Domain,
				},
				Account: StoredAccount{UID: acct.UID, EnterpriseID: acct.EnterpriseID, Nickname: acct.Nickname},
			}
			if err := saveAuthTo(targetPath, sa); err != nil {
				log.Printf("[网页登录] traceId=%s 阶段=写入凭据 结果=失败 凭据=%s 原因=%v 业务影响=账号未加入账号池",
					trace, filepath.Base(targetPath), err)
				s.finish("failed", "保存凭据失败："+err.Error(), nil, "")
				return
			}
			if err := ensureAccountLoaded(targetPath); err != nil {
				log.Printf("[网页登录] traceId=%s 阶段=加入账号池 结果=失败 凭据=%s 原因=%v 业务影响=文件已写入，可在下次扫描时加载",
					trace, filepath.Base(targetPath), err)
			}
			requestQuotaScan()
			log.Printf("[网页登录] traceId=%s 阶段=完成 站点=%s 凭据=%s 账号=%s 结果=成功 说明=凭据已自动加载，无需重启",
				trace, s.Site, filepath.Base(targetPath), sa.Account.UID)
			s.finish("done", "", sa, targetPath)
			return
		}
	}
}

// ensureAccountLoaded 让刚写入的凭据立刻进入账号池，不依赖目录扫描是否覆盖该文件。
func ensureAccountLoaded(path string) error {
	if _, err := reloadAccountsResult(); err != nil {
		return err
	}
	accountMu.Lock()
	for _, acc := range accounts {
		if acc.Path == path {
			accountMu.Unlock()
			return nil
		}
	}
	accountMu.Unlock()
	sa, err := loadAccountFile(path)
	if err != nil {
		return err
	}
	accountMu.Lock()
	accounts = append(accounts, &Account{Path: path, Auth: sa, Edition: profileForEdition(sa.Edition).Key})
	accountMu.Unlock()
	log.Printf("[账号新增] 凭据=%s 结果=已加入账号池 说明=目录扫描未覆盖该文件时由网页新增流程直接补入", filepath.Base(path))
	return nil
}

func findLoginSession(id string) *loginSession {
	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	return loginSessions[id]
}

// deleteAccountCredential 删除指定凭据文件（只认账号池里已有的那个路径）。
func deleteAccountCredential(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) {
		return "", errors.New("凭据名无效")
	}
	accountMu.Lock()
	target, duplicates := "", 0
	for _, acc := range accounts {
		if filepath.Base(acc.Path) == name {
			duplicates++
			target = acc.Path
		}
	}
	accountMu.Unlock()
	if duplicates > 1 {
		return "", errors.New("存在同名凭据，无法确定要删除哪一个")
	}
	if target == "" {
		return "", errors.New("账号不存在或已删除")
	}
	if !accountFileNamePattern.MatchString(filepath.Base(target)) {
		return "", errors.New("凭据名不符合 workbuddy*.json，拒绝删除")
	}
	info, err := os.Lstat(target)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return "", errors.New("凭据不是普通文件，拒绝删除")
		}
		if err := os.Remove(target); err != nil {
			return "", fmt.Errorf("删除凭据失败：%w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 授权失效流程可能已经删掉了凭据文件，只在账号池里留下失效提示项。
		// 这种条目仍应允许用户从网页显式删除。
	default:
		return "", fmt.Errorf("凭据不可读取：%w", err)
	}
	if err := os.Remove(markerPath(target)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[账号删除] 凭据=%s 失效标记清除失败：%v", filepath.Base(target), err)
	}
	// 显式删除必须无条件移出账号池：热加载收敛会保留 Disabled 的失效提示项，
	// 只删除凭据文件无法让网页和 CLI 里的条目消失。
	accountMu.Lock()
	kept := make([]*Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc.Path == target {
			continue
		}
		kept = append(kept, acc)
	}
	accounts = kept
	if len(accounts) > 0 {
		rrIndex %= len(accounts)
	} else {
		rrIndex = 0
	}
	accountMu.Unlock()
	reloadAccountsResult()
	requestQuotaScan()
	// 立即刷新状态快照，避免网页在下一次 3 秒快照前继续显示已删除账号。
	if err := writeStatusSnapshot(); err != nil {
		log.Printf("[账号删除] 凭据=%s 状态快照刷新失败：%v", filepath.Base(target), err)
	}
	return target, nil
}

// handleWebUIAccountAdd 支持读取默认名（GET）、发起登录（POST）、查询进度（GET ?id=）。
func handleWebUIAccountAdd(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			log.Printf("[账号新增] traceId=%s 阶段=读取默认凭据名 结果=成功 业务影响=只返回文件名，绝对路径不返回前端",
				debugTraceID(r))
			writeAdminData(w, r, map[string]any{"defaultFile": suggestCredentialFileName()})
			return
		}
		s := findLoginSession(id)
		if s == nil {
			log.Printf("[账号新增] traceId=%s 阶段=查询登录进度 id=%s 结果=会话不存在或已结束 业务影响=前端应提示重新获取登录接口",
				debugTraceID(r), id)
			writeJSONError(w, http.StatusNotFound, "登录会话不存在或已结束，请重新获取登录接口")
			return
		}
		writeAdminData(w, r, s.snapshot())
	case http.MethodPost:
		var input struct {
			Site string `json:"site"`
			File string `json:"file"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeJSONError(w, http.StatusBadRequest, "新增账号请求格式无效")
			return
		}
		trace := debugTraceID(r)
		site := strings.ToLower(strings.TrimSpace(input.Site))
		if site != "cn" && site != "intl" {
			log.Printf("[账号新增] traceId=%s 阶段=参数校验 结果=拒绝 站点=%s 原因=站点只能是 cn 或 intl 返回状态码=400 业务影响=未创建登录会话",
				trace, input.Site)
			writeJSONError(w, http.StatusBadRequest, "站点只能选择国内站或国际站")
			return
		}
		target, err := resolveCredentialPath(input.File)
		if err != nil {
			log.Printf("[账号新增] traceId=%s 阶段=参数校验 结果=拒绝 原因=%v 返回状态码=400 业务影响=未创建登录会话", trace, err)
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := os.Lstat(target); err == nil {
			log.Printf("[账号新增] traceId=%s 阶段=参数校验 结果=拒绝 凭据=%s 原因=同名文件已存在 返回状态码=409 业务影响=未覆盖已有凭据",
				trace, filepath.Base(target))
			writeJSONError(w, http.StatusConflict, "同名凭据已存在，请换一个名字")
			return
		}
		s, err := beginAccountLogin(site, target)
		if err != nil {
			log.Printf("[账号新增] traceId=%s 阶段=获取登录接口 结果=失败 站点=%s 原因=%v 业务影响=未创建登录会话", trace, site, err)
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		loginSessionsMu.Lock()
		loginSessions[s.ID] = s
		loginSessionsMu.Unlock()
		log.Printf("[账号新增] traceId=%s 阶段=获取登录接口 结果=成功 站点=%s 凭据=%s 会话=%s 业务影响=等待扫码后自动写入并加载",
			trace, site, filepath.Base(target), s.ID)
		body := s.snapshot()
		body["qrPng"] = s.qrDataURI()
		writeAdminData(w, r, body)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

// handleWebUIAccountCancel 取消进行中的登录会话，避免后台持续轮询。
func handleWebUIAccountCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "取消登录请求格式无效")
		return
	}
	trace := debugTraceID(r)
	s := findLoginSession(strings.TrimSpace(input.ID))
	if s == nil {
		log.Printf("[账号新增] traceId=%s 阶段=取消登录 结果=会话不存在 业务影响=无需取消", trace)
		writeAdminData(w, r, map[string]any{"cancelled": false})
		return
	}
	s.cancel()
	log.Printf("[账号新增] traceId=%s 阶段=取消登录 结果=成功 站点=%s 凭据=%s 业务影响=停止轮询，未写入任何凭据",
		trace, s.Site, s.File)
	writeAdminData(w, r, map[string]any{"cancelled": true})
}

// handleWebUIAccountDelete 删除指定凭据文件。
func handleWebUIAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input struct {
		File string `json:"file"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "删除账号请求格式无效")
		return
	}
	trace := debugTraceID(r)
	target, err := deleteAccountCredential(input.File)
	if err != nil {
		log.Printf("[账号删除] traceId=%s 阶段=删除凭据 结果=失败 凭据=%s 原因=%v 返回状态码=400 业务影响=凭据与账号池均未变化",
			trace, input.File, err)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("[账号删除] traceId=%s 阶段=删除凭据 结果=成功 凭据=%s 业务影响=凭据文件已删除，账号池已同步移除，统计与历史日志保留",
		trace, filepath.Base(target))
	writeAdminData(w, r, map[string]any{"deleted": filepath.Base(target)})
}
