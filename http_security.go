package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Local deployment hardening. No change to provider selection or model payloads.
const maxClientRequestBytes int64 = 32 << 20

var sensitiveSecrets sync.Map
var namedSecretPattern = regexp.MustCompile(`(?i)("?(?:access[_-]?token|refresh[_-]?token|api[_-]?key|x-refresh-token|authorization)"?\s*[:=]\s*"?)([^"\s,;}]+)`)

func registerSecrets(values ...string) {
	for _, value := range values {
		if len(value) >= 8 {
			sensitiveSecrets.Store(value, struct{}{})
		}
	}
}

func registerCredentialSecrets(auth *StoredAuth) {
	if auth != nil {
		registerSecrets(auth.Auth.AccessToken, auth.Auth.RefreshToken)
	}
}

func redactSensitiveText(text string) string {
	sensitiveSecrets.Range(func(key, _ any) bool {
		text = strings.ReplaceAll(text, key.(string), "[REDACTED]")
		return true
	})
	text = bearerLikeRe.ReplaceAllString(text, "[REDACTED]")
	return namedSecretPattern.ReplaceAllString(text, "${1}[REDACTED]")
}

type redactingLogWriter struct{ writer io.Writer }

func (w *redactingLogWriter) Write(data []byte) (int, error) {
	_, err := io.WriteString(w.writer, redactSensitiveText(string(data)))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func loadGatewayAPIKey() error {
	if cfg.APIKeyFile != "" {
		if cfg.APIKey != "" {
			return errors.New("-api-key 与 -api-key-file 不能同时使用")
		}
		file, err := os.Open(cfg.APIKeyFile)
		if err != nil {
			return fmt.Errorf("读取网关密钥文件失败: %w", err)
		}
		defer file.Close()
		data, err := readLimited(file, 4096)
		if err != nil {
			return errors.New("网关密钥文件无法读取或超过 4096 字节")
		}
		cfg.APIKey = strings.TrimSpace(string(data))
		if cfg.APIKey == "" {
			return errors.New("网关密钥文件为空")
		}
	}
	registerSecrets(cfg.APIKey)
	return nil
}

func validGatewayKey(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(cfg.APIKey)) == 1
}

func writeCredentialFile(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".workbuddy-auth-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	defer temp.Close()
	if err := temp.Chmod(0600); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// Go's default redirect policy does not strip custom X-Refresh-Token headers.
func safeUpstreamRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("上游重定向次数超过上限")
	}
	if len(via) == 0 {
		return nil
	}
	original := via[0]
	if original.URL.Scheme == "https" && request.URL.Scheme != "https" {
		return errors.New("拒绝上游 HTTPS 降级重定向")
	}
	hasSecret := false
	for _, header := range []string{"Authorization", "X-Refresh-Token", "X-Api-Key"} {
		hasSecret = hasSecret || original.Header.Get(header) != ""
	}
	if hasSecret && (request.URL.Scheme != original.URL.Scheme || !strings.EqualFold(request.URL.Host, original.URL.Host)) {
		return errors.New("拒绝携带凭据的跨来源重定向")
	}
	return nil
}

func readClientBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.ContentLength > maxClientRequestBytes {
		w.Header().Set("Connection", "close")
		return nil, &http.MaxBytesError{Limit: maxClientRequestBytes}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxClientRequestBytes)
	return io.ReadAll(r.Body)
}

func clientBodyErrorStatus(err error) int {
	var limitError *http.MaxBytesError
	if errors.As(err, &limitError) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func isLoopbackName(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLocalGatewayHost(host string) bool {
	name, port, err := net.SplitHostPort(host)
	return err == nil && isLoopbackName(name) && port == strconv.Itoa(cfg.Port)
}

func allowedBrowserOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "http" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && isLocalGatewayHost(u.Host)
}

func localSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackName(cfg.Addr) && !isLocalGatewayHost(r.Host) {
			writeUpstreamError(w, r, http.StatusForbidden, "invalid_host", "网关仅接受本机地址")
			return
		}
		next.ServeHTTP(w, r)
	})
}
