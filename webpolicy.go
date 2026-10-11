package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

func currentModelPolicyMode() string {
	modelFilterMu.RLock()
	defer modelFilterMu.RUnlock()
	if len(currentFilter.allowlist) > 0 {
		return "allowlist"
	}
	return "blocklist"
}

// 全局停用是调度策略，不复用授权失效字段，也不删除凭据或历史统计。
func annotateAccountPolicies(rows []accountSnapshot) {
	pool := make([]*Account, len(rows))
	for i := range rows {
		pool[i] = &Account{Path: rows[i].Path}
	}
	for i := range rows {
		enabled, reason := modelAccountAllowed("all", pool[i], pool)
		rows[i].SchedulingEnabled, rows[i].SchedulingReason = &enabled, reason
	}
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	return obj, nil
}

func sortedNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		if names[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// 用磁盘中最新配置做合并，而不是页面上可能过期的整份配置；保留无关配置段。
func savePolicyToggle(kind, target string, enabled bool, trace string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	gatewayConfigMu.Lock()
	doc, gateway, old, err := readGatewayDocumentLocked()
	if err == nil && (cfg.AdminKey == "" || gateway.AdminKey != cfg.AdminKey) {
		err = errConfigChange
	}
	gatewayConfigMu.Unlock()
	if err != nil {
		return err
	}
	c := runtimeFileConfig{}
	if len(old) > 0 {
		c, err = parseRuntimeConfig(old)
		if err != nil {
			return err
		}
	}
	models, err := rawObject(doc["models"])
	if err != nil {
		return err
	}
	// 从旧示例升级时移除未启用的模型名单，不再写入互相矛盾的两种字段。
	if hasModelNames(c.Models.Allowlist) {
		delete(models, "blocklist")
	} else {
		delete(models, "allowlist")
		if _, exists := models["blocklist"]; !exists {
			models["blocklist"] = json.RawMessage("[]")
		}
	}

	switch kind {
	case "account":
		accountMu.Lock()
		exists, duplicates := false, 0
		name := filepath.Base(target)
		accountNames := map[string]int{}
		for _, acc := range accounts {
			accountNames[filepath.Base(acc.Path)]++
			if acc.Path == target {
				exists = true
			}
			if filepath.Base(acc.Path) == name {
				duplicates++
			}
		}
		accountMu.Unlock()
		if !exists || duplicates != 1 {
			return gatewayValidationError("账号不存在或凭据文件名重复，不能设置全局开关")
		}
		// 只修改 all 的原始 JSON，保留模型专属规则的字段和 null/空值。
		accountRules, parseErr := rawObject(models["accounts"])
		if parseErr != nil {
			return parseErr
		}
		// 大小写兼容已有 ALL；删除旧拼写，保证只写一个 all。
		global := modelAccountFileConfig{}
		for id, rule := range c.Models.Accounts {
			if normalizeModelName(id) == "all" {
				global = rule
				delete(accountRules, id)
			}
		}
		blocks, allows := map[string]bool{}, map[string]bool{}
		for _, n := range global.Blocklist {
			blocks[strings.TrimSpace(n)] = true
		}
		for _, n := range global.Allowlist {
			allows[strings.TrimSpace(n)] = true
		}
		sourceMode := "blocklist"
		if len(allows) > 0 {
			sourceMode = "allowlist"
			// 将当前账号池中未获白名单放行的账号显式停用，
			// 不能仅删除白名单，否则会意外放开其他账号。
			for n := range accountNames {
				if !allows[n] {
					blocks[n] = true
				}
			}
		}
		if len(allows) > 0 || len(global.Blocklist) > 0 {
			for n, count := range accountNames {
				if count > 1 {
					blocks[n] = true // 保留规则启用时对重名凭据的拒绝状态。
				}
			}
		}
		if enabled {
			delete(blocks, name)
		} else {
			blocks[name] = true
		}
		finalBlocks := sortedNames(blocks)
		log.Printf("[账号开关规则] traceId=%s 阶段=转换全局账号名单 来源=%s 当前账号文件名数=%d 原白名单=%q 原黑名单=%q 目标=%q 启用=%t 最终黑名单=%q 说明=删除all.allowlist，仅写all.blocklist；其他当前账号全局状态保持，模型专属规则不变，之后新增账号默认开启",
			trace, sourceMode, len(accountNames), global.Allowlist, global.Blocklist, name, enabled, finalBlocks)
		accountRules["all"], err = json.Marshal(map[string][]string{"blocklist": finalBlocks})
		if err == nil {
			models["accounts"], err = json.Marshal(accountRules)
		}
	case "model":
		target = normalizeModelName(target)
		ids := managementModelIDs(c)
		if target == "" || !ids[target] {
			return gatewayValidationError("模型不在当前模型列表或已配置名单中")
		}
		blocks, allows := map[string]bool{}, map[string]bool{}
		for _, id := range c.Models.Blocklist {
			blocks[normalizeModelName(id)] = true
		}
		for _, id := range c.Models.Allowlist {
			allows[normalizeModelName(id)] = true
		}
		delete(blocks, "")
		delete(allows, "")
		if len(allows) > 0 {
			for id := range ids {
				if !allows[id] {
					blocks[id] = true
				}
			}
		}
		if enabled {
			delete(blocks, target)
		} else {
			blocks[target] = true
		}
		delete(models, "allowlist")
		models["blocklist"], err = json.Marshal(sortedNames(blocks))
	default:
		return errors.New("未定义的开关类型")
	}
	if err != nil {
		return err
	}
	doc["models"], err = json.Marshal(models)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	c, err = parseRuntimeConfig(data)
	if err != nil {
		return err
	}
	log.Printf("[管理开关] traceId=%s 阶段=配置落盘 类型=%s 目标=%q 目标启用=%t 说明=只修改目标规则，其他配置保留，凭据和统计不会删除", trace, kind, target, enabled)
	if err := writeConfigPreservingFile(append(data, '\n'), old); err != nil {
		return err
	}
	if err := applyRuntimeConfig(c, true, trace); err != nil {
		if restoreErr := writeConfigPreservingFile(old, data); restoreErr != nil {
			return fmt.Errorf("生效失败: %v；恢复配置失败: %w", err, restoreErr)
		}
		return err
	}
	runtimeAppliedHash, runtimeHasAppliedHash = sha256.Sum256(append(data, '\n')), true
	log.Printf("[管理开关] traceId=%s 结果=成功 类型=%s 目标=%q 启用=%t 说明=配置已保存且立即生效，已有上游请求不被强行中断", trace, kind, target, enabled)
	return nil
}

func managementModelIDs(c runtimeFileConfig) map[string]bool {
	ids := map[string]bool{}
	add := func(id string) {
		if id = normalizeModelName(id); id != "" {
			ids[id] = true
		}
	}
	catalog, _ := collectModelIDs(true)
	for _, id := range catalog {
		add(id)
	}
	for _, id := range c.Models.Allowlist {
		add(id)
	}
	for _, id := range c.Models.Blocklist {
		add(id)
	}
	for id := range c.Models.Accounts {
		if normalizeModelName(id) != "all" {
			add(id)
		}
	}
	modelStatsMu.Lock()
	for id := range modelStats {
		add(id)
	}
	modelStatsMu.Unlock()
	if snap, err := readStatusSnapshot(); err == nil {
		for _, row := range snap.Models {
			add(row.ID)
		}
		for _, row := range snap.ModelCatalog {
			add(row.ID)
		}
	}
	return ids
}

func handleWebUIAccountToggle(w http.ResponseWriter, r *http.Request) {
	handleWebUIPolicyToggle(w, r, "account")
}

func handleWebUIModelToggle(w http.ResponseWriter, r *http.Request) {
	handleWebUIPolicyToggle(w, r, "model")
}

func handleWebUIPolicyToggle(w http.ResponseWriter, r *http.Request, kind string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var input struct {
		Target  string `json:"target"`
		Enabled *bool  `json:"enabled"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil || input.Enabled == nil || strings.TrimSpace(input.Target) == "" {
		log.Printf("[管理开关] traceId=%s 阶段=参数校验 结果=拒绝 类型=%s 状态码=400 说明=必须提供目标与布尔开关，未修改配置", debugTraceID(r), kind)
		writeJSONError(w, http.StatusBadRequest, "必须提供 target 字符串和 enabled 布尔值")
		return
	}
	log.Printf("[管理开关] traceId=%s 阶段=参数校验 类型=%s 目标=%q 启用=%t 结果=通过", debugTraceID(r), kind, input.Target, *input.Enabled)
	if err := savePolicyToggle(kind, input.Target, *input.Enabled, debugTraceID(r)); err != nil {
		status := http.StatusInternalServerError
		var validation gatewayValidationError
		if errors.As(err, &validation) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, errConfigChange) {
			status = http.StatusConflict
		}
		log.Printf("[管理开关] traceId=%s 结果=失败 类型=%s 状态码=%d 原因=%s 说明=未成功修改，页面须保留原开关", debugTraceID(r), kind, status, safeDebugError(err))
		writeJSONError(w, status, safeDebugError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": input.Target, "enabled": *input.Enabled, "saved": true})
}
