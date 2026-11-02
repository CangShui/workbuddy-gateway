package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withWebSearchUpstream(t *testing.T, acc *Account, handler http.Handler) {
	t.Helper()
	chdirTemp(t)
	server := httptest.NewServer(handler)
	oldBase, oldOrigin := profileCN.Base, profileCN.Origin
	oldClient := cfg.HttpClient
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()
	profileCN.Base, profileCN.Origin = server.URL, server.URL
	cfg.HttpClient = server.Client()
	t.Cleanup(func() {
		server.Close()
		profileCN.Base, profileCN.Origin = oldBase, oldOrigin
		cfg.HttpClient = oldClient
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
}

func webSearchTestAccount() *Account {
	return &Account{Path: "workbuddy-cn.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "token-abc", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account: StoredAccount{UID: "uid-1"},
	}}
}

func TestHandleWebSearchMapsDSHFormat(t *testing.T) {
	var upstreamBody map[string]any
	withWebSearchUpstream(t, webSearchTestAccount(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != webSearchAPIPath {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &upstreamBody)
		_, _ = w.Write([]byte(`{"results":[{"title":"Go","url":"https://go.dev","snippet":"Language","site":"go.dev"}]}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"golang","count":8,"need_content":false,"ori_question":"Go language"}`))
	rec := httptest.NewRecorder()
	handleWebSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamBody["query"] != "golang" || upstreamBody["type"] != "text2text" || upstreamBody["max_results"] != float64(8) || len(upstreamBody) != 3 {
		t.Fatalf("upstream payload=%v", upstreamBody)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	results, ok := response["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("response results=%v", response["results"])
	}
	item := results[0].(map[string]any)
	if item["url"] != "https://go.dev" || item["title"] != "Go" || item["snippet"] != "Language" || item["date"] != nil {
		t.Fatalf("DSH result shape=%v", item)
	}
	if len(response) != 1 || len(item) != 4 {
		t.Fatalf("unexpected fields in DSH response: %v", response)
	}
}

func TestHandleWebFetchReturnsDSHFormat(t *testing.T) {
	withWebSearchUpstream(t, webSearchTestAccount(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != webFetchAPIPath {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"url":"https://go.dev","content":"# Go\n\nLanguage"}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", strings.NewReader(`{"url":"https://go.dev"}`))
	rec := httptest.NewRecorder()
	handleWebFetch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["content"] != "# Go\n\nLanguage" || len(response) != 1 {
		t.Fatalf("DSH fetch response=%v", response)
	}
}
