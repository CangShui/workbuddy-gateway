package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDeploymentRejectsOtherWebsitesAndRebinding(t *testing.T) {
	old := cfg
	cfg.Addr, cfg.Port, cfg.APIKey = "127.0.0.1", 8317, "local-test-key-value"
	t.Cleanup(func() { cfg = old })
	for _, tc := range []struct {
		name, host, origin, authorization string
		status                            int
	}{
		{"native", "127.0.0.1:8317", "", "Bearer " + cfg.APIKey, 204},
		{"localhost", "localhost:8317", "", "Bearer " + cfg.APIKey, 204},
		{"same_gateway_browser", "127.0.0.1:8317", "http://127.0.0.1:8317", "Bearer " + cfg.APIKey, 204},
		{"untrusted_browser", "127.0.0.1:8317", "https://attacker.example", "Bearer " + cfg.APIKey, 403},
		{"opaque_browser_origin", "127.0.0.1:8317", "null", "Bearer " + cfg.APIKey, 403},
		{"rebound_hostname", "attacker.example:8317", "", "Bearer " + cfg.APIKey, 403},
		{"wrong_port", "localhost:8080", "", "Bearer " + cfg.APIKey, 403},
		{"missing_key", "127.0.0.1:8317", "", "", 401},
		{"raw_authorization_key", "127.0.0.1:8317", "", cfg.APIKey, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := localSecurityMiddleware(corsMiddleware(authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(204)
			}))))
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8317/v1/chat/completions", nil)
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Authorization", tc.authorization)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || called != (tc.status == 204) {
				t.Fatalf("status=%d handlerCalled=%v, want=%d", w.Code, called, tc.status)
			}
			if w.Header().Get("Access-Control-Allow-Origin") == "*" {
				t.Fatal("wildcard browser access was enabled")
			}
		})
	}
}

func TestLocalRedirectDoesNotSendRefreshTokenToOtherOrigin(t *testing.T) {
	for _, protected := range []bool{true, false} {
		t.Run(map[bool]string{true: "credential_protected", false: "public_catalog_allowed"}[protected], func(t *testing.T) {
			received := make(chan string, 1)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Get("X-Refresh-Token")
				w.WriteHeader(200)
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, http.StatusFound)
			}))
			defer origin.Close()
			client := &http.Client{CheckRedirect: safeUpstreamRedirect}
			r, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
			if protected {
				r.Header.Set("X-Refresh-Token", "SYNTHETIC_REFRESH_SECRET")
			}
			response, err := client.Do(r)
			if response != nil {
				response.Body.Close()
			}
			if protected {
				if err == nil {
					t.Fatal("credential redirect unexpectedly succeeded")
				}
				select {
				case <-received:
					t.Fatal("cross-origin target received a credential request")
				default:
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	u, _ := url.Parse("https://copilot.tencent.com/test")
	r, _ := http.NewRequest(http.MethodGet, "http://copilot.tencent.com/test", nil)
	if safeUpstreamRedirect(r, []*http.Request{{URL: u}}) == nil {
		t.Fatal("HTTPS downgrade was permitted")
	}
}

func TestLocalRequestSizeLimitCoversAllProtocolEntries(t *testing.T) {
	for _, handler := range []http.HandlerFunc{handleChatCompletions, handleResponses, handleMessages, handleCountTokens} {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
		r.ContentLength = maxClientRequestBytes + 1
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized payload status=%d", w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", io.LimitReader(repeatingSpaceReader{}, maxClientRequestBytes+1))
	r.ContentLength = -1
	_, err := readClientBody(httptest.NewRecorder(), r)
	var limit *http.MaxBytesError
	if !errors.As(err, &limit) {
		t.Fatalf("chunked payload escaped limit: %v", err)
	}
}

type repeatingSpaceReader struct{}

func (repeatingSpaceReader) Read(data []byte) (int, error) {
	for i := range data {
		data[i] = ' '
	}
	return len(data), nil
}

func TestLocalCredentialsAreReplacedOnlyAfterFullWrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "workbuddy.json")
	for _, value := range []string{"OLD_CREDENTIAL", "NEW_CREDENTIAL"} {
		if err := writeCredentialFile(path, []byte(value)); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if string(data) != value {
			t.Fatal("credential replacement did not complete")
		}
	}
	if err := writeCredentialFile(filepath.Join(path, "child.json"), []byte("FAIL")); err == nil {
		t.Fatal("invalid parent unexpectedly succeeded")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "NEW_CREDENTIAL" {
		t.Fatal("failed write damaged the existing credential")
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatal("credential temporary files were left behind")
	}
}

func TestLocalSecretFileAndLogsDoNotExposeCredentials(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })
	key := "SYNTHETIC_LOCAL_API_KEY_1234567890"
	path := filepath.Join(t.TempDir(), "api-key.txt")
	if err := os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.APIKey, cfg.APIKeyFile = "", path
	if err := loadGatewayAPIKey(); err != nil || cfg.APIKey != key {
		t.Fatalf("secret-file loading failed: %v", err)
	}
	refresh := "SYNTHETIC_REFRESH_KEY_1234567890"
	registerSecrets(refresh)
	var output bytes.Buffer
	writer := &redactingLogWriter{writer: &output}
	message := "api_key=" + key + " refreshToken=" + refresh
	if n, err := writer.Write([]byte(message)); err != nil || n != len(message) {
		t.Fatal("redacting log writer failed")
	}
	if strings.Contains(output.String(), key) || strings.Contains(output.String(), refresh) || strings.Contains(safeDebugError(errors.New(message)), refresh) {
		t.Fatal("credential leaked into logs")
	}
	if err := loadGatewayAPIKey(); err == nil {
		t.Fatal("ambiguous command-line and file keys were accepted")
	}
}

func TestLogFallbackStillRedactsSecrets(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(logDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	oldStderr, oldOutput := os.Stderr, log.Writer()
	defer func() { os.Stderr = oldStderr; log.SetOutput(oldOutput) }()
	os.Stderr = writer
	secret := "SYNTHETIC_FALLBACK_LOG_SECRET_123456"
	registerSecrets(secret)
	closeLog := initFileLogging("serve")
	log.Print("request failed with token " + secret)
	closeLog()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatal("fallback console logging did not redact the credential")
	}
}

type securityTestTransport func(*http.Request) (*http.Response, error)

func (transport securityTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

type contextBoundTestBody struct {
	ctx context.Context
	io.ReadCloser
}

func (body contextBoundTestBody) Read(data []byte) (int, error) {
	if err := body.ctx.Err(); err != nil {
		return 0, err
	}
	return body.ReadCloser.Read(data)
}

func TestNPMVersionReadsBodyBeforeCancelingRequest(t *testing.T) {
	oldClient := cfg.HttpClient
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	var requestContext context.Context
	cfg.HttpClient = &http.Client{Transport: securityTestTransport(func(r *http.Request) (*http.Response, error) {
		requestContext = r.Context()
		return &http.Response{StatusCode: 200, Body: contextBoundTestBody{
			ctx: r.Context(), ReadCloser: io.NopCloser(strings.NewReader(`{"version":"9.9.9"}`)),
		}}, nil
	})}
	version, err := fetchNPMCatalogVersion()
	if err != nil || version != "9.9.9" {
		t.Fatalf("version request failed before reading its body: version=%q err=%v", version, err)
	}
	if requestContext == nil || !errors.Is(requestContext.Err(), context.Canceled) {
		t.Fatal("completed request context was not released")
	}
}

func TestNPMTarballLimitsDecompressedScan(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	archive := tar.NewWriter(gz)
	paddingSize := int64(npmTarballMaxBytes) + 1
	if err := archive.WriteHeader(&tar.Header{Name: "package/padding", Mode: 0600, Size: paddingSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(archive, repeatingSpaceReader{}, paddingSize); err != nil {
		t.Fatal(err)
	}
	catalog := []byte(`{"models":[{"id":"synthetic-model","credits":"x0.1"}]}`)
	if err := archive.WriteHeader(&tar.Header{Name: "package/catalog.json", Mode: 0600, Size: int64(len(catalog))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(catalog); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	oldClient := cfg.HttpClient
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	cfg.HttpClient = &http.Client{Transport: securityTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(compressed.Len()), Body: io.NopCloser(bytes.NewReader(compressed.Bytes()))}, nil
	})}
	if _, err := fetchNPMCatalogTarball("https://catalog.example.test/package.tgz", "catalog.json"); err == nil {
		t.Fatal("catalog beyond the decompressed scan limit was accepted")
	}
}
