package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCatalog = `{"models":[
  {"id":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","supportsExtra":false},
  {"id":"glm-5.2","name":"GLM 5.2","supportsExtra":false},
  {"id":"helper-extra","supportsExtra":true},
  {"id":"image-generator","supportsExtra":false},
  {"id":"glm-5.2","name":"duplicate"}
]}`

func resetModelsState(t *testing.T) {
	t.Helper()
	modelsMu.Lock()
	dynamicModels = nil
	dynamicSource = ""
	failedVersions = map[string]modelFailure{}
	modelsMu.Unlock()
}

func TestParseOfficialCatalogFiltersAndValidates(t *testing.T) {
	models, err := parseOfficialCatalog([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 || models[0].ID != "deepseek-v4.1-flash" || models[1].ID != "glm-5.2" || models[2].ID != "helper-extra" || models[3].ID != "image-generator" {
		t.Fatalf("unexpected models: %+v", models)
	}
	for _, bad := range []string{`{}`, `{"models":[]}`, `{"models":"bad"}`} {
		if _, err := parseOfficialCatalog([]byte(bad)); err == nil {
			t.Fatalf("expected validation error for %s", bad)
		}
	}
}

func TestExtractCatalogFromTarGz(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "package/other.txt", Size: 1, Mode: 0600}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x"))
	if err := tw.WriteHeader(&tar.Header{Name: officialCatalogTar, Size: int64(len(testCatalog)), Mode: 0600}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(testCatalog))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	models, err := extractCatalogFromTarGz(bytes.NewReader(buf.Bytes()))
	if err != nil || len(models) != 4 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestReadLimitedRejectsOversize(t *testing.T) {
	if _, err := readLimited(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("expected oversize error")
	}
}

func TestFetchCatalogTarballRejectsContentLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "90000000")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	oldClient := modelsHTTPClient
	modelsHTTPClient = server.Client()
	defer func() { modelsHTTPClient = oldClient }()
	if _, err := fetchCatalogTarball(server.URL); err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func TestFetchOfficialCatalogPrefersSingleFile(t *testing.T) {
	var singleCalls, tarCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			singleCalls++
			_, _ = io.WriteString(w, testCatalog)
		default:
			tarCalls++
			http.Error(w, "should not download tgz", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	oldClient := modelsHTTPClient
	oldUnpkg := unpkgCatalogURL
	oldNPM := npmPackageURLs
	modelsHTTPClient = server.Client()
	unpkgCatalogURL = func(string) string { return server.URL + "/catalog.json" }
	npmPackageURLs = []string{server.URL + "/npm"}
	defer func() {
		modelsHTTPClient = oldClient
		unpkgCatalogURL = oldUnpkg
		npmPackageURLs = oldNPM
	}()
	models, transport, err := fetchOfficialCatalog("1.2.3")
	if err != nil || len(models) != 4 || transport != "unpkg" || singleCalls != 1 || tarCalls != 0 {
		t.Fatalf("models=%+v transport=%s single=%d tar=%d err=%v", models, transport, singleCalls, tarCalls, err)
	}
}

func TestModelsCacheRoundTripAndMerge(t *testing.T) {
	resetModelsState(t)
	oldDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	models := []catalogModel{{ID: "official-new"}, {ID: "glm-5.2"}}
	if err := saveModelsCache("9.9.9", models); err != nil {
		t.Fatal(err)
	}
	loadModelsCache()
	ids, source := mergedModelIDs()
	if source != "9.9.9" || ids[0] != "official-new" {
		t.Fatalf("source=%s ids=%v", source, ids)
	}
	count := 0
	for _, id := range ids {
		if id == "glm-5.2" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected merged dedupe, ids=%v", ids)
	}
	foundFlash := false
	for _, id := range ids {
		if id == "deepseek-v4.1-flash" {
			foundFlash = true
		}
		if id == "deepseek-v4.1" {
			t.Fatalf("invalid static model deepseek-v4.1 should not be listed: %v", ids)
		}
	}
	if !foundFlash {
		t.Fatalf("static deepseek-v4.1-flash missing: %v", ids)
	}
	data, err := os.ReadFile(filepath.Join(dir, modelsCacheFile))
	if err != nil {
		t.Fatal(err)
	}
	var cache modelsCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Schema != modelsCacheSchema || cache.Source != "9.9.9" {
		t.Fatalf("cache=%+v err=%v", cache, err)
	}
}

func TestModelRetryDelay(t *testing.T) {
	if modelRetryDelay(1) != 6*time.Hour || modelRetryDelay(2) != 12*time.Hour || modelRetryDelay(3) != 24*time.Hour || modelRetryDelay(99) != 24*time.Hour {
		t.Fatal("unexpected model retry delays")
	}
}
