package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const credentialEditBudget = 256 << 10

var errCredentialChanged = errors.New("凭据文件已被后台续期或其他操作更新，请重新加载后再保存，避免覆盖新令牌")

func credentialRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func openEditableCredential(target string, flag int) (*os.Root, *os.File, error) {
	allowed := false
	for _, configured := range collectConfiguredAuthPaths() {
		if target == configured {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, nil, gatewayValidationError("只能编辑当前配置加载的凭据文件，不能指定任意路径")
	}
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return nil, nil, errors.New("凭据目录不可访问")
	}
	info, err := root.Lstat(filepath.Base(target))
	if err != nil || !info.Mode().IsRegular() {
		root.Close()
		return nil, nil, gatewayValidationError("凭据文件不存在、不是普通文件或为符号链接")
	}
	f, err := root.OpenFile(filepath.Base(target), flag, 0600)
	if err != nil {
		root.Close()
		return nil, nil, errors.New("凭据文件不可读写，请检查文件权限")
	}
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		f.Close()
		root.Close()
		return nil, nil, errCredentialChanged
	}
	return root, f, nil
}

func readEditableCredential(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, credentialEditBudget+1))
	if err == nil && len(data) > credentialEditBudget {
		err = gatewayValidationError("凭据文件超过 256 KiB，不能使用网页编辑")
	}
	return data, err
}

func validateCredentialDocument(raw []byte) (StoredAuth, error) {
	var auth StoredAuth
	if len(raw) > credentialEditBudget || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return auth, gatewayValidationError("凭据必须是非空 JSON 对象，最大 256 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&auth); err != nil || ensureJSONEOF(dec) != nil {
		return auth, gatewayValidationError("凭据 JSON 格式或字段类型无效")
	}
	if strings.TrimSpace(auth.Auth.AccessToken) == "" || strings.Contains(auth.Auth.AccessToken, "****") || strings.Contains(auth.Auth.RefreshToken, "****") ||
		strings.Contains(auth.Auth.AccessToken, "[REDACTED]") || strings.Contains(auth.Auth.RefreshToken, "[REDACTED]") {
		return auth, gatewayValidationError("auth.accessToken 不能为空，不能保存脱敏后的令牌占位值")
	}
	if auth.Edition != "" && auth.Edition != "cn" && auth.Edition != "intl" {
		return auth, gatewayValidationError("edition 必须为空、cn 或 intl")
	}
	registerCredentialSecrets(&auth)
	return auth, nil
}

// 获取账号锁后重查池中的对象，避免热加载移出/重新加入账号时操作过期对象。
func lockCredentialAccount(ctx context.Context, target string) (*Account, func(), error) {
	for {
		accountMu.Lock()
		var acc *Account
		for _, item := range accounts {
			if item.Path == target {
				acc = item
				break
			}
		}
		accountMu.Unlock()
		if acc != nil && !lockAccountWithContext(ctx, &acc.lock) {
			return nil, nil, errors.New("等待账号当前请求结束时被取消，凭据未修改")
		}
		accountMu.Lock()
		var current *Account
		for _, item := range accounts {
			if item.Path == target {
				current = item
				break
			}
		}
		if current == acc {
			return acc, func() {
				accountMu.Unlock()
				if acc != nil {
					acc.lock.Unlock()
				}
			}, nil
		}
		accountMu.Unlock()
		if acc != nil {
			acc.lock.Unlock()
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
	}
}

func saveEditedCredential(ctx context.Context, target, revision, content, trace string) error {
	auth, err := validateCredentialDocument([]byte(content))
	if err != nil {
		return err
	}
	acc, release, err := lockCredentialAccount(ctx, target)
	if err != nil {
		return err
	}
	defer release()
	root, f, err := openEditableCredential(target, os.O_RDWR)
	if err != nil {
		return err
	}
	defer root.Close()
	defer f.Close()
	old, err := readEditableCredential(f)
	if err != nil {
		return err
	}
	if revision == "" || revision != credentialRevision(old) {
		return errCredentialChanged
	}
	if ctx.Err() != nil {
		return errors.New("请求已取消，凭据未修改")
	}
	if same, err := root.Stat(filepath.Base(target)); err != nil {
		return errCredentialChanged
	} else if opened, err := f.Stat(); err != nil || !os.SameFile(same, opened) {
		return errCredentialChanged
	}
	log.Printf("[凭据编辑] traceId=%s 阶段=保存 文件=%q 旧字节=%d 新字节=%d 说明=版本检查通过，仅记录文件与长度，绝不记录JSON正文或令牌", trace, target, len(old), len(content))
	write := func(data []byte) error {
		n, err := f.WriteAt(data, 0)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = f.Truncate(int64(len(data)))
		}
		if err == nil {
			err = f.Sync()
		}
		return err
	}
	if err := write([]byte(content)); err != nil {
		if restoreErr := write(old); restoreErr != nil {
			return errors.New("凭据保存失败且恢复旧文件失败，请检查服务器磁盘状态")
		}
		return errors.New("凭据保存失败，旧文件已恢复")
	}
	clearDisabledMarker(target)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	added := acc == nil
	if added {
		acc = &Account{Path: target}
		accounts = append(accounts, acc)
	}
	identityChanged := acc.Auth != nil && (acc.Auth.Account.UID != auth.Account.UID || acc.Profile().Key != profileForEdition(auth.Edition).Key)
	if identityChanged {
		acc.QuotaKnown, acc.QuotaExhausted, acc.IsPaidUser = false, false, false
		acc.QuotaTotal, acc.QuotaUsed, acc.QuotaRemaining = 0, 0, 0
		acc.PlanLabel, acc.PlanCheckedAt, acc.PlanStale = "", 0, true
		acc.ModelStates = nil
		acc.CooldownUntil, acc.CooldownMsg = time.Time{}, ""
	}
	acc.Auth, acc.Edition, acc.fingerprint = &auth, profileForEdition(auth.Edition).Key, accountFingerprint(info)
	acc.Disabled, acc.DisabledReason = false, ""
	log.Printf("[凭据编辑] traceId=%s 结果=保存成功 文件=%q 加入账号池=%t 账号身份已变更=%t 说明=令牌未进入日志，未调用上游验证；同一账号保留账本，身份或站点改变则重置对应运行状态", trace, target, added, identityChanged)
	return nil
}

func handleWebUICredentialEdit(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		target := r.URL.Query().Get("file")
		_, release, err := lockCredentialAccount(r.Context(), target)
		if err != nil {
			writeJSONError(w, http.StatusRequestTimeout, "等待账号请求结束时被取消")
			return
		}
		root, f, err := openEditableCredential(target, os.O_RDONLY)
		var data []byte
		if err == nil {
			data, err = readEditableCredential(f)
			f.Close()
			root.Close()
		}
		release()
		if err != nil {
			writeAdminOperationError(w, r, "凭据加载", err)
			return
		}
		var auth StoredAuth
		if json.Unmarshal(data, &auth) == nil {
			registerCredentialSecrets(&auth)
		}
		log.Printf("[凭据编辑] traceId=%s 阶段=加载 文件=%q 字节=%d 说明=管理员主动打开编辑器才返回明文，不记录正文，不在浏览器持久保存", debugTraceID(r), target, len(data))
		// 唯一受管理鉴权保护的明文凭据编辑接口，不使用脱敏视图覆盖可编辑原文。
		writeJSON(w, http.StatusOK, map[string]any{"path": target, "content": string(data), "revision": credentialRevision(data)})
	case http.MethodPost:
		var input struct {
			Path     string `json:"path"`
			Revision string `json:"revision"`
			Content  string `json:"content"`
		}
		if err := decodeAdminJSONLimit(w, r, &input, credentialEditBudget*2+4096); err != nil {
			log.Printf("[凭据编辑] traceId=%s 阶段=参数校验 结果=拒绝 原因=编辑请求格式或长度无效，正文未记录", debugTraceID(r))
			writeJSONError(w, http.StatusBadRequest, "编辑请求格式或长度无效")
			return
		}
		if err := saveEditedCredential(r.Context(), input.Path, input.Revision, input.Content, debugTraceID(r)); err != nil {
			writeAdminOperationError(w, r, "凭据保存", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": input.Path, "saved": true, "revision": credentialRevision([]byte(input.Content))})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}
