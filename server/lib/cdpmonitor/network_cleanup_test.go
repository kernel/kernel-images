package cdpmonitor

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDurableBodyDisableFailureReplacesConnection(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	var failed atomic.Bool
	m, _, cleanup := startMonitor(t, srv, func(msg cdpMessage) any {
		var params struct {
			EnableDurableMessages *bool `json:"enableDurableMessages"`
		}
		if msg.Method == "Network.enable" && json.Unmarshal(msg.Params, &params) == nil && params.EnableDurableMessages != nil && !*params.EnableDurableMessages {
			failed.Store(true)
			return map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "fixture disable failure"}}
		}
		return nil
	})
	defer cleanup()
	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": "s", "targetInfo": map[string]any{"targetId": "t", "type": "page"}}})
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		return m.optionalSessions["s"] == "test-script"
	}, time.Second, time.Millisecond)
	m.lifeMu.Lock()
	old := m.conn
	m.lifeMu.Unlock()
	require.NoError(t, m.SetTelemetry(false))
	require.Eventually(t, func() bool { return failed.Load() && old.ctx.Err() != nil }, time.Second, time.Millisecond)
	waitForTelemetryReconcile(t, m, false)
	// A replacement connection releases session-owned body storage.
	require.False(t, m.captureEnabled())
}

func TestTelemetryCleanupAttemptsAllSessionsAfterError(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	var mu sync.Mutex
	removed := make(map[string]bool)
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		switch msg.Method {
		case "Page.addScriptToEvaluateOnNewDocument":
			return map[string]any{"id": msg.ID, "result": map[string]any{"identifier": "script"}}
		case "Page.removeScriptToEvaluateOnNewDocument":
			return map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "fixture failure"}}
		case "Runtime.removeBinding":
			mu.Lock()
			removed[msg.SessionID] = true
			mu.Unlock()
		}
		return nil
	})
	m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	<-srv.connCh
	for _, id := range []string{"one", "two"} {
		srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": id, "targetInfo": map[string]any{"targetId": id, "type": "page"}}})
	}
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		return len(m.networkReady) == 2
	}, time.Second, time.Millisecond)
	require.NoError(t, m.SetTelemetry(true))
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		return m.optionalSessions["one"] == "script" && m.optionalSessions["two"] == "script"
	}, time.Second, time.Millisecond)
	m.lifeMu.Lock()
	old := m.conn
	m.lifeMu.Unlock()
	require.NoError(t, m.SetTelemetry(false))
	require.Eventually(t, func() bool { return old.ctx.Err() != nil }, time.Second, time.Millisecond)
	mu.Lock()
	one, two := removed["one"], removed["two"]
	mu.Unlock()
	require.True(t, one)
	require.True(t, two)
	waitForTelemetryReconcile(t, m, false)
	m.telemetryMu.RLock()
	enabled := m.telemetryEnabled
	m.telemetryMu.RUnlock()
	require.False(t, enabled)
}

func TestScriptRegistrationTimeoutReplacesConnection(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		if msg.Method == "Page.addScriptToEvaluateOnNewDocument" {
			return map[string]any{"method": "Test.unanswered"}
		}
		return nil
	})
	m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	m.lifeMu.Lock()
	old := m.conn
	m.lifeMu.Unlock()
	info := targetInfo{targetID: "t", targetType: "page"}
	m.sessionsMu.Lock()
	m.sessions["s"] = info
	m.sessionsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	m.telemetryMu.RLock()
	m.enableOptionalCapture(ctx, "s", info)
	m.telemetryMu.RUnlock()
	require.Error(t, old.ctx.Err(), "unknown script registration must invalidate its connection")
}
