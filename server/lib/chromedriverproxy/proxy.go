package chromedriverproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/onkernel/kernel-images/server/lib/wsproxy"
)

const (
	defaultChromeDriverUpstream = "127.0.0.1:9225"
	defaultDevToolsProxyAddr    = "127.0.0.1:9222"
)

// Options controls which upstream ChromeDriver to proxy to, and which DevTools
// proxy address should be injected into WebDriver/BiDi session creation.
type Options struct {
	ChromeDriverUpstream string
	DevToolsProxyAddr    string
}

func resolveOptions(opts *Options) Options {
	resolved := Options{
		ChromeDriverUpstream: defaultChromeDriverUpstream,
		DevToolsProxyAddr:    defaultDevToolsProxyAddr,
	}
	if opts == nil {
		return resolved
	}
	if opts.ChromeDriverUpstream != "" {
		resolved.ChromeDriverUpstream = opts.ChromeDriverUpstream
	}
	if opts.DevToolsProxyAddr != "" {
		resolved.DevToolsProxyAddr = opts.DevToolsProxyAddr
	}
	return resolved
}

// Handler proxies HTTP and WebSocket traffic to an internal ChromeDriver
// instance. It injects `goog:chromeOptions.debuggerAddress` during session
// creation so ChromeDriver attaches to the existing Chromium process instead of
// launching another browser.
func Handler(logger *slog.Logger, opts *Options) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	cfg := resolveOptions(opts)
	upstream, _ := url.Parse("http://" + cfg.ChromeDriverUpstream)

	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = r.In.Host
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			proxyWebSocket(w, r, logger, cfg)
			return
		}

		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			handleCreateSession(w, r, logger, cfg)
			return
		}

		reverseProxy.ServeHTTP(w, r)
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !hasHeaderToken(r.Header.Values("Connection"), "upgrade") {
		return false
	}
	return hasHeaderToken(r.Header.Values("Upgrade"), "websocket")
}

func hasHeaderToken(values []string, token string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// handleCreateSession intercepts WebDriver session creation and injects
// `goog:chromeOptions.debuggerAddress`, which tells ChromeDriver to attach to
// the already-running Chromium instance in this VM.
// Reference: https://developer.chrome.com/docs/chromedriver/capabilities
//
// It then rewrites `capabilities.webSocketUrl` in the response so clients route
// BiDi traffic through this proxy instead of attempting a direct internal
// connection to ChromeDriver.
func handleCreateSession(w http.ResponseWriter, r *http.Request, logger *slog.Logger, cfg Options) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	injectDebuggerAddress(payload, cfg.DevToolsProxyAddr)

	rewritten, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode modified body", http.StatusInternalServerError)
		return
	}

	logger.Info("chromedriver proxy: injected debuggerAddress into POST /session",
		slog.String("rewritten", string(rewritten)))

	upstreamURL := fmt.Sprintf("http://%s/session", cfg.ChromeDriverUpstream)
	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(rewritten))
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			proxyReq.Header.Add(k, v)
		}
	}
	proxyReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	proxyReq.ContentLength = int64(len(rewritten))

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		logger.Error("chromedriver proxy: upstream POST /session failed", slog.String("err", err.Error()))
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	respBody = rewriteWebSocketURL(respBody, r.Host, logger)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(respBody)))
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// rewriteWebSocketURL rewrites `value.capabilities.webSocketUrl` in a
// WebDriver new-session response to point at this proxy.
//
// Why: ChromeDriver often returns an internal host/port, but external clients
// must connect through the proxy listener.
//
// Example:
//
//	ws://127.0.0.1:9225/session/abc -> ws://proxy-host:9224/session/abc
func rewriteWebSocketURL(body []byte, proxyHost string, logger *slog.Logger) []byte {
	var respPayload map[string]interface{}
	if err := json.Unmarshal(body, &respPayload); err != nil {
		return body
	}

	value, ok := respPayload["value"].(map[string]interface{})
	if !ok {
		return body
	}
	caps, ok := value["capabilities"].(map[string]interface{})
	if !ok {
		return body
	}
	ws, ok := caps["webSocketUrl"].(string)
	if !ok {
		return body
	}

	parsed, err := url.Parse(ws)
	if err != nil {
		return body
	}
	parsed.Host = proxyHost
	caps["webSocketUrl"] = parsed.String()

	out, err := json.Marshal(respPayload)
	if err != nil {
		logger.Error("chromedriver proxy: failed to re-encode response", slog.String("err", err.Error()))
		return body
	}

	logger.Info("chromedriver proxy: rewrote webSocketUrl",
		slog.String("original", ws), slog.String("rewritten", parsed.String()))
	return out
}

// injectDebuggerAddress ensures `capabilities.alwaysMatch.goog:chromeOptions`
// includes debuggerAddress. This is the WebDriver capability ChromeDriver uses
// to attach to an existing browser instance.
func injectDebuggerAddress(payload map[string]interface{}, addr string) {
	caps, ok := payload["capabilities"].(map[string]interface{})
	if !ok {
		caps = map[string]interface{}{}
		payload["capabilities"] = caps
	}

	alwaysMatch, ok := caps["alwaysMatch"].(map[string]interface{})
	if !ok {
		alwaysMatch = map[string]interface{}{}
		caps["alwaysMatch"] = alwaysMatch
	}
	opts, ok := alwaysMatch["goog:chromeOptions"].(map[string]interface{})
	if !ok {
		opts = map[string]interface{}{}
		alwaysMatch["goog:chromeOptions"] = opts
	}
	opts["debuggerAddress"] = addr
}

// proxyWebSocket proxies BiDi WebSocket traffic to the configured ChromeDriver
// upstream, preserving the incoming request path/query.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, logger *slog.Logger, cfg Options) {
	upstreamURL := (&url.URL{
		Scheme:   "ws",
		Host:     cfg.ChromeDriverUpstream,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}).String()
	acceptOpts := &websocket.AcceptOptions{
		OriginPatterns:  []string{"*"},
		CompressionMode: websocket.CompressionContextTakeover,
	}
	dialOpts := &websocket.DialOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	}

	transform := func(direction string, mt websocket.MessageType, msg []byte) []byte {
		if direction != "->" || mt != websocket.MessageText {
			return msg
		}
		return maybeInjectBidiSession(msg, cfg.DevToolsProxyAddr, logger)
	}

	wsproxy.Proxy(w, r, upstreamURL, wsproxy.ProxyOptions{
		AcceptOptions: acceptOpts,
		DialOptions:   dialOpts,
		Logger:        logger,
		Transform:     transform,
	})
}

// maybeInjectBidiSession mutates outbound BiDi `session.new` commands so
// ChromeDriver attaches to the existing browser via debuggerAddress.
func maybeInjectBidiSession(msg []byte, debuggerAddress string, logger *slog.Logger) []byte {
	var bidiMsg map[string]interface{}
	if err := json.Unmarshal(msg, &bidiMsg); err != nil {
		return msg
	}

	method, _ := bidiMsg["method"].(string)
	if method != "session.new" {
		return msg
	}

	params, ok := bidiMsg["params"].(map[string]interface{})
	if !ok {
		params = map[string]interface{}{}
		bidiMsg["params"] = params
	}

	injectDebuggerAddress(params, debuggerAddress)

	rewritten, err := json.Marshal(bidiMsg)
	if err != nil {
		logger.Error("chromedriver proxy: failed to re-encode session.new", slog.String("err", err.Error()))
		return msg
	}

	logger.Info("chromedriver proxy: injected debuggerAddress into BiDi session.new",
		slog.String("debuggerAddress", debuggerAddress))
	return rewritten
}
