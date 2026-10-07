package devtoolsproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
)

func filtered() bool   { return true }
func unfiltered() bool { return false }

func TestEgressGateRefusal(t *testing.T) {
	tests := []struct {
		name     string
		filtered EgressFilteredFunc
		frame    string
		refuse   bool
	}{
		{
			name:     "proxy server refused",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createBrowserContext","params":{"proxyServer":"http://127.0.0.1:9"}}`,
			refuse:   true,
		},
		{
			// An empty proxyServer still puts the context on direct
			// connections, which is why presence is what the gate tests.
			name:     "empty proxy server refused",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createBrowserContext","params":{"proxyServer":""}}`,
			refuse:   true,
		},
		{
			name:     "bypass list alone refused",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createBrowserContext","params":{"proxyBypassList":"*"}}`,
			refuse:   true,
		},
		{
			// Chromium rejects an explicit null as invalid parameters, so the
			// context never gets a proxy of its own.
			name:     "null proxy server forwarded",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createBrowserContext","params":{"proxyServer":null}}`,
		},
		{
			name:     "context without a proxy forwarded",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createBrowserContext","params":{"disposeOnDetach":true}}`,
		},
		{
			name:     "context with no params forwarded",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createBrowserContext"}`,
		},
		{
			// The gate is scoped to the one command that takes a context
			// proxy; a proxyServer key anywhere else means something else.
			name:     "other method forwarded",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createTarget","params":{"url":"about:blank","proxyServer":"http://127.0.0.1:9"}}`,
		},
		{
			// A session with no allowlist has nothing to step around.
			name:     "unfiltered session forwarded",
			filtered: unfiltered,
			frame:    `{"id":1,"method":"Target.createBrowserContext","params":{"proxyServer":"http://127.0.0.1:9"}}`,
		},
		{
			// The method is decoded rather than matched as text, so a client
			// cannot escape its way past the gate.
			name:     "escaped method name refused",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.\u0063reateBrowserContext","params":{"proxyServer":""}}`,
			refuse:   true,
		},
		{
			name:     "malformed frame forwarded",
			filtered: filtered,
			frame:    `{"id":1,"method":"Target.createBrowserContext","params":`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := newEgressGate(tt.filtered, silentLogger())
			_, refuse := gate.refuse(websocket.MessageText, []byte(tt.frame))
			if refuse != tt.refuse {
				t.Fatalf("refuse = %v, want %v", refuse, tt.refuse)
			}
		})
	}
}

func TestEgressGateRefusalReply(t *testing.T) {
	gate := newEgressGate(filtered, silentLogger())
	reply, refuse := gate.refuse(websocket.MessageText,
		[]byte(`{"id":7,"sessionId":"S1","method":"Target.createBrowserContext","params":{"proxyServer":"http://127.0.0.1:9"}}`))
	if !refuse {
		t.Fatal("command was not refused")
	}

	var got cdpErrorResponse
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if got.ID != 7 {
		t.Errorf("id = %d, want 7", got.ID)
	}
	// A reply that does not carry the command's session is not matched to it.
	if got.SessionID != "S1" {
		t.Errorf("sessionId = %q, want %q", got.SessionID, "S1")
	}
	if got.Error.Code != cdpInvalidParams {
		t.Errorf("error code = %d, want %d", got.Error.Code, cdpInvalidParams)
	}
	if !strings.Contains(got.Error.Message, "proxyServer") {
		t.Errorf("message does not name the refused parameter: %q", got.Error.Message)
	}
}

// A command with no id cannot be answered, but it must still not reach
// Chromium.
func TestEgressGateRefusesCommandWithoutIDWithoutReplying(t *testing.T) {
	gate := newEgressGate(filtered, silentLogger())
	reply, refuse := gate.refuse(websocket.MessageText,
		[]byte(`{"method":"Target.createBrowserContext","params":{"proxyServer":""}}`))
	if !refuse {
		t.Fatal("command was not refused")
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q, want none", reply)
	}
}

// A binary frame carrying the same JSON creates nothing on a real browser,
// which drops the connection instead of acting on it, so forwarding one is
// not a way past the gate.
func TestEgressGateIgnoresBinaryFrames(t *testing.T) {
	gate := newEgressGate(filtered, silentLogger())
	if _, refuse := gate.refuse(websocket.MessageBinary,
		[]byte(`{"id":1,"method":"Target.createBrowserContext","params":{"proxyServer":""}}`)); refuse {
		t.Fatal("binary frame was refused")
	}
}

// An image with no way to know the session's policy forwards everything, which
// is what a VM running an older control plane does.
func TestEgressGateAbsentWithoutPolicy(t *testing.T) {
	if gate := newEgressGate(nil, silentLogger()); gate != nil {
		t.Fatal("gate built without a policy source")
	}
}

// The gate belongs to the proxy, not to one client: whichever connection sends
// the command, it must not reach Chromium, and the client must get an answer
// rather than hanging.
func TestWebSocketProxyRefusesContextProxyEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var upstreamSaw []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		for {
			mt, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			mu.Lock()
			upstreamSaw = append(upstreamSaw, string(msg))
			mu.Unlock()
			if err := c.Write(r.Context(), mt, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	u, _ := url.Parse(upstream.URL)
	u.Scheme = "ws"
	u.Path = "/devtools/browser/x"

	logger := silentLogger()
	mgr := NewUpstreamManager("/dev/null", logger)
	mgr.setCurrent(u.String())

	proxy := httptest.NewServer(WebSocketProxyHandler(
		mgr, logger, false, scaletozero.NewNoopController(), nil, nil, nil, filtered, nil))
	t.Cleanup(proxy.Close)

	pu, _ := url.Parse(proxy.URL)
	pu.Scheme = "ws"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, pu.String(), nil)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })

	refused := `{"id":1,"method":"Target.createBrowserContext","params":{"proxyServer":"http://127.0.0.1:9"}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(refused)); err != nil {
		t.Fatalf("write refused command: %v", err)
	}
	_, reply, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	var got cdpErrorResponse
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("unmarshal refusal: %v", err)
	}
	if got.Error.Code != cdpInvalidParams {
		t.Fatalf("reply was not a refusal: %s", reply)
	}

	// A command the gate does not refuse still reaches Chromium, and reading
	// its echo proves the refused one was never forwarded ahead of it.
	allowed := `{"id":2,"method":"Target.createBrowserContext","params":{}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(allowed)); err != nil {
		t.Fatalf("write allowed command: %v", err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, msg := range upstreamSaw {
		if strings.Contains(msg, "proxyServer") {
			t.Fatalf("upstream received the refused command: %s", msg)
		}
	}
	if len(upstreamSaw) != 1 {
		t.Fatalf("upstream saw %d commands, want 1", len(upstreamSaw))
	}
}
