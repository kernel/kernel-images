package cdpmonitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/require"
)

func TestFailedProbeCancelsTelemetryDrain(t *testing.T) {
	for _, blockedMethod := range []string{"Runtime.enable", "Network.getResponseBody"} {
		for _, ending := range []string{"disabled", "enabled", "shutdown"} {
			t.Run(blockedMethod+"/"+ending, func(t *testing.T) {
				t.Parallel()
				testFailedProbeCancelsTelemetryDrain(t, blockedMethod, ending)
			})
		}
	}
}

func testFailedProbeCancelsTelemetryDrain(t *testing.T, blockedMethod, ending string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	blocked := make(chan struct{})
	sockets := make(chan *websocket.Conn, 4)
	var connections, replacementRuntimeEnables atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		generation := connections.Add(1)
		select {
		case sockets <- conn:
		case <-ctx.Done():
			return
		}
		blackhole := false
		for {
			var command struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if wsjson.Read(ctx, conn, &command) != nil {
				return
			}
			if generation == 1 && command.Method == blockedMethod && !blackhole {
				blackhole = true
				close(blocked)
			}
			if blackhole {
				continue // Keep the socket open, but stop replying to commands, including probes.
			}
			result := map[string]any{}
			switch command.Method {
			case "Target.getTargets":
				result["targetInfos"] = []any{map[string]any{"targetId": "target", "type": "page"}}
			case "Target.attachToTarget":
				result["sessionId"] = "session"
			case "Page.addScriptToEvaluateOnNewDocument":
				result["identifier"] = "script"
			case "Runtime.enable":
				if generation > 1 {
					replacementRuntimeEnables.Add(1)
				}
			}
			if wsjson.Write(ctx, conn, map[string]any{"id": command.ID, "result": result}) != nil {
				return
			}
		}
	}))
	defer server.Close()

	// Hold the disconnected publication so a fast reconnect cannot conceal a
	// stale health value. This also checks cancellation precedes publication.
	disconnected := make(chan bool, 1)
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	var m *Monitor
	m = New(newTestUpstream("ws"+strings.TrimPrefix(server.URL, "http")), func(ev events.Event) (events.Envelope, bool) {
		if ev.Type == EventMonitorDisconnected {
			select {
			case disconnected <- m.NetworkSnapshot().Up:
			case <-ctx.Done():
			}
			select {
			case <-released:
			case <-ctx.Done():
			}
		}
		return events.Envelope{Event: ev}, true
	}, 0, discardLogger, func() bool { return false })
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	defer release()
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, time.Second, time.Millisecond)
	conn := <-sockets
	send := func(method string, params any, socket *websocket.Conn) {
		t.Helper()
		require.NoError(t, wsjson.Write(ctx, socket, map[string]any{"method": method, "sessionId": "session", "params": params}))
	}
	terminal := map[string]any{"requestId": "seed", "errorText": "net::ERR_CONNECTION_RESET"}
	send("Network.loadingFailed", terminal, conn)
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Resets == 1 }, time.Second, time.Millisecond)
	require.NoError(t, m.SetTelemetry(true))
	if blockedMethod == "Network.getResponseBody" {
		require.Eventually(t, func() bool {
			m.sessionsMu.RLock()
			defer m.sessionsMu.RUnlock()
			return m.optionalSessions["session"] == "script" && !m.telemetryChanging.Load()
		}, time.Second, time.Millisecond)
		send("Network.requestWillBeSent", map[string]any{"requestId": "body", "type": "Fetch", "request": map[string]any{"method": "GET", "url": "http://fixture/body"}}, conn)
		send("Network.responseReceived", map[string]any{"requestId": "body", "response": map[string]any{"status": 200, "mimeType": "text/plain"}}, conn)
		send("Network.loadingFinished", map[string]any{"requestId": "body"}, conn)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("optional command was not blocked")
	}
	before := m.NetworkSnapshot()
	require.True(t, before.Up)
	m.lifeMu.Lock()
	old := m.conn
	m.lifeMu.Unlock()
	require.NoError(t, m.SetTelemetry(false))
	require.Eventually(t, m.telemetryChanging.Load, time.Second, time.Millisecond)
	if m.restartMu.TryLock() {
		m.restartMu.Unlock()
		t.Fatal("telemetry drain did not hold replacement serialization")
	}
	wantEnabled := ending == "enabled"
	if wantEnabled {
		require.NoError(t, m.SetTelemetry(true))
	}
	desired := m.desiredTelemetry.Load()
	select {
	case up := <-disconnected:
		require.False(t, up, "failed probe published disconnected while health was still up")
	case <-time.After(12 * time.Second):
		t.Fatal("supervisor did not detect the blackholed connection")
	}
	require.ErrorIs(t, old.ctx.Err(), context.Canceled, "failed connection was not canceled before waiting for telemetry drain")
	require.False(t, m.NetworkSnapshot().Up)
	release()
	if ending != "shutdown" {
		require.Eventually(t, func() bool { return connections.Load() == 2 && m.NetworkSnapshot().Up }, 2*time.Second, time.Millisecond)
		waitForTelemetryReconcile(t, m, wantEnabled)
		if wantEnabled {
			require.Eventually(t, func() bool { return replacementRuntimeEnables.Load() == 1 }, time.Second, time.Millisecond)
		} else {
			require.Zero(t, replacementRuntimeEnables.Load())
		}
		after := m.NetworkSnapshot()
		require.Equal(t, before.Resets, after.Resets)
		require.Equal(t, before.Completed, after.Completed)
		// Reusing the same IDs on the replacement must not reuse terminal history.
		fresh := <-sockets
		send("Network.loadingFailed", terminal, fresh)
		require.Eventually(t, func() bool { return m.NetworkSnapshot().Resets == before.Resets+1 }, time.Second, time.Millisecond)
		require.Equal(t, before.Completed+1, m.NetworkSnapshot().Completed)
	}
	require.Equal(t, desired, m.desiredTelemetry.Load())
	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for the optional command timeout")
	}
	require.False(t, m.NetworkSnapshot().Up)
	require.False(t, m.IsRunning())
	if ending == "shutdown" {
		require.Equal(t, before.Resets, m.NetworkSnapshot().Resets)
		require.Equal(t, before.Completed, m.NetworkSnapshot().Completed)
	}
}
