package main

import (
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WebUI 是独立端口的管理台：状态接口只读，仅首次设置与鉴权配置允许写入。
// 设计约束：不改变转发主链路与调度逻辑，仅复用进程已有的状态快照、模型统计、
// 日志文件与配置/凭据文件，凭据令牌一律脱敏。
//
//go:embed web/index.html web/app.js web/style.css
var webFS embed.FS

const (
	webUIPrefix      = "/ui"
	webUIPrefixSlash = "/ui/"
)

// webuiEnabled 返回当前进程是否启用只读管理台。
func webuiEnabled() bool { return cfg.WebUI }

// 管理 Key 可在首次本机访问时初始化，不能再强制开启模型 API 鉴权。
func webUIPreflightError() error {
	if cfg.WebUI && cfg.Port == cfg.WebPort {
		return errors.New("API 与 Web 端口不能相同")
	}
	return nil
}

// webUIIsExemptPath 判断该路径是否属于「无数据的静态外壳」，可免 API 密钥访问。
// 只有 /ui 与 /ui/ 下的静态资源免鉴权；/admin/api/* 一律需要密钥。
func webUIIsExemptPath(p string) bool {
	if !webuiEnabled() {
		return false
	}
	return p == webUIPrefix || p == webUIPrefixSlash || strings.HasPrefix(p, webUIPrefixSlash)
}

// registerWebUIRoutes 在启用管理台时注册静态资源与只读 API 路由。
func registerWebUIRoutes(mux *http.ServeMux) {
	mux.HandleFunc(webUIPrefix, handleWebUIIndex)
	mux.HandleFunc(webUIPrefixSlash, handleWebUIIndex)
	mux.HandleFunc("/admin/api/status", handleWebUIStatus)
	mux.HandleFunc("/admin/api/models", handleWebUIModels)
	mux.HandleFunc("/admin/api/logs", handleWebUILogs)
	mux.HandleFunc("/admin/api/config", handleWebUIConfig)
	mux.HandleFunc("/admin/api/credentials", handleWebUICredentials)
	mux.HandleFunc("/admin/api/setup", handleWebUISetup)
	mux.HandleFunc("/admin/api/settings", handleWebUISettings)
	mux.HandleFunc("/admin/api/client-events", handleWebUIClientEvent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleWebUIIndex 提供管理台静态外壳与静态资源。
// 访问 /ui 时跳到 /ui/，保证相对资源路径正确；/ui/<file> 按名读取嵌入资源。
func handleWebUIIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/HEAD")
		return
	}
	if r.URL.Path == webUIPrefix {
		http.Redirect(w, r, webUIPrefixSlash, http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, webUIPrefixSlash)
	if name == "" {
		name = "index.html"
	}
	serveWebAsset(w, name)
}

// serveWebAsset 从嵌入资源读取单个静态文件，拒绝任何路径穿越。
func serveWebAsset(w http.ResponseWriter, name string) {
	name = path.Clean(name)
	if name == "." || strings.HasPrefix(name, "..") || strings.Contains(name, "/") {
		http.NotFound(w, nil)
		return
	}
	data, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	_, _ = w.Write(data)
}

// webGatewayMeta 是管理台展示的网关运行元信息（不含任何密钥）。
type webGatewayMeta struct {
	Version       string `json:"version"`
	Listen        string `json:"listen"`
	APIKeyEnabled bool   `json:"apiKeyEnabled"`
	WebUIEnabled  bool   `json:"webuiEnabled"`
	ModelSource   string `json:"modelSource,omitempty"`
	WebListen     string `json:"webListen,omitempty"`
}

// webStatusResponse 是 /admin/api/status 的响应体。
type webStatusResponse struct {
	Gateway     webGatewayMeta            `json:"gateway"`
	UpdatedAt   int64                     `json:"updatedAt"`
	Summary     map[string]int            `json:"summary"`
	SiteSummary map[string]map[string]int `json:"siteSummary"`
	Accounts    []accountSnapshot         `json:"accounts"`
	Models      []modelStatSnapshot       `json:"models"`
	Stale       bool                      `json:"stale"`
}

// readStatusSnapshot 读取 serve 周期写入的状态快照（与 monitor 命令同一数据源）。
func readStatusSnapshot() (statusSnapshot, error) {
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		return statusSnapshot{}, err
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return statusSnapshot{}, err
	}
	if snap.UpdatedAt <= 0 {
		return statusSnapshot{}, errors.New("状态快照没有有效更新时间")
	}
	return snap, nil
}

func summarizeAccounts(accs []accountSnapshot) map[string]int {
	summary := map[string]int{
		"total": len(accs), "active": 0, "cooldown": 0,
		"paidExhausted": 0, "expired": 0, "disabled": 0,
	}
	for _, a := range accs {
		switch a.State {
		case "active":
			summary["active"]++
		case "cooldown":
			summary["cooldown"]++
		case "quota_exhausted", "paid_exhausted":
			summary["paidExhausted"]++
		case "expired":
			summary["expired"]++
		case "disabled":
			summary["disabled"]++
		}
	}
	return summary
}

// summarizeAccountsBySite 按站点（cn/intl）分别汇总账号数量，供总览页分站点展示。
// 站点标识缺失时按国内站处理，与凭据默认站点保持一致。
func summarizeAccountsBySite(accs []accountSnapshot) map[string]map[string]int {
	sites := map[string][]accountSnapshot{}
	for _, a := range accs {
		key := a.Edition
		if key != "intl" {
			key = "cn"
		}
		sites[key] = append(sites[key], a)
	}
	return map[string]map[string]int{
		"cn":   summarizeAccounts(sites["cn"]),
		"intl": summarizeAccounts(sites["intl"]),
	}
}

func handleWebUIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	resp := webStatusResponse{
		Gateway: webGatewayMeta{
			Version:       version,
			Listen:        net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.Port)),
			WebListen:     net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.WebPort)),
			APIKeyEnabled: currentAPIKey() != "",
			WebUIEnabled:  webuiEnabled(),
			ModelSource:   modelSourceLabel(modelSourceFromSnapshots(snap.Models)),
		},
		UpdatedAt: snap.UpdatedAt,
		Accounts:  snap.Accounts,
		Models:    snap.Models,
		Stale:     time.Now().Unix()-snap.UpdatedAt > 15,
	}
	if resp.Accounts == nil {
		resp.Accounts = []accountSnapshot{}
	}
	if resp.Models == nil {
		resp.Models = []modelStatSnapshot{}
	}
	resp.Summary = summarizeAccounts(resp.Accounts)
	resp.SiteSummary = summarizeAccountsBySite(resp.Accounts)
	log.Printf("[管理状态] traceId=%s 结果=读取成功 账号数=%d 模型数=%d 快照过旧=%t 说明=只展示快照，不调用上游或改变账本", debugTraceID(r), len(resp.Accounts), len(resp.Models), resp.Stale)
	writeAdminData(w, r, resp)
}

func handleWebUIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	snap, err := readStatusSnapshot()
	if err != nil {
		webSnapshotUnavailable(w, r, err)
		return
	}
	models := snap.Models
	if models == nil {
		models = []modelStatSnapshot{}
	}
	log.Printf("[管理模型] traceId=%s 结果=读取成功 模型数=%d", debugTraceID(r), len(models))
	writeAdminData(w, r, map[string]any{
		"source": modelSourceLabel(modelSourceFromSnapshots(models)),
		"models": models,
		"stale":  time.Now().Unix()-snap.UpdatedAt > 15,
	})
}

func webSnapshotUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[管理状态] traceId=%s 阶段=读取快照 结果=失败 原因=%v 状态码=503 业务影响=前端保留上次数据，不伪装成零账号", debugTraceID(r), err)
	writeJSONError(w, http.StatusServiceUnavailable, "状态快照暂不可用，请稍后重试；已有页面数据不是新的零账号结果")
}

var logFilePattern = regexp.MustCompile(`^(gateway-\d{4}-\d{2}-\d{2}\.log|debug-\d{4}-\d{2}-\d{2}\.jsonl)$`)

type logFileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

// listLogFiles 仅列出 logs/ 下受限命名格式的日志文件，最新在前。
func listLogFiles() []logFileInfo {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return []logFileInfo{}
	}
	files := make([]logFileInfo, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || !logFilePattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, logFileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files
}

func handleWebUILogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	files := listLogFiles()
	name := r.URL.Query().Get("file")
	if name != "" && (baseNameSafe(name) != name || !logFilePattern.MatchString(name)) {
		log.Printf("[管理日志] traceId=%s 结果=拒绝 原因=日志文件名不合法 状态码=400", debugTraceID(r))
		writeJSONError(w, http.StatusBadRequest, "非法的日志文件名")
		return
	}
	if name == "" && len(files) > 0 {
		name = files[0].Name
	}

	linesN := 200
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > 2000 {
				n = 2000
			}
			linesN = n
		}
	}

	resp := map[string]any{"files": files, "file": name, "lines": []string{}, "truncated": false, "readBudgetBytes": webUILogReadBudget}
	if name != "" {
		log.Printf("[管理日志] traceId=%s 阶段=读取 文件=%s 请求行数=%d 扫描字节预算=%d", debugTraceID(r), name, linesN, webUILogReadBudget)
		lines, truncated, err := readWebLog(name, linesN)
		if err != nil {
			log.Printf("[管理日志] traceId=%s 结果=失败 文件=%s 原因=%v 状态码=503", debugTraceID(r), name, err)
			writeJSONError(w, http.StatusServiceUnavailable, "日志暂不可读，请刷新文件列表后重试")
			return
		}
		// 脱敏完整行副本，覆盖旧版本写出的日志；不改动文件或业务错误。
		if len(lines) > 0 {
			// 一批日志只建立一次秘密值替换器，避免逐行重复构建。
			lines = strings.Split(redactSensitiveText(strings.Join(lines, "\n")), "\n")
		}
		resp["lines"], resp["truncated"] = lines, truncated
		log.Printf("[管理日志] traceId=%s 结果=成功 返回行数=%d 预算截断=%t 秘密值已隐藏=true", debugTraceID(r), len(lines), truncated)
	}
	writeJSON(w, http.StatusOK, resp)
}

func readWebLog(name string, lines int) ([]string, bool, error) {
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	return readTailLines(f, lines, webUILogReadBudget)
}

// baseNameSafe 只取基名，避免任何目录穿越意图进入后续校验。
func baseNameSafe(name string) string {
	name = strings.TrimSpace(name)
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	return name
}

func handleWebUIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	gatewayConfigMu.RLock()
	data, err := os.ReadFile(runtimeConfigFile)
	gatewayConfigMu.RUnlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"exists": false, "path": runtimeConfigFile, "content": ""})
		} else {
			log.Printf("[管理配置读取] traceId=%s 结果=失败 原因=%v", debugTraceID(r), err)
			writeJSONError(w, http.StatusServiceUnavailable, "配置文件暂不可读")
		}
		return
	}
	var parsed any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil || ensureJSONEOF(decoder) != nil {
		log.Printf("[管理配置读取] traceId=%s 结果=失败 原因=配置文件不是有效JSON，拒绝返回未脱敏原文", debugTraceID(r))
		writeJSONError(w, http.StatusServiceUnavailable, "配置文件暂不可解析")
		return
	}
	safe, err := json.MarshalIndent(redactDebugValue(parsed), "", "  ")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "配置展示编码失败")
		return
	}
	log.Printf("[管理配置读取] traceId=%s 结果=成功 管理Key与APIKey已隐藏=true 说明=磁盘配置仍按用户要求明文保存", debugTraceID(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":  true,
		"path":    runtimeConfigFile,
		"content": string(safe),
	})
}

// credentialView 是脱敏后的凭据视图：只保留非敏感字段，令牌仅显示前缀。
type credentialView struct {
	Path               string `json:"path"`
	Exists             bool   `json:"exists"`
	Error              string `json:"error,omitempty"`
	Edition            string `json:"edition,omitempty"`
	Nickname           string `json:"nickname,omitempty"`
	UID                string `json:"uid,omitempty"`
	EnterpriseID       string `json:"enterpriseId,omitempty"`
	Domain             string `json:"domain,omitempty"`
	ExpiresAt          int64  `json:"expiresAt,omitempty"`
	RefreshExpiresAt   int64  `json:"refreshExpiresAt,omitempty"`
	LastRefreshTime    int64  `json:"lastRefreshTime,omitempty"`
	AccessTokenMasked  string `json:"accessTokenMasked,omitempty"`
	RefreshTokenMasked string `json:"refreshTokenMasked,omitempty"`
}

// maskToken 仅保留令牌前 6 位用于识别，其余以星号替代；空值保持为空。
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 6 {
		return "****"
	}
	return token[:6] + "****"
}

func handleWebUICredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	views := make([]credentialView, 0)
	for _, p := range collectConfiguredAuthPaths() {
		view := credentialView{Path: p, Exists: false}
		data, err := os.ReadFile(p)
		if err != nil {
			views = append(views, view)
			continue
		}
		var sa StoredAuth
		if err := json.Unmarshal(data, &sa); err != nil {
			view.Exists = true
			view.Error = "凭据文件解析失败"
			views = append(views, view)
			continue
		}
		view.Exists = true
		view.Edition = sa.Edition
		view.Nickname = sa.Account.Nickname
		view.UID = sa.Account.UID
		view.EnterpriseID = sa.Account.EnterpriseID
		view.Domain = sa.Auth.Domain
		view.ExpiresAt = sa.Auth.ExpiresAt
		view.RefreshExpiresAt = sa.Auth.RefreshExpiresAt
		view.LastRefreshTime = sa.Auth.LastRefreshTime
		view.AccessTokenMasked = maskToken(sa.Auth.AccessToken)
		view.RefreshTokenMasked = maskToken(sa.Auth.RefreshToken)
		views = append(views, view)
	}
	log.Printf("[管理凭据] traceId=%s 结果=读取完成 文件数=%d 说明=只返回脱敏视图，不触发刷新或写入", debugTraceID(r), len(views))
	writeAdminData(w, r, map[string]any{
		"hint":  "令牌已脱敏，仅显示前缀；完整凭据仅保存在本地凭据文件中。",
		"files": views,
		// 保留字段名便于前端展示生成时间
		"generatedAt": time.Now().Unix(),
	})
}
