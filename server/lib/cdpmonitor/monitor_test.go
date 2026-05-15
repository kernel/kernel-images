package cdpmonitor

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
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
		m.tryScreenshot(context.Background(), "Page.loadEventFired", "")
		require.Eventually(t, func() bool { return captureCount.Load() == 1 }, 2*time.Second, 20*time.Millisecond)

		ev := ec.waitFor(t, "monitor_screenshot", 2*time.Second)
		assert.Equal(t, events.System, ev.Category)
		assert.Equal(t, oapi.LocalProcess, ev.Source.Kind)
		require.NotNil(t, ev.Source.Event)
		assert.Equal(t, "Page.loadEventFired", *ev.Source.Event)
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.NotEmpty(t, data["png"])
	})

	t.Run("rate_limited", func(t *testing.T) {
		before := captureCount.Load()
		m.tryScreenshot(context.Background(), "Page.loadEventFired", "")
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, before, captureCount.Load(), "should be rate-limited within 2s")
	})

	t.Run("captures_after_cooldown", func(t *testing.T) {
		m.lastScreenshotAt.Store(time.Now().Add(-3 * time.Second).UnixMilli())
		before := captureCount.Load()
		m.tryScreenshot(context.Background(), "Page.loadEventFired", "")
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

// TestRedirectCounter verifies that redirect hops (same requestId, multiple
// requestWillBeSent) do not double-increment netPending, which would permanently
// block network_idle.
func TestRedirectCounter(t *testing.T) {
	m, ec := newComputedMonitor(t)
	navigateMonitor(m, "https://example.com")

	initiator := json.RawMessage(`{"type":"other"}`)
	// First requestWillBeSent — genuine new request.
	m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{
		RequestID: "r-redirect",
		Type:      "Document",
		Request:   cdpNetworkRequest{Method: "GET", URL: "https://example.com/old"},
		Initiator: initiator,
	}, "s1")

	// Second requestWillBeSent with the same requestId — this is the redirect hop.
	m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{
		RequestID: "r-redirect",
		Type:      "Document",
		Request:   cdpNetworkRequest{Method: "GET", URL: "https://example.com/new"},
		Initiator: initiator,
	}, "s1")

	// Only one loadingFinished fires per redirect chain.
	m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "r-redirect"}, "s1")

	// If netPending was double-incremented, network_idle would never fire.
	ec.waitFor(t, "network_idle", 2*time.Second)
}

// TestSubframeNavigationNoReset verifies that a frameNavigated event with a
// non-empty parentId does not reset computed state (netPending, timers, etc.).
func TestSubframeNavigationNoReset(t *testing.T) {
	m, ec := newComputedMonitor(t)
	navigateMonitor(m, "https://example.com") // top-level nav, sets mainSessionID

	// Start a request on the main frame.
	simulateRequest(m, "main-req")

	// An iframe navigates — should not reset state or clear pendingRequests.
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{
		Frame: cdpPageFrame{
			ID:       "iframe-frame",
			ParentID: "top-frame",
			URL:      "https://iframe.example.com",
		},
	}, "s1")

	// mainSessionID should still be "s1", not reset by the subframe nav.
	assert.Equal(t, "s1", m.mainSessionID.Load(), "mainSessionID should not change on subframe nav")

	// Finishing the main request should still drive network_idle (state not reset).
	simulateFinished(m, "main-req")
	ec.waitFor(t, "network_idle", 2*time.Second)
}

// TestIframeTargetNoStateMachine verifies that attaching an iframe target does
// not create a computedState. Only page targets get state machines; iframes share
// the CDP page domains but must not generate computed events like navigation_settled.
func TestIframeTargetNoStateMachine(t *testing.T) {
	m, _ := newComputedMonitor(t)
	m.sessionsMu.Lock()
	m.sessions["iframe-session"] = targetInfo{targetID: "iframe-target", targetType: "iframe"}
	// Intentionally do NOT create a computedState — mirrors handleAttachedToTarget behaviour.
	m.sessionsMu.Unlock()

	m.sessionsMu.RLock()
	cs := m.computedStates["iframe-session"]
	m.sessionsMu.RUnlock()

	assert.Nil(t, cs, "iframe target must not have a computedState")
}
