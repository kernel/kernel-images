package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func chromeServer(t *testing.T, connects, commands *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		connects.Add(1)
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			commands.Add(1)
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				return
			}
			delete(m, "method")
			m["result"] = map[string]any{"value": commands.Load()}
			data, _ = json.Marshal(m)
			if c.Write(r.Context(), websocket.MessageText, data) != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestRelayRetainsChromeAndAcknowledgesEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var connects, commands atomic.Int32
	chrome := chromeServer(t, &connects, &commands)
	r, err := newRelay(ctx, strings.Replace(chrome.URL, "http:", "ws:", 1), "test-token")
	require.NoError(t, err)
	defer r.chrome.CloseNow()
	s := httptest.NewServer(r)
	defer s.Close()
	f := frame{Seq: 1, Data: json.RawMessage(`{"id":1,"method":"Runtime.evaluate"}`)}
	require.Error(t, request(ctx, "POST", s.URL+"/send", "wrong-token", f, nil))
	require.NoError(t, request(ctx, "POST", s.URL+"/send", "test-token", f, nil))
	// Each HTTP request uses a fresh TCP connection, including this retry.
	require.NoError(t, request(ctx, "POST", s.URL+"/send", "test-token", f, nil))
	var batch []frame
	require.NoError(t, request(ctx, "GET", s.URL+"/events?after=0", "test-token", nil, &batch))
	require.Len(t, batch, 1)
	require.Equal(t, uint64(1), batch[0].Seq)
	require.Error(t, request(ctx, "GET", s.URL+"/events?after=99", "test-token", nil, nil))
	var replay []frame
	require.NoError(t, request(ctx, "GET", s.URL+"/events?after=0", "test-token", nil, &replay))
	require.Equal(t, batch, replay)
	require.NoError(t, request(ctx, "GET", s.URL+"/events?after=1", "test-token", nil, &batch))
	require.Empty(t, batch)
	require.Equal(t, int32(1), connects.Load())
	require.Equal(t, int32(1), commands.Load())
	f.Seq = 3
	require.Error(t, request(ctx, "POST", s.URL+"/send", "test-token", f, nil))
	f.Seq = 2
	require.NoError(t, request(ctx, "POST", s.URL+"/send", "test-token", f, nil))
	require.Eventually(t, func() bool { return commands.Load() == 2 }, time.Second, time.Millisecond)
}

func TestRelayFailsRatherThanDroppingBufferedEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chrome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		data := []byte(`{"method":"event","padding":"` + strings.Repeat("x", maxBytes/2) + `"}`)
		for range 2 {
			if c.Write(req.Context(), websocket.MessageText, data) != nil {
				return
			}
		}
		_, _, _ = c.Read(req.Context())
	}))
	defer chrome.Close()
	r, err := newRelay(ctx, strings.Replace(chrome.URL, "http:", "ws:", 1), "token")
	require.NoError(t, err)
	defer r.chrome.CloseNow()
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.err != nil
	}, 3*time.Second, time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	require.ErrorContains(t, r.err, "buffer full")
	require.LessOrEqual(t, r.bytes, maxBytes)
}

func TestCommandKeyIncludesSession(t *testing.T) {
	require.NotEqual(t, commandKey([]byte(`{"id":1,"sessionId":"a"}`)), commandKey([]byte(`{"id":1,"sessionId":"b"}`)))
	require.Empty(t, commandKey([]byte(`{"method":"Runtime.bindingCalled"}`)))
}

func TestStandbyRejectsPendingCommands(t *testing.T) {
	g := &gateway{state: "running", client: &websocket.Conn{}, pending: map[string]bool{"session:1": true}}
	require.ErrorContains(t, g.standby(context.Background()), "commands pending")
}

func TestGatewayIdleAndWake(t *testing.T) {
	for _, mode := range []string{"direct", "relay"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var connects, commands, standbys, restores atomic.Int32
			chrome := chromeServer(t, &connects, &commands)
			upstream := strings.Replace(chrome.URL, "http:", "ws:", 1)
			if mode == "relay" {
				r, err := newRelay(ctx, upstream, "token")
				require.NoError(t, err)
				defer r.chrome.CloseNow()
				s := httptest.NewServer(r)
				defer s.Close()
				upstream = s.URL
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/instances/test/standby":
					standbys.Add(1)
				case "/instances/test/restore":
					restores.Add(1)
				default:
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer api.Close()
			g := &gateway{mode: mode, upstream: upstream, api: api.URL, instance: "test", relayToken: "token", idle: 200 * time.Millisecond, state: "running", pending: make(map[string]bool)}
			s := httptest.NewServer(g.handler())
			defer s.Close()
			client, _, err := websocket.Dial(ctx, strings.Replace(s.URL, "http:", "ws:", 1)+"/cdp", nil)
			require.NoError(t, err)
			defer client.CloseNow()
			require.NoError(t, client.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"test"}`)))
			_, _, err = client.Read(ctx)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				g.mu.Lock()
				defer g.mu.Unlock()
				return g.state == "standby"
			}, 3*time.Second, 10*time.Millisecond)
			// A websocket ping is answered without restoring the browser.
			pingCtx, pingCancel := context.WithTimeout(ctx, time.Second)
			defer pingCancel()
			readDone := make(chan error, 1)
			go func() { _, _, err := client.Read(ctx); readDone <- err }()
			require.NoError(t, client.Ping(pingCtx))
			require.Zero(t, restores.Load())
			require.NoError(t, client.Write(ctx, websocket.MessageText, []byte(`{"id":2,"method":"test"}`)))
			require.NoError(t, <-readDone)
			require.Equal(t, int32(1), standbys.Load())
			require.Equal(t, int32(1), restores.Load())
			require.Equal(t, int32(1), connects.Load())
			require.Equal(t, int32(2), commands.Load())
		})
	}
}
