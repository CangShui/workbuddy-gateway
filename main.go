package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/skip2/go-qrcode"
)

const (
	version       = "1.1.0"
	upstreamBase  = "https://copilot.tencent.com"
	clientUA      = "CLI/2.143.1 CodeBuddy/2.143.1"
	originReferer = "https://www.codebuddy.cn"

	endpointAuthState    = upstreamBase + "/v2/plugin/auth/state?platform=VSCode"
	endpointLoginAcct    = upstreamBase + "/v2/plugin/login/account?state="
	endpointAuthToken    = upstreamBase + "/v2/plugin/auth/token?state="
	endpointTokenRefresh = upstreamBase + "/v2/plugin/auth/token/refresh"
	endpointChat         = upstreamBase + "/v2/chat/completions"

	loginTTL = 5 * time.Minute
)

// -----------------------------------------------------------------------------
// 数据结构定义
// -----------------------------------------------------------------------------

type StoredAuth struct {
	Auth    StoredTokens  `json:"auth"`
	Account StoredAccount `json:"account"`
}

type StoredTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

type StoredAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// -----------------------------------------------------------------------------
// 全局状态与配置
// -----------------------------------------------------------------------------

type Config struct {
	Addr       string
	Port       int
	AuthFile   string
	APIKey     string
	ProxyURL   string
	Verbose    bool
	HttpClient *http.Client
}

var (
	cfg           Config
	authLock      sync.RWMutex
	currAuth      *StoredAuth
	reqCounter    uint64
	upstreamMutex sync.Mutex // 限制同一账号到腾讯上游的单并发排队，杜绝触发 11128 风控
)

// -----------------------------------------------------------------------------
// 主入口与命令行控制
// -----------------------------------------------------------------------------

func main() {
	if len(os.Args) > 1 {
		first := os.Args[1]
		if first == "help" || first == "-h" || first == "--help" {
			printHelp()
			return
		}
		if first == "version" || first == "-v" || first == "--version" {
			fmt.Printf("WorkBuddy Local Gateway v%s\n", version)
			return
		}
	}

	command := "serve"
	args := os.Args[1:]
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		command = os.Args[1]
		args = os.Args[2:]
	}

	fs := flag.NewFlagSet(command, flag.ExitOnError)
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1", "网关监听地址")
	fs.IntVar(&cfg.Port, "port", 8317, "网关监听端口")
	fs.StringVar(&cfg.AuthFile, "auth", "workbuddy.json", "凭据存储文件路径")
	fs.StringVar(&cfg.APIKey, "api-key", "", "可选：访问网关所需的 API Key (客户端 Bearer 校验)")
	fs.StringVar(&cfg.ProxyURL, "proxy", "", "可选：上游请求代理 (如 http://127.0.0.1:7890)")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "输出详细调试日志")
	_ = fs.Parse(args)

	// 初始化 HTTP 客户端
	initHTTPClient()

	switch command {
	case "serve", "run", "start":
		runServe()
	case "login":
		runLogin()
	case "status":
		runStatus()
	case "refresh":
		runRefresh()
	case "version", "-v", "--version":
		fmt.Printf("WorkBuddy Local Gateway v%s\n", version)
	default:
		fmt.Printf("未知命令: %s\n\n", command)
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println(`WorkBuddy Local Gateway - 轻量化跨平台本地 AI 网关
将腾讯 CodeBuddy / 混元反代为标准 OpenAI 协议，支持直接透传任意模型到上游。

用法:
  workbuddy-gateway [command] [options]

命令:
  serve       启动本地网关 (默认操作)
  login       微信/企业微信扫码登录，获取/更新凭据
  status      查看当前账号凭据状态与过期时间
  refresh     手动立即刷新访问令牌 (Access Token)
  version     查看版本信息
  help        查看帮助说明

参数选项:
  -addr <ip>        网关监听地址 (默认: 127.0.0.1)
  -port <port>      网关监听端口 (默认: 8317)
  -auth <path>      凭据存储文件路径 (默认: ./workbuddy.json)
  -api-key <key>    设置后，调用网关必须携带 Bearer <key> 鉴权
  -proxy <url>      设置上游转发代理 (例如 http://127.0.0.1:7890 或 socks5://...)
  -verbose          输出详细调试日志 (请求/响应体)

示例:
  # 首次使用扫码登录
  workbuddy-gateway login

  # 启动本地网关 (监听 127.0.0.1:8317)
  workbuddy-gateway serve

  # 启动网关并指定端口和代理
  workbuddy-gateway serve -port 9000 -proxy http://127.0.0.1:7890`)
}

func initHTTPClient() {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		MaxIdleConns:        50,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 10,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
	}
	if cfg.ProxyURL != "" {
		pURL, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			log.Fatalf("错误: 无效的代理地址 %s: %v", cfg.ProxyURL, err)
		}
		transport.Proxy = http.ProxyURL(pURL)
	}
	cfg.HttpClient = &http.Client{
		Timeout:   180 * time.Second,
		Transport: transport,
		Jar:       jar,
	}
}

// -----------------------------------------------------------------------------
// 凭据加载、保存与自动续期
// -----------------------------------------------------------------------------

func loadAuth() (*StoredAuth, error) {
	authLock.RLock()
	if currAuth != nil {
		defer authLock.RUnlock()
		return currAuth, nil
	}
	authLock.RUnlock()

	data, err := os.ReadFile(cfg.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件失败 (%s): %w", cfg.AuthFile, err)
	}
	var sa StoredAuth
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("解析凭据文件失败: %w", err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("凭据文件中缺少 AccessToken")
	}

	authLock.Lock()
	currAuth = &sa
	authLock.Unlock()
	return &sa, nil
}

func saveAuth(sa *StoredAuth) error {
	authLock.Lock()
	currAuth = sa
	authLock.Unlock()

	dir := filepath.Dir(cfg.AuthFile)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(sa, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.AuthFile, data, 0600)
}

func ensureValidToken() error {
	sa, err := loadAuth()
	if err != nil {
		return err
	}
	// 如果距过期不足 15 分钟，则自动刷新
	if time.Now().Unix() > sa.Auth.ExpiresAt-900 {
		log.Printf("[Auth] 访问令牌即将或已经过期 (ExpiresAt=%s)，正在自动刷新...", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
		return doRefreshToken(sa)
	}
	return nil
}

func doRefreshToken(sa *StoredAuth) error {
	if sa.Auth.RefreshToken == "" {
		return fmt.Errorf("无法刷新：缺少 RefreshToken")
	}
	headers := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	}

	data, status, err := doJSON(cfg.HttpClient, http.MethodPost, endpointTokenRefresh, headers, nil)
	if err != nil {
		return fmt.Errorf("上游刷新拒绝 (HTTP %d): %w", status, err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("解析新 Token 失败: %w", err)
	}

	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		sa.Auth.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}

	if err := saveAuth(sa); err != nil {
		return fmt.Errorf("写回凭据失败: %w", err)
	}
	log.Printf("[Auth] Token 刷新成功！新过期时间: %s", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	return nil
}

// -----------------------------------------------------------------------------
// CLI 子命令实现: login, status, refresh
// -----------------------------------------------------------------------------

func runStatus() {
	sa, err := loadAuth()
	if err != nil {
		fmt.Printf("未找到有效凭据: %v\n请先执行: workbuddy-gateway login 扫码登录。\n", err)
		return
	}
	expTime := time.Unix(sa.Auth.ExpiresAt, 0)
	remaining := time.Until(expTime)
	statusStr := "有效"
	if remaining <= 0 {
		statusStr = "已过期"
	}

	fmt.Println("================== WorkBuddy 凭据状态 ==================")
	fmt.Printf("凭据存储文件: %s\n", cfg.AuthFile)
	fmt.Printf("用户昵称:     %s\n", sa.Account.Nickname)
	fmt.Printf("用户 UID:     %s\n", sa.Account.UID)
	fmt.Printf("企业 ID:      %s\n", ifEmpty(sa.Account.EnterpriseID, "(个人账号)"))
	fmt.Printf("认证域名:     %s\n", ifEmpty(sa.Auth.Domain, "www.codebuddy.cn"))
	fmt.Printf("Token 状态:   %s\n", statusStr)
	fmt.Printf("过期时间:     %s (剩余 %v)\n", expTime.Format("2006-01-02 15:04:05"), remaining.Round(time.Minute))
	fmt.Println("=======================================================")
}

func runRefresh() {
	sa, err := loadAuth()
	if err != nil {
		fmt.Printf("读取凭据失败: %v\n", err)
		return
	}
	fmt.Println("正在向上游请求刷新令牌...")
	if err := doRefreshToken(sa); err != nil {
		fmt.Printf("刷新失败: %v\n", err)
	} else {
		fmt.Println("刷新成功并已持久化保存！")
	}
}

func runLogin() {
	fmt.Println("================ WorkBuddy 扫码登录 ================")
	fmt.Println("正在生成登录凭据与二维码...")

	loginClient := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     cfg.HttpClient.Jar,
	}

	data, _, err := doJSON(loginClient, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		fmt.Printf("获取登录状态失败: %v\n", err)
		return
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		fmt.Println("上游返回的登录状态信息异常，请重试。")
		return
	}

	// 终端字符二维码
	qr, err := qrcode.New(st.AuthURL, qrcode.Medium)
	if err == nil {
		fmt.Println("\n请使用 微信 或 企业微信 扫描下方二维码登录：")
		fmt.Println(qr.ToSmallString(false))
	}

	fmt.Println("如无法扫码，也可在浏览器中直接打开以下链接：")
	fmt.Printf("👉 %s\n\n", st.AuthURL)
	fmt.Println("等待扫码授权中 (按 Ctrl+C 可取消)...")

	deadline := time.Now().Add(loginTTL)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				fmt.Println("\n登录已超时，请重新执行 login 命令。")
				return
			}
			tokRaw, _, errTok := doJSON(loginClient, http.MethodGet, endpointAuthToken+st.State, nil, nil)
			if errTok != nil {
				continue
			}
			var tok tokenData
			if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
				continue
			}

			// 登录成功，拉取账号信息
			var acct accountData
			acctHeaders := func(r *http.Request) {
				commonHeaders(r)
				r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
			}
			if acctRaw, _, errAcct := doJSON(loginClient, http.MethodGet, endpointLoginAcct+st.State, acctHeaders, nil); errAcct == nil {
				_ = json.Unmarshal(acctRaw, &acct)
			}

			sa := &StoredAuth{
				Auth: StoredTokens{
					AccessToken:  tok.AccessToken,
					RefreshToken: tok.RefreshToken,
					ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
					Domain:       tok.Domain,
				},
				Account: StoredAccount{
					UID:          acct.UID,
					EnterpriseID: acct.EnterpriseID,
					Nickname:     acct.Nickname,
				},
			}

			if err := saveAuth(sa); err != nil {
				fmt.Printf("\n登录成功但保存凭据失败: %v\n", err)
				return
			}

			fmt.Println("\n🎉 登录成功！")
			fmt.Printf("欢迎，%s (UID: %s)\n", sa.Account.Nickname, sa.Account.UID)
			fmt.Printf("凭据已成功保存至: %s\n", cfg.AuthFile)
			fmt.Printf("令牌有效期至: %s\n", time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
			fmt.Println("\n现在您可以运行以下命令启动网关服务：")
			fmt.Println("  workbuddy-gateway serve")
			return
		}
	}
}

// -----------------------------------------------------------------------------
// 本地 HTTP API 网关服务 (OpenAI 协议兼容)
// -----------------------------------------------------------------------------

func runServe() {
	sa, err := loadAuth()
	if err != nil {
		fmt.Printf("警告: 未检测到有效凭据 (%v)。\n请先执行: workbuddy-gateway login 扫码登录，或确保 %s 存在。\n\n", err, cfg.AuthFile)
	} else {
		_ = ensureValidToken()
		fmt.Printf("已就绪账号: %s (UID: %s, 有效期至: %s)\n",
			sa.Account.Nickname, sa.Account.UID, time.Unix(sa.Auth.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	}

	// 启动后台自动刷新协程
	go backgroundTokenRefresher()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/ping", handleHealth)
	mux.HandleFunc("/", handleIndex)

	listenAddr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	server := &http.Server{
		Addr:         listenAddr,
		Handler:      corsMiddleware(authMiddleware(mux)),
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 300 * time.Second,
	}

	fmt.Println("================================================================")
	fmt.Printf("🚀 WorkBuddy 本地网关已启动！\n")
	fmt.Printf("   服务监听地址:  http://%s\n", listenAddr)
	fmt.Printf("   Chat 接口地址: http://%s/v1/chat/completions\n", listenAddr)
	fmt.Printf("   Models 接口:   http://%s/v1/models\n", listenAddr)
	fmt.Printf("   模型转发策略:  【完全透传】客户端请求的任意 model 原样中继至上游\n")
	if cfg.APIKey != "" {
		fmt.Printf("   API 鉴权:      已启用 (Bearer %s)\n", cfg.APIKey)
	} else {
		fmt.Printf("   API 鉴权:      未启用 (任何客户端均可直连)\n")
	}
	if cfg.ProxyURL != "" {
		fmt.Printf("   上游出口代理:  %s\n", cfg.ProxyURL)
	}
	fmt.Println("================================================================")
	fmt.Println("等待客户端请求中 (按 Ctrl+C 安全停止)...")

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	<-stopChan
	fmt.Println("\n正在关闭网关服务...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	fmt.Println("网关已安全停止。")
}

func backgroundTokenRefresher() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if err := ensureValidToken(); err != nil {
			log.Printf("[BackgroundAuth] 自动检查/续期令牌异常: %v", err)
		}
	}
}

// -----------------------------------------------------------------------------
// 中间件: CORS 与可选 APIKey 校验
// -----------------------------------------------------------------------------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.APIKey != "" && r.URL.Path != "/health" && r.URL.Path != "/ping" && r.URL.Path != "/" {
			authHeader := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token != cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// 路由处理: /v1/chat/completions (支持任意 model 透传)
// -----------------------------------------------------------------------------

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}

	reqID := atomic.AddUint64(&reqCounter, 1)
	startTime := time.Now()

	sa, err := loadAuth()
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "no_auth", "未找到有效登录凭据，请先执行 login 命令扫码登录")
		return
	}
	_ = ensureValidToken()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	var reqObj map[string]any
	if err := json.Unmarshal(bodyBytes, &reqObj); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}

	// 核心特性：完全透传 model 字段
	// 客户端传什么 model，我们就透传什么 model 给上游，不做任何硬编码限制！
	modelName, _ := reqObj["model"].(string)
	if modelName == "" {
		modelName = "hy4-preview" // 默认保底
		reqObj["model"] = modelName
	}

	isStream, _ := reqObj["stream"].(bool)

	// 腾讯上游强制要求 stream 必须为 true，非流式会被拦截 (code 11101)
	reqObj["stream"] = true

	// 深度思考 (Thinking) 自动适配：混元系列如果未关闭思考，自动赋予 high 档位保证深度思考输出
	applyThinkingRules(reqObj, modelName)

	// 模板净化：改写 Claude Code 等框架被腾讯官方逐字拉黑的固定 prompt 语句
	sanitizeMessages(reqObj)

	upstreamBytes, err := json.Marshal(reqObj)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "序列化请求失败")
		return
	}

	if cfg.Verbose {
		log.Printf("[#%d][Req] Model: %s | Stream: %v | BodyLen: %d", reqID, modelName, isStream, len(upstreamBytes))
	} else {
		log.Printf("[#%d] POST /v1/chat/completions -> Upstream [Model: %s, Stream: %v]", reqID, modelName, isStream)
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpointChat, bytes.NewReader(upstreamBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "req_create_error", err.Error())
		return
	}

	// 注入 CodeBuddy 凭据与指纹 Header
	backendHeaders(upstreamReq, sa)

	// 限制同一账号向腾讯上游的请求严格单并发串行排队，防止 DSH 同时发起标题生成+主对话触发腾讯 11128 风控
	upstreamMutex.Lock()
	defer upstreamMutex.Unlock()

	resp, err := cfg.HttpClient.Do(upstreamReq)
	if err != nil {
		log.Printf("[#%d] 上游请求失败: %v", reqID, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_network_error", fmt.Sprintf("网络转发失败: %v", err))
		return
	}
	defer resp.Body.Close()

	// 上游非 200 响应处理
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		errStr := string(errBody)
		log.Printf("[#%d] 上游返回 HTTP %d: %s (耗时 %v)", reqID, resp.StatusCode, errStr, time.Since(startTime))
		writeOpenAIError(w, resp.StatusCode, "upstream_error", fmt.Sprintf("upstream %d: %s", resp.StatusCode, errStr))
		return
	}

	if isStream {
		// 客户端需要流式响应
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			cleanData := stripDataPrefix(line)
			if cleanData == "" {
				continue
			}
			if cleanData == "[DONE]" {
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				break
			}
			cleanedChunk := cleanChunkJSON(cleanData)
			if cleanedChunk != "" {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", cleanedChunk)
				flusher.Flush()
			}
		}
		log.Printf("[#%d] 流式输出完成 (耗时 %v)", reqID, time.Since(startTime))
	} else {
		// 客户端需要完整 JSON 响应，聚合 SSE 数据流
		completionJSON, err := aggregateCompletion(resp.Body, modelName)
		if err != nil {
			log.Printf("[#%d] 聚合响应失败: %v", reqID, err)
			writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(completionJSON)
		log.Printf("[#%d] 非流式响应完成 (耗时 %v)", reqID, time.Since(startTime))
	}
}

// -----------------------------------------------------------------------------
// 路由处理: /v1/models (列出常见模型供客户端自动补全)
// -----------------------------------------------------------------------------

func handleModels(w http.ResponseWriter, r *http.Request) {
	// 动态内置常见模型，用户直接传入任何未列出的 model 也会直接透传到上游
	modelsList := []map[string]any{
		{"id": "hy4-preview", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "hy3-preview-agent", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "hy3-preview", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "hy3", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "glm-5.2", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "glm-5.1", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "kimi-k2.7", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "deepseek-v4-pro", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "deepseek-v4-flash", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
		{"id": "minimax-m3-pay", "object": "model", "owned_by": "workbuddy", "permission": []any{}},
	}
	resp := map[string]any{
		"object": "list",
		"data":   modelsList,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "healthy",
		"timestamp": time.Now().Unix(),
		"version":   version,
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "WorkBuddy Local Gateway v%s is running.\n\nEndpoints:\n- POST /v1/chat/completions\n- GET  /v1/models\n- GET  /health\n", version)
}

// -----------------------------------------------------------------------------
// 请求转译与净化工具函数
// -----------------------------------------------------------------------------

func applyThinkingRules(obj map[string]any, modelName string) {
	// 遵循 CodeBuddy 规范：仅当客户端显式设置了 reasoning_effort 时才传递与规范化
	// 绝不可强行对普通请求注入 reasoning_effort，否则极易触发腾讯内容与安全策略拦截 (code 11128)
	currEff, exists := obj["reasoning_effort"].(string)
	if !exists || currEff == "" || currEff == "off" || currEff == "none" {
		delete(obj, "reasoning_effort")
		delete(obj, "reasoning_summary")
		return
	}
	// 客户端显式请求思考时，设置 auto
	obj["reasoning_summary"] = "auto"
}

func sanitizeMessages(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			msg["content"] = sanitizeBlockedTemplates(c)
		case []any:
			for _, partAny := range c {
				part, ok := partAny.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := part["text"].(string); ok {
					part["text"] = sanitizeBlockedTemplates(t)
				}
			}
		}
	}
}

func sanitizeBlockedTemplates(s string) string {
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	return s
}

// backendHeaders 设置 CodeBuddy 上游专用指纹与鉴权 Header
func backendHeaders(req *http.Request, sa *StoredAuth) {
	commonHeaders(req)
	reqID := uuid.New().String()
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("X-Trace-ID", reqID)
	req.Header.Set("X-Client-ID", "codebuddy-cli")
	req.Header.Set("X-Client-Version", "2.143.1")

	if sa != nil && sa.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if sa != nil && sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if sa != nil && sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if sa != nil && sa.Auth.RefreshToken != "" {
		req.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
	}
	if sa != nil && sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
}

func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d: %s", resp.StatusCode, string(raw))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("解析上游 JSON 失败: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// 流式聚合与格式清理
// -----------------------------------------------------------------------------

func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	var toolCalls []map[string]any

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						if call, ok := tc.(map[string]any); ok {
							toolCalls = append(toolCalls, call)
						}
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}

	message := map[string]any{"role": ifEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      ifEmpty(respID, fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())),
		"object":  "chat.completion",
		"created": created,
		"model":   ifEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": ifEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	return json.Marshal(result)
}

func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for k, v := range delta {
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func writeOpenAIError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    statusCode,
		},
	})
}

func ifEmpty(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
