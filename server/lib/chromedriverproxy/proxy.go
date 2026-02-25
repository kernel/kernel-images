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

var (
	chromeDriverAddr = "127.0.0.1:9225"
	debuggerAddr     = "127.0.0.1:9222"
)

// Handler returns an http.Handler that proxies all requests to the upstream
// ChromeDriver instance at 127.0.0.1:9225. For POST /session it injects the
// debuggerAddress capability pointing to the devtools proxy (127.0.0.1:9222)
// so ChromeDriver attaches to the already-running Chrome. WebSocket upgrade
// requests (used by WebDriver BiDi) are proxied bidirectionally.
func Handler(logger *slog.Logger) http.Handler {
	upstream, _ := url.Parse("http://" + chromeDriverAddr)

	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = r.In.Host
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			proxyWebSocket(w, r, logger)
			return
		}

		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			handleCreateSession(w, r, logger)
			return
		}

		reverseProxy.ServeHTTP(w, r)
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// handleCreateSession intercepts POST /session, injects goog:chromeOptions
// with debuggerAddress pointing to the devtools proxy, and forwards to ChromeDriver.
// It also rewrites the webSocketUrl in the response to point back through the proxy.
func handleCreateSession(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
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

	injectDebuggerAddress(payload, debuggerAddr)

	rewritten, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode modified body", http.StatusInternalServerError)
		return
	}

	logger.Info("chromedriver proxy: injected debuggerAddress into POST /session",
		slog.String("rewritten", string(rewritten)))

	upstreamURL := fmt.Sprintf("http://%s/session", chromeDriverAddr)
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

// rewriteWebSocketURL rewrites value.capabilities.webSocketUrl in a session
// creation response so it points back through the proxy instead of directly
// to ChromeDriver.
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

// injectDebuggerAddress sets goog:chromeOptions.debuggerAddress in
// capabilities.alwaysMatch, which ChromeDriver merges into every candidate.
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
	setChromeOption(alwaysMatch, "debuggerAddress", addr)
}

func setChromeOption(caps map[string]interface{}, key, value string) {
	opts, ok := caps["goog:chromeOptions"].(map[string]interface{})
	if !ok {
		opts = map[string]interface{}{}
		caps["goog:chromeOptions"] = opts
	}
	opts[key] = value
}

// proxyWebSocket handles WebSocket upgrade requests by proxying them
// bidirectionally to the upstream ChromeDriver, preserving the request path.
// Client-to-upstream messages are inspected for BiDi session.new commands;
// when found, debuggerAddress is injected into the capabilities.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	upstreamURL := (&url.URL{
		Scheme:   "ws",
		Host:     chromeDriverAddr,
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
		return maybeInjectBidiSession(msg, logger)
	}

	wsproxy.Proxy(w, r, upstreamURL, acceptOpts, dialOpts, logger, transform)
}

// maybeInjectBidiSession checks if a WebSocket message is a BiDi session.new
// command and injects debuggerAddress into its capabilities if so.
func maybeInjectBidiSession(msg []byte, logger *slog.Logger) []byte {
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

	injectDebuggerAddress(params, debuggerAddr)

	rewritten, err := json.Marshal(bidiMsg)
	if err != nil {
		logger.Error("chromedriver proxy: failed to re-encode session.new", slog.String("err", err.Error()))
		return msg
	}

	logger.Info("chromedriver proxy: injected debuggerAddress into BiDi session.new",
		slog.String("debuggerAddress", debuggerAddr))
	return rewritten
}
