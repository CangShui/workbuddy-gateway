package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// 所有进程内配置文件读改写共用此锁，避免网页写入与热加载互相覆盖。
// 锁序：runtimeConfigMu -> gatewayConfigMu；调度不持有此锁。
var runtimeConfigMu sync.Mutex
var upstreamConfigMu sync.RWMutex
var statusSnapshotWriteMu sync.Mutex

// 与运行时状态同步更新，不能仅由扫描协程缓存：网页写入后立即撤回旧文件也必须生效。
var runtimeAppliedHash [32]byte
var runtimeHasAppliedHash bool

func parseRuntimeConfig(data []byte) (runtimeFileConfig, error) {
	var c runtimeFileConfig
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return c, errors.New("config.json 必须是 JSON 对象，不能是空文件或 null")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return c, err
	}
	g, err := normalizeGatewayConfig(c.Gateway)
	if err != nil {
		return c, err
	}
	apiPort := g.APIPort
	if cfg.PortExplicit {
		apiPort = cfg.Port
	}
	if apiPort < 1 || apiPort > 65535 || cfg.WebUI && apiPort == g.WebPort {
		return c, errors.New("API 监听端口无效或与 Web 监听端口冲突")
	}
	if _, err := buildModelAccountRules(c.Models.Accounts); err != nil {
		return c, err
	}
	for _, seconds := range []int{c.Upstream.HeaderTimeoutSeconds, c.Upstream.IdleTimeoutSeconds} {
		if int64(seconds) > int64((1<<63-1)/time.Second) {
			return c, errors.New("upstream 超时秒数过大，超出 time.Duration 可表示范围")
		}
	}
	// 兼容旧示例同时携带两个空数组，但非空黑白名单不能同时生效。
	if hasModelNames(c.Models.Blocklist) && hasModelNames(c.Models.Allowlist) {
		return c, errors.New("models.allowlist 与 models.blocklist 不能同时非空，请只保留一种模型名单")
	}
	if c.Logs.RetentionDays < 0 || c.Logs.RetentionDays > 36500 {
		return c, gatewayValidationError("logs.retentionDays 必须为 0～36500 的整数；0 表示不自动清理")
	}
	if _, err := normalizeRequestProfile(c.Upstream.RequestProfile); err != nil {
		return c, err
	}
	return c, nil
}

func hasModelNames(names []string) bool {
	for _, name := range names {
		if normalizeModelName(name) != "" {
			return true
		}
	}
	return false
}

func loadRuntimeConfig(path string) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte("{}")
	} else if err != nil {
		return fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	c, err := parseRuntimeConfig(data)
	if err != nil {
		return fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	if err := applyRuntimeConfig(c, false, ""); err != nil {
		return err
	}
	runtimeAppliedHash, runtimeHasAppliedHash = sha256.Sum256(data), true
	return nil
}

// 调用前已完整校验。热加载不修改监听端口，不中断正在输出的流。
func applyRuntimeConfig(c runtimeFileConfig, hot bool, trace string) error {
	profile, err := normalizeRequestProfile(c.Upstream.RequestProfile)
	if err != nil {
		return err
	}
	if hot {
		if err := switchDebugLogging(c.Debug.Enabled); err != nil {
			return err // 日志文件不可写时，整份配置仍保留上一版本。
		}
		g, _ := normalizeGatewayConfig(c.Gateway)
		gatewayConfigMu.Lock()
		gatewaySettings = g
		setGatewayKeysLocked(g)
		gatewayConfigMu.Unlock()
	} else if err := applyGatewayConfig(c.Gateway); err != nil {
		return err
	}
	if !hot {
		cfg.DebugEnabled = c.Debug.Enabled // 仅启动时写入 cfg，运行期间由 sink 判断。
	}
	previousFull := debugFullRequests.Swap(c.Debug.FullRequests)
	if previousFull != c.Debug.FullRequests || !hot && c.Debug.FullRequests {
		log.Printf("[完整请求日志] traceId=%s 配置开启=%t 调试日志开启=%t 说明=仅模型API；入站与实际出站请求完整记录到debug日志，已知凭据脱敏，提示词代码与图片仍属敏感数据；无次数和时长限制，请排障后关闭",
			trace, c.Debug.FullRequests, c.Debug.Enabled)
	}
	setModelFilter(c.Models.Blocklist, c.Models.Allowlist)
	_ = setModelAccountFilter(c.Models.Accounts)
	setSystemPromptConfig(c.SystemPrompt.Fallback, c.SystemPrompt.Force)
	previousProfile := officialRequests.Load()
	setRequestProfile(profile)
	// 未配置 upstream.requestProfile 时会回退 legacy，出站请求体因此不压缩。
	// 该回退此前是静默的：升级后只能从「请求体没变小」间接推断，故启动时无条件打印一次。
	profileSource := "config.json upstream.requestProfile"
	if c.Upstream.RequestProfile == "" {
		profileSource = "缺省：config.json 未提供 upstream.requestProfile"
	}
	switch {
	case !hot:
		log.Printf("[出站兼容配置] traceId=%s profile=%s 来源=%s 结果=已应用 说明=legacy 不压缩出站请求体，official 启用出站 gzip 与桌面端指纹；只影响后续模型请求，不修改登录与凭据",
			trace, profile, profileSource)
	case previousProfile != officialRequests.Load():
		log.Printf("[出站兼容配置] traceId=%s profile=%s 结果=已应用 说明=只影响后续模型请求，正在执行的请求沿用原快照，不修改登录和凭据", trace, profile)
	}
	logRetentionDays.Store(int64(c.Logs.RetentionDays))
	header, idle, retries := upstreamHeaderTimeoutDefault, upstreamIdleTimeoutDefault, upstreamTransientRetriesDefault
	if n := c.Upstream.HeaderTimeoutSeconds; n > 0 {
		header = time.Duration(n) * time.Second
	}
	if n := c.Upstream.IdleTimeoutSeconds; n > 0 {
		idle = time.Duration(n) * time.Second
	}
	if n := c.Upstream.TransientRetries; n != nil && *n >= 0 {
		retries = *n
	}
	keepalive := streamKeepaliveDefault
	if n := c.Upstream.StreamKeepaliveSeconds; n > 0 {
		keepalive = time.Duration(n) * time.Second
	}
	firstByteRetry := enableFirstByteRetryDefault
	if n := c.Upstream.EnableFirstByteRetry; n != nil {
		firstByteRetry = *n
	}
	upstreamConfigMu.Lock()
	upstreamHeaderTimeout, upstreamIdleTimeout, upstreamTransientRetries = header, idle, retries
	streamKeepalive = keepalive
	enableFirstByteRetry = firstByteRetry
	upstreamConfigMu.Unlock()
	if hot {
		log.Printf("[配置热加载] traceId=%s 结果=成功 模型黑名单=%d 模型白名单=%d 账号规则=%d 调试日志=%t 日志保留天数=%d 响应头超时=%v 流空闲超时=%v 重试次数=%d 流心跳=%v 首字节重放=%t 说明=后续请求使用新配置，凭据及历史账本保留，端口变更需重启",
			trace, len(c.Models.Blocklist), len(c.Models.Allowlist), len(c.Models.Accounts), c.Debug.Enabled, c.Logs.RetentionDays, header, idle, retries, keepalive, firstByteRetry)
	}
	return nil
}

func upstreamSettings() (time.Duration, time.Duration, int) {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return upstreamHeaderTimeout, upstreamIdleTimeout, upstreamTransientRetries
}

func currentIdleTimeout() time.Duration {
	_, idle, _ := upstreamSettings()
	return idle
}

// currentStreamKeepalive 返回当前流式心跳间隔（0 表示关闭）。
func currentStreamKeepalive() time.Duration {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return streamKeepalive
}

// currentFirstByteRetry 返回当前首字节前重放开关。
func currentFirstByteRetry() bool {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return enableFirstByteRetry
}

func switchDebugLogging(enabled bool) error {
	debugSinkMu.Lock()
	defer debugSinkMu.Unlock()
	if enabled == (debugSink != nil) {
		return nil
	}
	if enabled {
		f, err := newDailyLogWriter("debug", ".jsonl")
		if err != nil {
			return err
		}
		debugSink = &debugJSONSink{writer: f, closer: f}
	} else {
		sink := debugSink
		debugSink = nil
		sink.mu.Lock()
		if sink.closer != nil {
			_ = sink.closer.Close()
		}
		sink.mu.Unlock()
	}
	return nil
}

// 根据内容摘要检测变化，避免相同大小/相同时间戳的编辑被漏掉。
// 缺失/半写/错误配置保留上一份有效配置；同一坏内容只告警一次。
type runtimeConfigWatcher struct {
	failed    [32]byte
	hasFailed bool
	lastError error
}

func (watcher *runtimeConfigWatcher) reload() bool {
	watcher.lastError = nil
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	data, err := os.ReadFile(runtimeConfigFile)
	sum := sha256.Sum256(data)
	if err != nil {
		watcher.lastError = err
		sum = sha256.Sum256([]byte(err.Error()))
	}
	if err == nil && runtimeHasAppliedHash && runtimeAppliedHash == sum {
		watcher.hasFailed = false
		return false
	}
	trace := newTraceID()
	var c runtimeFileConfig
	if err == nil {
		c, err = parseRuntimeConfig(data)
	}
	if err == nil {
		err = applyRuntimeConfig(c, true, trace)
	}
	if err != nil {
		if !watcher.hasFailed || watcher.failed != sum {
			log.Printf("[配置热加载] traceId=%s 结果=拒绝 原因=%s 业务影响=配置未切换，保留上一份有效配置；请修正文件后再保存", trace, safeDebugError(err))
		}
		watcher.lastError = err
		watcher.failed, watcher.hasFailed = sum, true
		return false
	}
	runtimeAppliedHash, runtimeHasAppliedHash, watcher.hasFailed = sum, true, false
	return true
}

func runtimeConfigLoop(ctx context.Context) {
	watcher := runtimeConfigWatcher{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastLogged time.Time
	var previousStatus string
	logScheduledTask("config_reload", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(time.Second), "")
	if logRetentionDays.Load() == 0 {
		logScheduledTask("log_cleanup", "disabled", "disabled", newTraceID(), time.Time{}, time.Time{}, "保留天数为0，不自动清理")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-ticker.C:
			next := tick.Add(time.Second)
			started := time.Now()
			changed := watcher.reload()
			status, detail := "success", "检查完成，配置没有变化"
			if changed {
				detail = "新配置已校验并应用"
			}
			if watcher.lastError != nil {
				status, detail = "failed", safeDebugError(watcher.lastError)
			}
			logFrequentTask("config_reload", status, detail, started, next, &lastLogged, &previousStatus, changed)
			scheduledLogCleanup(time.Now())
		}
	}
}

// Transport 不可在活跃请求期间原地改字段。仅超时变化时克隆，
// 新请求用新 Transport；旧流继续读取，闲置连接回收。
type hotConfigTransport struct {
	mu        sync.Mutex
	transport *http.Transport
}

func (t *hotConfigTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	header, _, _ := upstreamSettings()
	t.mu.Lock()
	if t.transport.ResponseHeaderTimeout != header {
		old := t.transport
		t.transport = old.Clone()
		t.transport.ResponseHeaderTimeout = header
		old.CloseIdleConnections()
	}
	current := t.transport
	t.mu.Unlock()
	return current.RoundTrip(r)
}

func (t *hotConfigTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.transport.CloseIdleConnections()
}
