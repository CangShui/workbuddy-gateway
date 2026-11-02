package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

func newAPIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/responses", handleResponses)
	mux.HandleFunc("/responses", handleResponses)
	mux.HandleFunc("/v1/messages", handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", handleCountTokens)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/models", handleModels)
	// DSH Search / Fetch API compatibility.
	mux.HandleFunc("/v1/search", handleWebSearch)
	mux.HandleFunc("/v1/fetch", handleWebFetch)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/ping", handleHealth)
	mux.HandleFunc("/admin/probe", handleAdminProbe)
	mux.HandleFunc("/", handleIndex)
	return requestAuditMiddleware(corsMiddleware(authMiddleware(mux)))
}

func prepareGatewayServers() ([]*http.Server, []net.Listener, error) {
	if err := webUIPreflightError(); err != nil {
		return nil, nil, err
	}
	servers := []*http.Server{{
		Addr: net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.Port)), Handler: newAPIHandler(),
		ReadTimeout: 120 * time.Second,
		// 保留模型流无总写入时限，不把管理端限制扩散到模型 API。
		WriteTimeout: 0,
	}}
	if cfg.WebUI {
		servers = append(servers, &http.Server{
			Addr: net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.WebPort)), Handler: newWebUIHandler(),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		})
	}
	trace := newTraceID()
	var listeners []net.Listener
	for i, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			log.Printf("[监听初始化] traceId=%s 地址=%s 结果=失败 原因=%v 业务影响=已释放先前监听，未启动后台业务", trace, server.Addr, err)
			return nil, nil, fmt.Errorf("监听 %s 失败: %w", server.Addr, err)
		}
		listeners = append(listeners, listener)
		log.Printf("[监听初始化] traceId=%s 地址=%s 管理端=%t 结果=绑定成功", trace, server.Addr, i == 1)
	}
	return servers, listeners, nil
}

type gatewayHTTPSlot struct {
	server   *http.Server
	listener net.Listener
	web      bool
}

type gatewayServerManager struct {
	mu        sync.Mutex
	installed bool
	addr      string
	api       *gatewayHTTPSlot
	web       *gatewayHTTPSlot
	retired   []*http.Server
	errs      chan<- error
}

var gatewayServers gatewayServerManager

func newGatewayHTTPSlot(addr string, web bool) *gatewayHTTPSlot {
	var handler http.Handler
	var server *http.Server
	if web {
		handler = newWebUIHandler()
		server = &http.Server{
			Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		}
	} else {
		handler = newAPIHandler()
		server = &http.Server{Addr: addr, Handler: handler, ReadTimeout: 120 * time.Second, WriteTimeout: 0}
	}
	return &gatewayHTTPSlot{server: server, web: web}
}

// register adopts listeners that were already bound by prepareGatewayServers.
func (m *gatewayServerManager) register(servers []*http.Server, listeners []net.Listener) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.installed, m.addr = true, cfg.Addr
	m.api, m.web, m.retired = nil, nil, nil
	for i, server := range servers {
		slot := &gatewayHTTPSlot{server: server, listener: listeners[i], web: i == 1}
		if slot.web {
			m.web = slot
		} else {
			m.api = slot
		}
	}
}

func (m *gatewayServerManager) start(errs chan<- error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errs = errs
	m.serveLocked(m.api)
	m.serveLocked(m.web)
}

func (m *gatewayServerManager) serveLocked(slot *gatewayHTTPSlot) {
	if slot == nil {
		return
	}
	errs := m.errs
	server, listener := slot.server, slot.listener
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			if errs != nil {
				select {
				case errs <- fmt.Errorf("监听 %s 服务异常: %w", server.Addr, err):
				default:
				}
			}
		}
	}()
}

func (m *gatewayServerManager) ports() (int, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.installed {
		return cfg.Port, cfg.WebPort, false
	}
	apiPort, webPort := 0, 0
	if m.api != nil {
		apiPort, _ = strconv.Atoi(portOf(m.api.listener.Addr()))
	}
	if m.web != nil {
		webPort, _ = strconv.Atoi(portOf(m.web.listener.Addr()))
	}
	return apiPort, webPort, true
}

func portOf(addr net.Addr) string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "0"
	}
	return port
}

// apply swaps changed listeners without closing established connections.
// It validates and binds every new listener before retiring any old one.
func (m *gatewayServerManager) apply(g gatewayFileConfig, apiPort int, webEnabled bool) (int, int, error) {
	return m.applyWith(g, apiPort, webEnabled, nil)
}

// beforeCommit may prepare another fallible runtime resource (debug logging).
// Run it only after all new ports bind, but before publishing/retiring listeners.
func (m *gatewayServerManager) applyWith(g gatewayFileConfig, apiPort int, webEnabled bool, beforeCommit func() error) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.installed {
		if beforeCommit != nil {
			if err := beforeCommit(); err != nil {
				return cfg.Port, cfg.WebPort, err
			}
		}
		return cfg.Port, cfg.WebPort, nil
	}
	currentAPI, currentWeb := 0, 0
	if m.api != nil {
		currentAPI, _ = strconv.Atoi(portOf(m.api.listener.Addr()))
	}
	if m.web != nil {
		currentWeb, _ = strconv.Atoi(portOf(m.web.listener.Addr()))
	}
	targetWeb := 0
	if webEnabled {
		targetWeb = g.WebPort
	}
	if apiPort == currentAPI && targetWeb == currentWeb {
		if beforeCommit != nil {
			if err := beforeCommit(); err != nil {
				return currentAPI, currentWeb, err
			}
		}
		return currentAPI, currentWeb, nil
	}
	// Swapping the two live ports cannot be pre-bound. Refuse rather than take
	// every listener offline; users can switch through a temporary third port.
	if apiPort == currentWeb || targetWeb != 0 && targetWeb == currentAPI {
		return currentAPI, currentWeb, errors.New("新端口与本进程另一现有监听正在互换，请先改到临时端口再切换")
	}
	newAPI, newWeb := m.api, m.web
	opened := []net.Listener{}
	closeOpened := func() {
		for _, listener := range opened {
			_ = listener.Close()
		}
	}
	if apiPort != currentAPI {
		listener, err := net.Listen("tcp", net.JoinHostPort(m.addr, strconv.Itoa(apiPort)))
		if err != nil {
			return currentAPI, currentWeb, fmt.Errorf("新 API 监听 %s 绑定失败: %w", net.JoinHostPort(m.addr, strconv.Itoa(apiPort)), err)
		}
		opened = append(opened, listener)
		slot := newGatewayHTTPSlot(listener.Addr().String(), false)
		slot.listener = listener
		newAPI = slot
	}
	if targetWeb != 0 && targetWeb != currentWeb {
		listener, err := net.Listen("tcp", net.JoinHostPort(m.addr, strconv.Itoa(targetWeb)))
		if err != nil {
			closeOpened()
			return currentAPI, currentWeb, fmt.Errorf("新 Web 监听 %s 绑定失败: %w", net.JoinHostPort(m.addr, strconv.Itoa(targetWeb)), err)
		}
		opened = append(opened, listener)
		slot := newGatewayHTTPSlot(listener.Addr().String(), true)
		slot.listener = listener
		newWeb = slot
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			closeOpened()
			return currentAPI, currentWeb, err
		}
	}
	oldAPI, oldWeb := m.api, m.web
	m.api, m.web = newAPI, newWeb
	if newAPI != oldAPI {
		m.serveLocked(newAPI)
	}
	if newWeb != oldWeb && newWeb != nil {
		m.serveLocked(newWeb)
	}
	// Close only listeners. Existing HTTP connections and active model streams
	// continue to be served by their original server goroutines.
	for _, old := range []*gatewayHTTPSlot{oldAPI, oldWeb} {
		if old == nil || old == newAPI || old == newWeb {
			continue
		}
		_ = old.listener.Close()
		m.retired = append(m.retired, old.server)
	}
	apiOut, webOut := apiPort, targetWeb
	if newAPI != nil {
		apiOut, _ = strconv.Atoi(portOf(newAPI.listener.Addr()))
	}
	if newWeb != nil {
		webOut, _ = strconv.Atoi(portOf(newWeb.listener.Addr()))
	}
	return apiOut, webOut, nil
}

func (m *gatewayServerManager) shutdown(ctx context.Context) {
	m.mu.Lock()
	slots := []*gatewayHTTPSlot{m.api, m.web}
	retired := append([]*http.Server(nil), m.retired...)
	m.installed, m.api, m.web, m.retired = false, nil, nil, nil
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, slot := range slots {
		if slot == nil {
			continue
		}
		wg.Add(1)
		go func(s *gatewayHTTPSlot) {
			defer wg.Done()
			if err := s.server.Shutdown(ctx); err != nil {
				_ = s.server.Close()
			}
		}(slot)
	}
	for _, server := range retired {
		// Retired slots may still carry an old stream; give them the same bounded
		// shutdown window but never close an active response prematurely.
		wg.Add(1)
		go func(server *http.Server) {
			defer wg.Done()
			_ = server.Shutdown(ctx)
		}(server)
	}
	wg.Wait()
}
