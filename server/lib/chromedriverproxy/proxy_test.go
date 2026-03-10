package chromedriverproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testOptions(upstream, debugger string) *Options {
	return &Options{
		ChromeDriverUpstream: upstream,
		DevToolsProxyAddr:    debugger,
	}
}

func TestInjectDebuggerAddress_EmptyPayload(t *testing.T) {
	payload := map[string]interface{}{}
	injectDebuggerAddress(payload, "127.0.0.1:9988")

	caps := payload["capabilities"].(map[string]interface{})
	alwaysMatch := caps["alwaysMatch"].(map[string]interface{})
	opts := alwaysMatch["goog:chromeOptions"].(map[string]interface{})
	assert.Equal(t, "127.0.0.1:9988", opts["debuggerAddress"])
}

func TestInjectDebuggerAddress_ExistingCapabilities(t *testing.T) {
	payload := map[string]interface{}{
		"capabilities": map[string]interface{}{
			"alwaysMatch": map[string]interface{}{
				"goog:chromeOptions": map[string]interface{}{
					"args": []interface{}{"--headless"},
				},
			},
		},
	}
	injectDebuggerAddress(payload, "127.0.0.1:9988")

	caps := payload["capabilities"].(map[string]interface{})
	alwaysMatch := caps["alwaysMatch"].(map[string]interface{})
	opts := alwaysMatch["goog:chromeOptions"].(map[string]interface{})
	assert.Equal(t, "127.0.0.1:9988", opts["debuggerAddress"])
	assert.Equal(t, []interface{}{"--headless"}, opts["args"], "existing options should be preserved")
}

func TestInjectDebuggerAddress_OverridesExisting(t *testing.T) {
	payload := map[string]interface{}{
		"capabilities": map[string]interface{}{
			"alwaysMatch": map[string]interface{}{
				"goog:chromeOptions": map[string]interface{}{
					"debuggerAddress": "old:1234",
				},
			},
		},
	}
	injectDebuggerAddress(payload, "127.0.0.1:9988")

	caps := payload["capabilities"].(map[string]interface{})
	alwaysMatch := caps["alwaysMatch"].(map[string]interface{})
	opts := alwaysMatch["goog:chromeOptions"].(map[string]interface{})
	assert.Equal(t, "127.0.0.1:9988", opts["debuggerAddress"])
}

func TestIsWebSocketUpgrade_MultiValueConnection(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	assert.True(t, isWebSocketUpgrade(req))
}

func TestHandler_PostSession_InjectsDebuggerAddress(t *testing.T) {
	debuggerAddr := "127.0.0.1:9911"
	var capturedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := `{"value":{"sessionId":"abc123","capabilities":{"webSocketUrl":"ws://127.0.0.1:9225/session/abc123"}}}`
		w.Write([]byte(resp))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	handler := Handler(silentLogger(), testOptions(backendURL.Host, debuggerAddr))

	reqBody := `{"capabilities":{"alwaysMatch":{"browserName":"chrome"}}}`
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(reqBody))
	req.Host = "127.0.0.1:9224"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, capturedBody)

	var received map[string]interface{}
	require.NoError(t, json.Unmarshal(capturedBody, &received))

	caps := received["capabilities"].(map[string]interface{})
	alwaysMatch := caps["alwaysMatch"].(map[string]interface{})
	opts := alwaysMatch["goog:chromeOptions"].(map[string]interface{})
	assert.Equal(t, debuggerAddr, opts["debuggerAddress"])
	assert.Equal(t, "chrome", alwaysMatch["browserName"], "original capabilities preserved")

	var respBody map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &respBody))
	value := respBody["value"].(map[string]interface{})
	assert.Equal(t, "abc123", value["sessionId"])
	respCaps := value["capabilities"].(map[string]interface{})
	assert.Equal(t, "ws://127.0.0.1:9224/session/abc123", respCaps["webSocketUrl"],
		"webSocketUrl in capabilities should be rewritten to proxy address")
}

func TestHandler_HTTPPassthrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]interface{}{
			"path":   r.URL.Path,
			"method": r.Method,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	handler := Handler(silentLogger(), testOptions(backendURL.Host, "127.0.0.1:9922"))

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/status"},
		{http.MethodGet, "/session/abc123"},
		{http.MethodPost, "/session/abc123/url"},
		{http.MethodDelete, "/session/abc123"},
		{http.MethodPost, "/session/abc123/element"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var body io.Reader
			if tt.method == http.MethodPost {
				body = strings.NewReader(`{"url":"https://example.com"}`)
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			if tt.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)

			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tt.path, resp["path"])
			assert.Equal(t, tt.method, resp["method"])
		})
	}
}

func TestHandler_WebSocketPassthrough(t *testing.T) {
	echoBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		for {
			mt, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, mt, msg); err != nil {
				return
			}
		}
	}))
	defer echoBackend.Close()

	backendURL, _ := url.Parse(echoBackend.URL)
	handler := Handler(silentLogger(), testOptions(backendURL.Host, "127.0.0.1:9922"))
	proxySrv := httptest.NewServer(handler)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	proxyURL.Scheme = "ws"
	proxyURL.Path = "/session/abc123"

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, proxyURL.String(), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	msg := "hello bidi"
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(msg)))

	_, resp, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, msg, string(resp))
}

func TestHandler_WebSocket_BiDiSessionNew_InjectsDebuggerAddress(t *testing.T) {
	debuggerAddr := "127.0.0.1:9933"
	var capturedMsg []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		mt, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		capturedMsg = msg
		c.Write(ctx, mt, []byte(`{"id":1,"type":"success","result":{}}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	handler := Handler(silentLogger(), testOptions(backendURL.Host, debuggerAddr))
	proxySrv := httptest.NewServer(handler)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	proxyURL.Scheme = "ws"
	proxyURL.Path = "/session"

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, proxyURL.String(), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	bidiCmd := `{"id":1,"method":"session.new","params":{"capabilities":{"alwaysMatch":{"webSocketUrl":true}}}}`
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(bidiCmd)))

	_, _, err = conn.Read(ctx)
	require.NoError(t, err)
	require.NotNil(t, capturedMsg)

	var received map[string]interface{}
	require.NoError(t, json.Unmarshal(capturedMsg, &received))

	params := received["params"].(map[string]interface{})
	caps := params["capabilities"].(map[string]interface{})
	alwaysMatch := caps["alwaysMatch"].(map[string]interface{})
	opts := alwaysMatch["goog:chromeOptions"].(map[string]interface{})
	assert.Equal(t, debuggerAddr, opts["debuggerAddress"])
	assert.Equal(t, true, alwaysMatch["webSocketUrl"], "original capabilities preserved")
}

func TestHandler_WebSocket_NonSessionNew_PassesThrough(t *testing.T) {
	var capturedMsg []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		mt, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		capturedMsg = msg
		c.Write(ctx, mt, msg)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	handler := Handler(silentLogger(), testOptions(backendURL.Host, "127.0.0.1:9922"))
	proxySrv := httptest.NewServer(handler)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	proxyURL.Scheme = "ws"
	proxyURL.Path = "/session"

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, proxyURL.String(), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "")

	bidiCmd := `{"id":2,"method":"browsingContext.getTree","params":{}}`
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(bidiCmd)))

	_, resp, err := conn.Read(ctx)
	require.NoError(t, err)

	assert.Equal(t, bidiCmd, string(capturedMsg), "non-session.new messages should pass through unmodified")
	assert.Equal(t, bidiCmd, string(resp))
}
