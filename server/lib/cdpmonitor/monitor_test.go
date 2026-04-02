package cdpmonitor

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger is defined in cdp_test.go (package-level, shared across test files).

func TestAutoAttach(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	ec := newEventCollector()
	upstream := newTestUpstream(srv.wsURL())
	m := New(upstream, ec.publishFn(), 99, discardLogger)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()

	msg := srv.readFromMonitor(t, 3*time.Second)
	assert.Equal(t, "Target.setAutoAttach", msg.Method)

	var params struct {
		AutoAttach             bool `json:"autoAttach"`
		WaitForDebuggerOnStart bool `json:"waitForDebuggerOnStart"`
		Flatten                bool `json:"flatten"`
	}
	require.NoError(t, json.Unmarshal(msg.Params, &params))
	assert.True(t, params.AutoAttach)
	assert.False(t, params.WaitForDebuggerOnStart)
	assert.True(t, params.Flatten)

	stopResponder := make(chan struct{})
	go listenAndRespond(srv, stopResponder, nil)
	defer close(stopResponder)
	srv.sendToMonitor(t, map[string]any{"id": msg.ID, "result": map[string]any{}})

	srv.sendToMonitor(t, map[string]any{
		"method": "Target.attachedToTarget",
		"params": map[string]any{
			"sessionId":  "session-abc",
			"targetInfo": map[string]any{"targetId": "target-xyz", "type": "page", "url": "https://example.com"},
		},
	})
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		_, ok := m.sessions["session-abc"]
		return ok
	}, 2*time.Second, 50*time.Millisecond, "session not stored")

	m.sessionsMu.RLock()
	info := m.sessions["session-abc"]
	m.sessionsMu.RUnlock()
	assert.Equal(t, "target-xyz", info.targetID)
	assert.Equal(t, "page", info.targetType)
}

func TestLifecycle(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	ec := newEventCollector()
	upstream := newTestUpstream(srv.wsURL())
	m := New(upstream, ec.publishFn(), 99, discardLogger)

	assert.False(t, m.IsRunning(), "idle at boot")

	require.NoError(t, m.Start(context.Background()))
	assert.True(t, m.IsRunning(), "running after Start")
	srv.readFromMonitor(t, 2*time.Second)

	m.Stop()
	assert.False(t, m.IsRunning(), "stopped after Stop")

	require.NoError(t, m.Start(context.Background()))
	assert.True(t, m.IsRunning(), "running after second Start")
	srv.readFromMonitor(t, 2*time.Second)

	require.NoError(t, m.Start(context.Background()))
	assert.True(t, m.IsRunning(), "running after implicit restart")

	m.Stop()
	assert.False(t, m.IsRunning(), "stopped at end")
}

func TestReconnect(t *testing.T) {
	srv1 := newTestServer(t)

	upstream := newTestUpstream(srv1.wsURL())
	ec := newEventCollector()
	m := New(upstream, ec.publishFn(), 99, discardLogger)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()

	srv1.readFromMonitor(t, 2*time.Second)

	srv2 := newTestServer(t)
	defer srv2.close()
	defer srv1.close()

	upstream.notifyRestart(srv2.wsURL())

	ec.waitFor(t, "monitor_disconnected", 3*time.Second)
	srv2.readFromMonitor(t, 5*time.Second)

	ev := ec.waitFor(t, "monitor_reconnected", 3*time.Second)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	_, ok := data["reconnect_duration_ms"]
	assert.True(t, ok, "missing reconnect_duration_ms")
}

func TestAttachExistingTargets(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	responder := func(msg cdpMessage) any {
		switch msg.Method {
		case "Target.getTargets":
			return map[string]any{
				"id": msg.ID,
				"result": map[string]any{
					"targetInfos": []any{
						map[string]any{"targetId": "existing-1", "type": "page", "url": "https://preexisting.example.com"},
					},
				},
			}
		case "Target.attachToTarget":
			srv.sendToMonitor(t, map[string]any{
				"method": "Target.attachedToTarget",
				"params": map[string]any{
					"sessionId":  "session-existing-1",
					"targetInfo": map[string]any{"targetId": "existing-1", "type": "page", "url": "https://preexisting.example.com"},
				},
			})
			return map[string]any{"id": msg.ID, "result": map[string]any{"sessionId": "session-existing-1"}}
		}
		return nil
	}

	m, _, cleanup := startMonitor(t, srv, responder)
	defer cleanup()

	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		_, ok := m.sessions["session-existing-1"]
		return ok
	}, 3*time.Second, 50*time.Millisecond, "existing target not auto-attached")

	m.sessionsMu.RLock()
	info := m.sessions["session-existing-1"]
	m.sessionsMu.RUnlock()
	assert.Equal(t, "existing-1", info.targetID)
}

func TestScreenshot(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	m, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()

	var captureCount atomic.Int32
	m.screenshotFn = func(ctx context.Context, displayNum int) ([]byte, error) {
		captureCount.Add(1)
		return minimalPNG, nil
	}

	t.Run("capture_and_publish", func(t *testing.T) {
		m.tryScreenshot(context.Background())
		require.Eventually(t, func() bool { return captureCount.Load() == 1 }, 2*time.Second, 20*time.Millisecond)

		ev := ec.waitFor(t, "screenshot", 2*time.Second)
		assert.Equal(t, events.CategorySystem, ev.Category)
		assert.Equal(t, events.KindLocalProcess, ev.Source.Kind)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.NotEmpty(t, data["png"])
	})

	t.Run("rate_limited", func(t *testing.T) {
		before := captureCount.Load()
		m.tryScreenshot(context.Background())
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, before, captureCount.Load(), "should be rate-limited within 2s")
	})

	t.Run("captures_after_cooldown", func(t *testing.T) {
		m.lastScreenshotAt.Store(time.Now().Add(-3 * time.Second).UnixMilli())
		before := captureCount.Load()
		m.tryScreenshot(context.Background())
		require.Eventually(t, func() bool { return captureCount.Load() > before }, 2*time.Second, 20*time.Millisecond)
	})
}
