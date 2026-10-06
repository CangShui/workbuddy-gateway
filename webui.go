package main

import (
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WebUI 是只读管理台：浏览器静态外壳 + /admin/api/* 只读 JSON 接口。
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

// webUIPreflightError 校验管理台启动前置条件：启用时必须设置 API 密钥，
// 否则接口会暴露账号与额度信息。
func webUIPreflightError() error {
	if cfg.WebUI && cfg.APIKey == "" {
		return errors.New("启用 -webui 时必须同时设置 -api-key（管理台接口会暴露账号与额度信息）")
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
}

// webStatusResponse 是 /admin/api/status 的响应体。
type webStatusResponse struct {
	Gateway     webGatewayMeta            `json:"gateway"`
	UpdatedAt   int64                     `json:"updatedAt"`
	Summary     map[string]int            `json:"summary"`
	SiteSummary map[string]map[string]int `json:"siteSummary"`
	Accounts    []accountSnapshot         `json:"accounts"`
	Models      []modelStatSnapshot       `json:"models"`
}

// readStatusSnapshot 读取 serve 周期写入的状态快照（与 monitor 命令同一数据源）。
func readStatusSnapshot() (statusSnapshot, bool) {
	data, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		return statusSnapshot{}, false
	}
	var snap statusSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return statusSnapshot{}, false
	}
	return snap, true
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
	snap, _ := readStatusSnapshot()
	resp := webStatusResponse{
		Gateway: webGatewayMeta{
			Version:       version,
			Listen:        cfg.Addr + ":" + strconv.Itoa(cfg.Port),
			APIKeyEnabled: cfg.APIKey != "",
			WebUIEnabled:  webuiEnabled(),
			ModelSource:   modelSourceLabel(modelSourceFromSnapshots(snap.Models)),
		},
		UpdatedAt: snap.UpdatedAt,
		Accounts:  snap.Accounts,
		Models:    snap.Models,
	}
	if resp.Accounts == nil {
		resp.Accounts = []accountSnapshot{}
	}
	if resp.Models == nil {
		resp.Models = []modelStatSnapshot{}
	}
	resp.Summary = summarizeAccounts(resp.Accounts)
	resp.SiteSummary = summarizeAccountsBySite(resp.Accounts)
	writeJSON(w, http.StatusOK, resp)
}

func handleWebUIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	snap, _ := readStatusSnapshot()
	models := snap.Models
	if models == nil {
		models = []modelStatSnapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": modelSourceLabel(modelSourceFromSnapshots(models)),
		"models": models,
	})
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
		if e.IsDir() || !logFilePattern.MatchString(e.Name()) {
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
	name := baseNameSafe(r.URL.Query().Get("file"))
	if name != "" && !logFilePattern.MatchString(name) {
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

	resp := map[string]any{"files": files, "file": name, "lines": []string{}}
	if name != "" {
		if lines, err := tailLines(filepath.Join(logDir, name), linesN); err == nil {
			resp["lines"] = lines
		} else {
			resp["error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, resp)
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
	data, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"exists": false, "path": runtimeConfigFile, "content": "",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":  true,
		"path":    runtimeConfigFile,
		"content": string(data),
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
	writeJSON(w, http.StatusOK, map[string]any{
		"hint":  "令牌已脱敏，仅显示前缀；完整凭据仅保存在本地凭据文件中。",
		"files": views,
		// 保留字段名便于前端展示生成时间
		"generatedAt": time.Now().Unix(),
	})
}
