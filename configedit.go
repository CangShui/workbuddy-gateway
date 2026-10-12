package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"unicode/utf8"
)

const configEditBudget = 1 << 20

func configRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// 仅编辑固定的 config.json；不接受客户端指定路径，也不跟随符号链接。
// 调用方持有 runtimeConfigMu，与网页设置、名单开关和配置热加载串行。
func readConfigEditorDocument() ([]byte, error) {
	info, err := os.Lstat(runtimeConfigFile)
	if err != nil {
		return nil, errors.New("config.json 不可读取，请检查文件是否存在和文件权限")
	}
	if !info.Mode().IsRegular() {
		return nil, gatewayValidationError("config.json 必须是普通文件，网页编辑不支持符号链接或目录")
	}
	f, err := os.Open(runtimeConfigFile)
	if err != nil {
		return nil, errors.New("config.json 不可读取，请检查文件权限")
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		return nil, errConfigChange
	}
	data, err := io.ReadAll(io.LimitReader(f, configEditBudget+1))
	if err != nil {
		return nil, errors.New("config.json 读取失败，未返回配置原文")
	}
	if len(data) > configEditBudget || !utf8.Valid(data) {
		return nil, gatewayValidationError("网页编辑仅支持最大 1 MiB 的 UTF-8 配置文件")
	}
	return data, nil
}

func validateConfigEditorDocument(content string) (runtimeFileConfig, error) {
	if len(content) > configEditBudget || !utf8.ValidString(content) {
		return runtimeFileConfig{}, gatewayValidationError("配置必须是最大 1 MiB 的 UTF-8 JSON 对象")
	}
	c, err := parseRuntimeConfig([]byte(content))
	if err != nil {
		// 解析错误可能携带用户输入的字段名或值，不把其原文写入响应或审计日志。
		return c, gatewayValidationError("配置校验失败：必须是单个 JSON 对象；字段名、类型、端口、日志天数及账号/模型名单须符合配置规则")
	}
	for _, key := range []string{c.Gateway.AdminKey, c.Gateway.APIKey} {
		if key == "[REDACTED]" {
			return c, gatewayValidationError("不能把脱敏占位符 [REDACTED] 保存为 Key，请重新加载编辑原文")
		}
	}
	return c, nil
}

func handleWebUIConfigEdit(w http.ResponseWriter, r *http.Request) {
	trace := debugTraceID(r)
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
		return
	}
	var input struct {
		Revision               string `json:"revision"`
		Content                string `json:"content"`
		Acknowledged           bool   `json:"acknowledged"`
		ConfirmAdminKeyRemoval bool   `json:"confirmAdminKeyRemoval"`
	}
	if r.Method == http.MethodGet {
		input.Acknowledged = r.URL.Query().Get("acknowledged") == "1"
	} else if err := decodeAdminJSONLimit(w, r, &input, configEditBudget*6+4096); err != nil {
		// JSON 字符串中的转义字符可能扩大请求长度；配置原文仍单独限制 1 MiB。
		log.Printf("[配置编辑] traceId=%s 阶段=参数校验 结果=拒绝 状态码=400 原因=请求格式或长度无效 正文已省略=true", trace)
		writeJSONError(w, http.StatusBadRequest, "配置编辑请求格式或长度无效")
		return
	}
	if !input.Acknowledged {
		log.Printf("[配置编辑] traceId=%s 阶段=风险确认 结果=拒绝 状态码=400 说明=未确认编辑警告，不返回明文、不写入配置", trace)
		writeJSONError(w, http.StatusBadRequest, "请先确认配置编辑风险警告")
		return
	}
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	// 请求可能在等待配置锁时遇到管理 Key 轮换，锁内必须再次校验。
	key := currentAdminKey()
	if key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) != 1 {
		log.Printf("[配置编辑] traceId=%s 阶段=锁内鉴权 结果=拒绝 状态码=401 说明=管理Key已变化，未读取或写入配置", trace)
		writeJSONError(w, http.StatusUnauthorized, "管理 Key 已变化，请重新登录")
		return
	}
	old, err := readConfigEditorDocument()
	if err != nil {
		writeAdminOperationError(w, r, "配置编辑加载", err)
		return
	}
	// 有效磁盘配置已修改管理 Key、但热加载尚未执行时，不能用旧会话覆盖它。
	if onDisk, err := parseRuntimeConfig(old); err == nil && onDisk.Gateway.AdminKey != key {
		writeAdminOperationError(w, r, "配置编辑", errConfigChange)
		return
	}
	if r.Method == http.MethodGet {
		log.Printf("[配置编辑] traceId=%s 阶段=加载 结果=成功 字节=%d 风险已确认=true 说明=仅此管理接口返回可编辑原文，配置正文及Key不进入日志、不在浏览器持久保存", trace, len(old))
		writeJSON(w, http.StatusOK, map[string]any{
			"path": runtimeConfigFile, "content": string(old), "revision": configRevision(old),
		})
		return
	}
	if input.Revision == "" || input.Revision != configRevision(old) {
		log.Printf("[配置编辑] traceId=%s 阶段=版本校验 结果=拒绝 状态码=409 说明=配置已被其他操作修改，未覆盖新内容", trace)
		writeJSONError(w, http.StatusConflict, "配置已被其他页面或服务器编辑，请重新加载后再保存；当前修改未写入")
		return
	}
	c, err := validateConfigEditorDocument(input.Content)
	if err != nil {
		writeAdminOperationError(w, r, "配置编辑校验", err)
		return
	}
	adminKeyChanged := c.Gateway.AdminKey != key
	if c.Gateway.AdminKey == "" && !input.ConfirmAdminKeyRemoval {
		writeAdminOperationError(w, r, "配置编辑校验", gatewayValidationError("清空 gateway.adminKey 会使管理登录不可用，必须额外确认；可在服务器本机重新设置"))
		return
	}
	if r.Context().Err() != nil {
		log.Printf("[配置编辑] traceId=%s 阶段=保存前检查 结果=取消 说明=请求已取消，配置未写入", trace)
		writeJSONError(w, http.StatusRequestTimeout, "请求已取消，配置未写入")
		return
	}
	// 外部编辑不受进程内配置锁约束；完整校验后再次核对磁盘，
	// 避免校验较大配置期间发生的服务器编辑被覆盖。
	current, err := readConfigEditorDocument()
	if err != nil {
		writeAdminOperationError(w, r, "配置编辑保存前检查", err)
		return
	}
	if configRevision(current) != input.Revision {
		log.Printf("[配置编辑] traceId=%s 阶段=保存前版本复查 结果=拒绝 状态码=409 说明=校验期间磁盘配置已变化，未覆盖新内容", trace)
		writeJSONError(w, http.StatusConflict, "配置在校验期间被修改，请重新加载后再保存；当前修改未写入")
		return
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal([]byte(input.Content), &doc) // 已由完整配置校验保证单个对象和字段类型。
	log.Printf("[配置编辑] traceId=%s 阶段=保存 旧字节=%d 新字节=%d 管理Key变化=%t API鉴权启用=%t 配置API端口=%d 配置Web端口=%d 说明=校验通过，先落盘再热应用；不记录JSON正文或Key", trace, len(old), len(input.Content), adminKeyChanged, c.Gateway.APIKeyEnabled, c.Gateway.APIPort, c.Gateway.WebPort)
	if err := persistRuntimeDocument(doc, old, trace); err != nil {
		log.Printf("[配置编辑] traceId=%s 阶段=落盘或应用 结果=失败 异常类型=%T 原因=%s 正文已省略=true", trace, err, safeDebugError(err))
		writeAdminOperationError(w, r, "配置编辑保存", errors.New("配置保存或应用失败，请检查文件权限及服务器日志；未完成更新"))
		return
	}
	settings := gatewaySettingsView()
	log.Printf("[配置编辑] traceId=%s 结果=保存成功 管理Key变化=%t 管理Key清空=%t 需重启=%t 说明=后续请求使用新配置；当前监听端口及已有请求不变，原文未记录", trace, adminKeyChanged, c.Gateway.AdminKey == "", settings["restartRequired"])
	writeJSON(w, http.StatusOK, map[string]any{
		"saved": true, "revision": hex.EncodeToString(runtimeAppliedHash[:]),
		"adminKeyChanged": adminKeyChanged, "adminKeyCleared": c.Gateway.AdminKey == "",
		"settings": settings,
	})
}
