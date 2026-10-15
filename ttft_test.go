package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func firstTokenChunk(t *testing.T, delta string) map[string]any {
	t.Helper()
	var chunk map[string]any
	if err := json.Unmarshal([]byte(`{"choices":[{"delta":`+delta+`}]}`), &chunk); err != nil {
		t.Fatal(err)
	}
	return chunk
}

func TestTTFTDeltaDefinition(t *testing.T) {
	for _, tc := range []struct {
		delta string
		want  bool
	}{
		{`{"role":"assistant"}`, false},
		{`{"content":"","reasoning_content":""}`, false},
		{`{"content":null}`, false},
		{`{"content":27}`, false},
		{`{"content":" "}`, true},
		{`{"content":"hello"}`, true},
		{`{"reasoning_content":"thinking"}`, true},
		{`{"reasoning":"thinking"}`, true},
		{`{"tool_calls":[{"index":0,"id":"call-1","type":"function"}]}`, false},
		{`{"tool_calls":[{"function":{"arguments":""}}]}`, false},
		{`{"tool_calls":[{"function":{"name":"shell"}}]}`, true},
		{`{"tool_calls":[{"function":{"arguments":"{"}}]}`, true},
		{`{"function_call":{"name":"legacy"}}`, true},
	} {
		if got := hasFirstTokenDelta(firstTokenChunk(t, tc.delta)); got != tc.want {
			t.Fatalf("delta=%s got=%v", tc.delta, got)
		}
	}
	for _, raw := range []string{`{}`, `{"usage":{"total_tokens":4}}`, `{"choices":[{"finish_reason":"stop"}]}`, `{"choices":[null]}`} {
		var c map[string]any
		_ = json.Unmarshal([]byte(raw), &c)
		if hasFirstTokenDelta(c) {
			t.Fatalf("control frame accepted: %s", raw)
		}
	}
}

// 每条协议响应路径都实际消费分阶段SSE：先角色/心跳，再Token；确保计时不是首字节。
// 六路径+纯控制流/截断流，验证日志、CLI模型快照与持久化样本用同一个duration。
func TestTTFTAllResponsePaths(t *testing.T) {
	handlers := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request, *http.Response, *Account, time.Time)
	}{
		{"chat_stream", func(w http.ResponseWriter, r *http.Request, resp *http.Response, a *Account, s time.Time) {
			streamChatResponse(w, r, resp, "metric-model", 1, a, &profileCN, s)
		}},
		{"chat_aggregate", func(w http.ResponseWriter, r *http.Request, resp *http.Response, a *Account, s time.Time) {
			writeChatAggregate(w, r, resp, "metric-model", 1, a, &profileCN, s)
		}},
		{"responses_stream", func(w http.ResponseWriter, r *http.Request, resp *http.Response, a *Account, s time.Time) {
			streamResponsesResponse(w, r, resp, "metric-model", 1, a, &profileCN, s)
		}},
		{"responses_aggregate", func(w http.ResponseWriter, r *http.Request, resp *http.Response, a *Account, s time.Time) {
			writeResponsesAggregate(w, r, resp, "metric-model", 1, a, &profileCN, s)
		}},
		{"messages_stream", func(w http.ResponseWriter, r *http.Request, resp *http.Response, a *Account, s time.Time) {
			streamMessagesResponse(w, r, resp, "metric-model", 1, a, &profileCN, s, true)
		}},
		{"messages_aggregate", func(w http.ResponseWriter, r *http.Request, resp *http.Response, a *Account, s time.Time) {
			writeMessagesAggregate(w, r, resp, "metric-model", 1, a, &profileCN, s, true)
		}},
	}
	for _, handler := range handlers {
		for _, mode := range []string{"text", "reasoning", "tool", "control_only", "truncated"} {
			t.Run(handler.name+"/"+mode, func(t *testing.T) {
				isolatedGatewayConfig(t)
				resetStatsState(t)
				loadOptionalStats()
				modelStatsMu.Lock()
				old := modelStats
				modelStats = map[string]*modelStat{}
				modelStatsMu.Unlock()
				t.Cleanup(func() { modelStatsMu.Lock(); modelStats = old; modelStatsMu.Unlock() })
				start := time.Now().Add(-40 * time.Millisecond)
				r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				ensureDebugRequestContext(r, "ttft-test", start)
				p := &statsAttempt{id: "test", account: statsAccount{File: "synthetic.json"}, model: "metric-model", started: start}
				r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, p))
				pipeR, pipeW := io.Pipe()
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer pipeW.Close()
					fmt.Fprint(pipeW, ": heartbeat\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
					time.Sleep(25 * time.Millisecond)
					delta := `{"content":"test content"}`
					if mode == "reasoning" {
						delta = `{"reasoning_content":"test reasoning"}`
					}
					if mode == "tool" {
						delta = `{"tool_calls":[{"index":0,"id":"id1","function":{"name":"shell","arguments":"{}"}}]}`
					}
					if mode != "control_only" {
						fmt.Fprintf(pipeW, "data: {\"choices\":[{\"delta\":%s}]}\n\n", delta)
					}
					if mode != "truncated" {
						fmt.Fprint(pipeW, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7,\"credit\":0.1}}\n\ndata: [DONE]\n\n")
					}
				}()
				rec := httptest.NewRecorder()
				handler.fn(rec, r, &http.Response{Body: pipeR}, &Account{Path: "synthetic.json"}, start)
				<-done
				statsFinishAttempt(r, rec.Code)
				requestStats.Lock()
				rows := append([]statsRequest(nil), requestStats.document.Requests...)
				requestStats.Unlock()
				if len(rows) != 1 {
					t.Fatalf("rows=%d", len(rows))
				}
				row := rows[0]
				valid := mode != "control_only" && mode != "truncated"
				if (row.TTFTMS != nil) != valid || row.TTFTMetric != ttftMetric {
					t.Fatalf("invalid metric %+v", row)
				}
				modelStatsMu.Lock()
				st := modelStats["metric-model"]
				var samples int64
				var sum time.Duration
				if st != nil {
					for _, b := range st.Buckets {
						samples += b.TTFTSamples
						sum += b.TTFTSum
					}
				}
				modelStatsMu.Unlock()
				if valid {
					if *row.TTFTMS < 60 || samples != 1 || math.Abs(*row.TTFTMS-durationMilliseconds(sum)) > 0.001 {
						t.Fatalf("first-byte or duplicate/mismatched metric: %v samples=%d sum=%v", *row.TTFTMS, samples, sum)
					}
				} else if samples != 0 {
					t.Fatalf("incomplete/control only must not enter average: %d", samples)
				}
				files, _ := os.ReadDir(logDir)
				found := false
				for _, f := range files {
					if !strings.HasPrefix(f.Name(), "gateway-") {
						continue
					}
					raw, _ := os.ReadFile(logDir + "/" + f.Name())
					text := string(raw)
					if strings.Contains(text, "[首Token统计]") && strings.Contains(text, "口径="+ttftMetric) {
						found = true
					}
					if strings.Contains(text, "test content") || strings.Contains(text, "test reasoning") {
						t.Fatal("logged payload")
					}
				}
				if !found {
					t.Fatal("missing persisted metric audit")
				}
			})
		}
	}
}

func TestTTFTLegacySamplesStayOnDiskButNotInAverage(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	loadOptionalStats()
	now := time.Now().UnixMilli()
	legacy := statsRequest{Account: "a", Model: "m", At: now, Status: 200, Complete: true, TotalTokens: int64p(10), Credit: float64p(1), TTFTMS: float64p(87000), LatencyMS: 100000}
	current := statsRequest{Account: "a", Model: "m", At: now, Status: 200, Complete: true, TotalTokens: int64p(20), Credit: float64p(2), TTFTMS: float64p(6400), TTFTMetric: ttftMetric, LatencyMS: 19000}
	failed := current
	failed.Complete = false
	failed.TTFTMS = float64p(90000)
	requestStats.Lock()
	requestStats.document.Accounts["a"] = statsAccount{File: "a.json"}
	requestStats.document.Requests = []statsRequest{legacy, current, failed}
	requestStats.revision++
	requestStats.Unlock()
	if err := flushOptionalStats(); err != nil {
		t.Fatal(err)
	}
	loadOptionalStats()
	code, view := statsQuery(t, "period=day")
	if code != 200 {
		t.Fatal(code)
	}
	s := view["summary"].(map[string]any)
	if s["avgTtftMs"] != float64(6400) || s["ttftSamples"] != float64(1) || s["legacyTtftSamples"] != float64(1) || s["tokens"] != float64(50) || s["credits"] != float64(5) {
		t.Fatalf("historical metrics mixed or token/credit changed: %+v", s)
	}
	requestStats.Lock()
	oldTTFT := *requestStats.document.Requests[0].TTFTMS
	requestStats.Unlock()
	if oldTTFT != 87000 {
		t.Fatal("legacy measurement was overwritten")
	}
}

func TestTTFTCancelledDoesNotCount(t *testing.T) {
	modelStatsMu.Lock()
	old := modelStats
	modelStats = map[string]*modelStat{}
	modelStatsMu.Unlock()
	defer func() { modelStatsMu.Lock(); modelStats = old; modelStatsMu.Unlock() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	reader := newTTFTReader(strings.NewReader(""), time.Now().Add(-time.Second))
	observeFirstToken(reader, firstTokenChunk(t, `{"content":"x"}`))
	completeTokenStream(reader, true)
	finishFirstToken(r, "cancelled", 1, reader)
	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()
	if s := modelStats["cancelled"]; s != nil && len(s.Buckets) > 0 {
		t.Fatal("cancelled entered mean")
	}
}

func TestTTFTFirstTokenStableAndLegacyCLISnapshotHidden(t *testing.T) {
	r := newTTFTReader(strings.NewReader(""), time.Now().Add(-time.Second))
	observeFirstToken(r, firstTokenChunk(t, `{"content":"first"}`))
	first := r.duration()
	observeFirstToken(r, firstTokenChunk(t, `{"reasoning_content":"later"}`))
	if r.duration() != first {
		t.Fatal("later delta overwrote first token time")
	}
	legacy := modelStatSnapshot{ID: "legacy", AvgTTFTMs: 87190, HasTTFT: true}
	formatted := formatMilliseconds(legacy.AvgTTFTMs, true)
	if text := renderModelTable([]modelStatSnapshot{legacy}); strings.Contains(text, formatted) {
		t.Fatal("old first-byte snapshot must not appear as v2 TTFT in CLI")
	}
	legacy.TTFTMetric = ttftMetric
	if text := renderModelTable([]modelStatSnapshot{legacy}); !strings.Contains(text, formatted) {
		t.Fatal("valid new-metric CLI value missing")
	}
}
