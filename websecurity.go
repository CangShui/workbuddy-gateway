package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func newWebUIHandler() http.Handler {
	if !cfg.WebUI {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	registerWebUIRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	return requestAuditMiddleware(adminSecurityMiddleware(adminAuthMiddleware(mux)))
}

func adminSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		reject := func(reason string) {
			log.Printf("[管理请求拦截] traceId=%s 层=管理来源校验 原因=%s 状态码=403 业务影响=请求未进入管理业务", debugTraceID(r), reason)
			writeJSONError(w, http.StatusForbidden, reason)
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") ||
				!strings.EqualFold(u.Host, r.Host) || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				reject("管理界面只接受同源请求")
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			reject("拒绝跨站管理请求")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
			if r.Header.Get("X-Workbuddy-Admin") != "1" || !strings.EqualFold(contentType, "application/json") {
				reject("管理写操作必须由同源 JSON 请求提交")
				return
			}
		}
		log.Printf("[管理来源校验] traceId=%s 结果=通过 说明=不向其他来源开放管理CORS", debugTraceID(r))
		next.ServeHTTP(w, r)
	})
}

func adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/admin/api/") || r.URL.Path == "/admin/api/setup" {
			next.ServeHTTP(w, r)
			return
		}
		key := currentAdminKey()
		header := r.Header.Get("Authorization")
		token := ""
		if strings.HasPrefix(header, "Bearer ") {
			token = strings.TrimPrefix(header, "Bearer ")
		}
		if key == "" || subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
			log.Printf("[管理请求拦截] traceId=%s 层=管理鉴权 状态码=401 业务影响=请求未进入业务方法 管理Key已配置=%t", debugTraceID(r), key != "")
			writeJSONError(w, http.StatusUnauthorized, "请使用管理 Key 登录，不接受模型 API Key")
			return
		}
		log.Printf("[管理鉴权] traceId=%s 结果=通过 说明=管理Key与模型API鉴权独立", debugTraceID(r))
		next.ServeHTTP(w, r)
	})
}

// 不信任代理声明的客户端 IP；初始化只能在回环连接和回环 Host 上完成。
func localAdminSetupRequest(r *http.Request) bool {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(remote).IsLoopback() {
		return false
	}
	u, err := url.Parse("http://" + r.Host)
	if err != nil || u.User != nil {
		return false
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
		return false
	}
	return r.Header.Get("Forwarded") == "" && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Real-IP") == ""
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10) // 仅小型管理表单，不影响模型请求。
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureJSONEOF(dec)
}

func handleWebUISetup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"setupRequired": currentAdminKey() == "", "localSetupOnly": true,
			"canInitialize": localAdminSetupRequest(r),
		})
	case http.MethodPost:
		if !localAdminSetupRequest(r) {
			log.Printf("[管理初始化] traceId=%s 结果=拒绝 原因=非本机直接访问 状态码=403 业务影响=未生成或写入管理Key", debugTraceID(r))
			writeJSONError(w, http.StatusForbidden,
				"首次自动生成管理 Key 仅允许本机访问。可选方式：① 在服务器本机打开本页面；"+
					"② 使用 SSH 本地端口转发（如 ssh -L 8316:127.0.0.1:8316 用户@服务器）后访问；"+
					"③ 直接在服务器上编辑 config.json，把 gateway.adminKey 设为随机字符串后重启网关")
			return
		}
		var empty map[string]json.RawMessage
		if err := decodeAdminJSON(w, r, &empty); err != nil || empty == nil || len(empty) != 0 {
			log.Printf("[管理初始化] traceId=%s 结果=拒绝 原因=请求JSON无效 状态码=400", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "初始化请求必须是空 JSON 对象")
			return
		}
		log.Printf("[管理初始化] traceId=%s 阶段=开始 说明=只生成一次32字符随机管理Key，原文不进入日志", debugTraceID(r))
		key, err := initializeAdminKey(debugTraceID(r))
		if err != nil {
			status := http.StatusInternalServerError
			message := "管理 Key 保存失败，未完成初始化，请检查服务器日志和配置文件权限"
			if errors.Is(err, errSetupDone) {
				status, message = http.StatusConflict, errSetupDone.Error()
			}
			log.Printf("[管理初始化] traceId=%s 结果=失败 状态码=%d 原因=%v", debugTraceID(r), status, err)
			writeJSONError(w, status, message)
			return
		}
		log.Printf("[管理初始化] traceId=%s 结果=成功 Key字符数=32 说明=已落盘，仅本次初始化响应返回Key", debugTraceID(r))
		// 有意仅此一次返回原文，满足页面弹窗；其他管理接口均不返回秘密值。
		writeJSON(w, http.StatusCreated, map[string]string{"adminKey": key})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

func handleWebUISettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		log.Printf("[管理配置] traceId=%s 阶段=读取 结果=成功 说明=仅返回鉴权开关、Key是否已配置及实际监听端口", debugTraceID(r))
		writeJSON(w, http.StatusOK, gatewaySettingsView())
	case http.MethodPost:
		var input struct {
			Enabled *bool  `json:"apiKeyEnabled"`
			Key     string `json:"apiKey"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil || input.Enabled == nil {
			log.Printf("[管理配置] traceId=%s 阶段=参数校验 结果=拒绝 状态码=400 原因=必须提供有效API鉴权开关和JSON", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "必须提供 apiKeyEnabled 布尔值；apiKey 为可选字符串")
			return
		}
		log.Printf("[管理配置] traceId=%s 阶段=参数校验 API鉴权目标=%t 新Key已提供=%t 说明=Key原文不记录", debugTraceID(r), *input.Enabled, input.Key != "")
		if err := saveAPIKeySettings(*input.Enabled, input.Key, debugTraceID(r)); err != nil {
			log.Printf("[管理配置] traceId=%s 结果=失败 原因=%v 业务影响=当前运行时鉴权状态保留", debugTraceID(r), err)
			status := http.StatusInternalServerError
			var validation gatewayValidationError
			if errors.As(err, &validation) {
				status = http.StatusBadRequest
			}
			if errors.Is(err, errConfigChange) {
				status = http.StatusConflict
			}
			writeJSONError(w, status, safeDebugError(err))
			return
		}
		writeJSON(w, http.StatusOK, gatewaySettingsView())
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

// 将前端校验/请求结果以有限的结构化字段落盘，不接受正文、Key 或任意日志字符串。
func handleWebUIClientEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input struct {
		Event  string `json:"event"`
		Route  string `json:"route"`
		Status int    `json:"status"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "无效的前端审计事件")
		return
	}
	allowed := map[string]bool{"request_completed": true, "request_failed": true, "settings_validation_failed": true}
	routes := map[string]bool{
		"/admin/api/status": true, "/admin/api/models": true, "/admin/api/logs": true,
		"/admin/api/config": true, "/admin/api/credentials": true, "/admin/api/settings": true,
	}
	if !allowed[input.Event] || !routes[input.Route] || input.Status < 0 || input.Status > 599 {
		writeJSONError(w, http.StatusBadRequest, "不接受未定义的前端审计事件")
		return
	}
	log.Printf("[管理前端] traceId=%s 事件=%s 接口=%s 状态码=%d 说明=前端请求或参数校验结果已落盘，不含Key与业务正文",
		debugTraceID(r), input.Event, input.Route, input.Status)
	w.WriteHeader(http.StatusNoContent)
}

// 只脱敏输出副本，不能修改用于业务判断的原始状态或磁盘文件。
func writeAdminData(w http.ResponseWriter, r *http.Request, value any) {
	raw, err := json.Marshal(value)
	var copyValue any
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err = decoder.Decode(&copyValue)
	}
	if err != nil {
		log.Printf("[管理响应] traceId=%s 阶段=编码 结果=失败 原因=%v", debugTraceID(r), err)
		writeJSONError(w, http.StatusInternalServerError, "管理数据编码失败")
		return
	}
	writeJSON(w, http.StatusOK, redactDebugValue(copyValue))
}
