package wsproxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func proxyWithOptions(t *testing.T, opts ProxyOptions) (*websocket.Conn, <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer close(closed)
		defer conn.CloseNow()
		for {
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), kind, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Proxy(w, r, "ws"+strings.TrimPrefix(upstream.URL, "http"), opts)
	}))
	t.Cleanup(proxy.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn, closed
}

func TestProxyReadLimit(t *testing.T) {
	conn, _ := proxyWithOptions(t, ProxyOptions{ReadLimit: 64})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(strings.Repeat("x", 128))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("oversized frame was forwarded")
	}
}

func TestProxyPingDetectsUnresponsiveClient(t *testing.T) {
	_, closed := proxyWithOptions(t, ProxyOptions{PingInterval: 20 * time.Millisecond})
	// Without a client read loop, pings are not answered. The proxy must close
	// the upstream too instead of keeping the remote agent alive indefinitely.
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("unresponsive client did not close upstream")
	}
}
