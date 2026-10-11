package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// 调用方持有 runtimeConfigMu；先校验、落盘，再应用，失败恢复旧配置。
func persistRuntimeDocument(doc map[string]json.RawMessage, old []byte, trace string) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	c, err := parseRuntimeConfig(data)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeConfigPreservingFile(data, old); err != nil {
		return err
	}
	if err := applyRuntimeConfig(c, true, trace); err != nil {
		if restoreErr := writeConfigPreservingFile(old, data); restoreErr != nil {
			return errors.New("配置应用失败且旧配置无法恢复，请检查服务器日志")
		}
		return err
	}
	runtimeAppliedHash, runtimeHasAppliedHash = sha256.Sum256(data), true
	return nil
}

func writeAdminOperationError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	status := http.StatusInternalServerError
	var validation gatewayValidationError
	if errors.As(err, &validation) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, errConfigChange) || errors.Is(err, errCredentialChanged) {
		status = http.StatusConflict
	}
	log.Printf("[管理操作失败] traceId=%s 操作=%s 状态码=%d 原因=%s 说明=未成功修改，页面应保留用户输入供修正或重试", debugTraceID(r), operation, status, safeDebugError(err))
	writeJSONError(w, status, safeDebugError(err))
}
