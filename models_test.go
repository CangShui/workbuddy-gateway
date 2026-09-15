package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 验证官方目录条目的过滤规则：
// supportsExtra 补全模型与 image 图像模型不暴露，default/auto 官方路由条目保留。
func TestIsListableModel(t *testing.T) {
	cases := []struct {
		id            string
		supportsExtra bool
		want          bool
	}{
		{"deepseek-v4.1", false, true},
		{"glm-5.2", false, true},
		{"default", false, true},
		{"auto", false, true},
		{"hunyuan-image-v3.0", false, false},
		{"GLM-4.6V-Image", false, false},
		{"some-completion-model", true, false},
		{"", false, false},
	}
	for _, c := range cases {
		if got := isListableModel(c.id, c.supportsExtra); got != c.want {
			t.Errorf("isListableModel(%q, %v) = %v, want %v", c.id, c.supportsExtra, got, c.want)
		}
	}
}

// 验证 product.cloudhosted.json 的解析与过滤
func TestParseOfficialCatalog(t *testing.T) {
	sample := `{
	  "models": [
	    {"id": "default", "name": "Default"},
	    {"id": "deepseek-v4.1", "name": "DeepSeek-V4.1"},
	    {"id": "deepseek-v4-pro", "name": "Deepseek-V4-Pro"},
	    {"id": "hunyuan-image-v3.0", "name": "Hunyuan Image V3"},
	    {"id": "quick-completion", "name": "Quick", "supportsExtra": true}
	  ]
	}`
	models, err := parseOfficialCatalog([]byte(sample))
	if err != nil {
		t.Fatalf("parseOfficialCatalog 失败: %v", err)
	}
	got := make([]string, 0, len(models))
	for _, m := range models {
		got = append(got, m.ID)
	}
	want := []string{"default", "deepseek-v4.1", "deepseek-v4-pro"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("解析结果 = %v, want %v", got, want)
	}
}

// 验证目录解析结果为空时报错（防止坏包导致空列表覆盖静态兜底之外的数据）
func TestParseOfficialCatalogEmpty(t *testing.T) {
	if _, err := parseOfficialCatalog([]byte(`{"models": []}`)); err == nil {
		t.Fatal("空目录应返回错误")
	}
}

// 构造一个内存 tar.gz（npm 包布局），验证流式提取仅命中目标文件
func TestExtractCatalogFromTarGz(t *testing.T) {
	catalogJSON := `{"models":[{"id":"deepseek-v4.1","name":"DeepSeek-V4.1"},{"id":"hy4-preview","name":"HY4"}]}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	addFile := func(name, content string) {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	addFile("package/package.json", `{"name":"@tencent-ai/codebuddy-code"}`)
	addFile(catalogTarPath, catalogJSON)
	addFile("package/dist/noise.js", "should be skipped")
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	models, err := extractCatalogFromTarGz(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("extractCatalogFromTarGz 失败: %v", err)
	}
	if len(models) != 2 || models[0].ID != "deepseek-v4.1" || models[1].ID != "hy4-preview" {
		t.Fatalf("提取结果异常: %+v", models)
	}
}

// 验证 tarball 中缺少目录文件时报错
func TestExtractCatalogFromTarGzMissing(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "package/other.json", Mode: 0o644, Size: 2}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	_, _ = tw.Write([]byte("{}"))
	_ = tw.Close()
	_ = gz.Close()

	if _, err := extractCatalogFromTarGz(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("缺少目录文件应返回错误")
	}
}

// 验证动态目录与静态兜底的合并去重（动态在前、静态补充、跨来源去重）
func TestMergedModelIDs(t *testing.T) {
	oldModels, oldSource := modelsSnapshot()
	defer func() {
		modelsMu.Lock()
		dynamicModels = oldModels
		dynamicSource = oldSource
		modelsMu.Unlock()
	}()

	modelsMu.Lock()
	dynamicModels = []catalogModel{
		{ID: "deepseek-v4.1", Name: "DeepSeek-V4.1"},
		{ID: "deepseek-v4-pro", Name: "Deepseek-V4-Pro"}, // 与静态兜底重复，应去重
	}
	dynamicSource = "2.151.0"
	modelsMu.Unlock()

	ids, source := mergedModelIDs()
	if source != "2.151.0" {
		t.Fatalf("source = %q, want 2.151.0", source)
	}
	if len(ids) == 0 || ids[0] != "deepseek-v4.1" {
		t.Fatalf("动态模型应排在首位: %v", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("存在重复模型 id: %v", ids)
		}
		seen[id] = true
	}
	for _, fb := range staticFallbackModels {
		if !seen[fb] {
			t.Fatalf("静态兜底模型 %s 未包含在合并结果中: %v", fb, ids)
		}
	}
}

// 验证磁盘缓存写入后可重新加载
func TestModelsCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	saveModelsCache("2.151.0", []catalogModel{{ID: "deepseek-v4.1", Name: "DeepSeek-V4.1"}})
	if _, err := os.Stat(filepath.Join(dir, modelsCacheFile)); err != nil {
		t.Fatalf("缓存文件未写入: %v", err)
	}

	modelsMu.Lock()
	dynamicModels = nil
	dynamicSource = ""
	modelsMu.Unlock()

	loadModelsCache()
	models, source := modelsSnapshot()
	if source != "2.151.0" || len(models) != 1 || models[0].ID != "deepseek-v4.1" {
		t.Fatalf("缓存加载结果异常: source=%q models=%+v", source, models)
	}
}

// 验证缓存文件名不会命中凭据自动发现规则（workbuddy*.json 前缀误判）
func TestModelsCacheFileNotCredential(t *testing.T) {
	if isCredentialFile(modelsCacheFile) {
		t.Fatalf("缓存文件 %s 不应被识别为凭据文件", modelsCacheFile)
	}
	if isCredentialFile(modelsCacheFile + ".tmp") {
		t.Fatalf("缓存临时文件 %s 不应被识别为凭据文件", modelsCacheFile+".tmp")
	}
}
