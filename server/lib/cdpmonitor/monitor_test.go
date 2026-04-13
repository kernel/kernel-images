package cdpmonitor

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger is defined in cdp_test.go (package-level, shared across test files).

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

// TestFailPendingCommandsUnblocksSend verifies that clearState (called during
// reconnect) unblocks any goroutine blocked in send() by delivering an error.
func TestFailPendingCommandsUnblocksSend(t *testing.T) {
	ec := newEventCollector()
	upstream := newTestUpstream("ws://127.0.0.1:0")
	m := New(upstream, ec.publishFn(), 0, discardLogger)

	// Pre-register a fake pending command channel as if send() had registered it.
	id := int64(42)
	ch := make(chan cdpMessage, 1)
	m.pendMu.Lock()
	m.pending[id] = ch
	m.pendMu.Unlock()

	// failPendingCommands should deliver an error message to ch without blocking.
	done := make(chan struct{})
	go func() {
		m.failPendingCommands()
		close(done)
	}()

	select {
	case msg := <-ch:
		require.NotNil(t, msg.Error, "expected error response from failPendingCommands")
		assert.Equal(t, -1, msg.Error.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("failPendingCommands did not unblock the pending channel")
	}
	<-done
}

// TestInitSessionAutoAttachFailure verifies that a monitor_init_failed event is
// published (and the monitor logs the failure) when Target.setAutoAttach returns
// an error.
func TestInitSessionAutoAttachFailure(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()

	ec := newEventCollector()
	upstream := newTestUpstream(srv.wsURL())
	m := New(upstream, ec.publishFn(), 99, discardLogger)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()

	stopResponder := make(chan struct{})
	defer close(stopResponder)

	go listenAndRespond(srv, stopResponder, func(msg cdpMessage) any {
		if msg.Method == "Target.setAutoAttach" {
			return map[string]any{
				"id":    msg.ID,
				"error": map[string]any{"code": -32601, "message": "Method not found"},
			}
		}
		return nil
	})

	ec.waitFor(t, EventMonitorInitFailed, 3*time.Second)
}
