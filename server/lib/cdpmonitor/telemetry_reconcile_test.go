package cdpmonitor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

func TestInteractionObligationsUseTargetIdentity(t *testing.T) {
	m := New(newTestUpstream(""), newEventCollector().publishFn(), 0, discardLogger, nil)
	m.sessions["old-session"] = targetInfo{targetID: "live-target", targetType: "page"}
	m.optionalSessions["old-session"] = "script"
	m.interactionTargets["live-target"] = struct{}{}
	m.handleDetachedFromTarget(cdpTargetDetachedFromTargetParams{SessionID: "old-session"})
	m.clearState()
	require.Contains(t, m.interactionTargets, "live-target")
	require.NoError(t, m.cleanupAttachedTarget(context.Background(), "old-session", targetInfo{targetID: "different-target", targetType: "page"}))
	require.Contains(t, m.interactionTargets, "live-target", "a reused session ID cannot discharge another target's obligation")
	m.handleSurfaceEvent(nil, browsersurface.Event{Kind: browsersurface.EventProtocol, Message: cdpclient.Message{Method: "Target.targetDestroyed", Params: []byte(`{"targetId":"live-target"}`)}})
	require.Empty(t, m.interactionTargets)
}

func TestTelemetryRevisionFencesPendingBody(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	blocked := make(chan cdpMessage, 1)
	var first atomic.Bool
	m, ec, cleanup := startMonitor(t, srv, func(msg cdpMessage) any {
		if msg.Method == "Network.getResponseBody" {
			if first.CompareAndSwap(false, true) {
				blocked <- msg
				return map[string]any{"method": "Test.unanswered"}
			}
			return map[string]any{"id": msg.ID, "result": map[string]any{"body": "new-body"}}
		}
		return nil
	})
	defer cleanup()
	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": "s", "targetInfo": map[string]any{"targetId": "t", "type": "page"}}})
	ec.waitFor(t, EventTabOpened, time.Second)
	request := func(id string) {
		for _, event := range []map[string]any{
			{"method": "Network.requestWillBeSent", "params": map[string]any{"requestId": id, "type": "Fetch", "request": map[string]any{"method": "GET", "url": "http://fixture/" + id}}},
			{"method": "Network.responseReceived", "params": map[string]any{"requestId": id, "response": map[string]any{"status": 200, "mimeType": "text/plain"}}},
			{"method": "Network.loadingFinished", "params": map[string]any{"requestId": id}},
		} {
			event["sessionId"] = "s"
			srv.sendToMonitor(t, event)
		}
	}
	request("old")
	var command cdpMessage
	select {
	case command = <-blocked:
	case <-time.After(time.Second):
		t.Fatal("body capture did not start")
	}
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.SetTelemetry(true))
	srv.sendToMonitor(t, map[string]any{"id": command.ID, "result": map[string]any{"body": "old-body"}})
	waitForTelemetryReconcile(t, m, true)
	ec.mu.Lock()
	responses := 0
	for _, event := range ec.events {
		if event.Type == EventNetworkResponse {
			responses++
		}
	}
	ec.mu.Unlock()
	require.Zero(t, responses, "old body escaped into the new capture revision")
	request("new")
	ec.waitFor(t, EventNetworkResponse, time.Second)
	require.Equal(t, uint64(2), m.NetworkSnapshot().Completed)
}

func TestTelemetrySchedulesOnlyReadyAttachments(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	m, _, cleanup := startMonitor(t, srv, nil)
	defer cleanup()
	require.NoError(t, m.SetTelemetry(false))
	waitForTelemetryReconcile(t, m, false)
	m.sessionsMu.Lock()
	m.sessions["pending"] = targetInfo{targetID: "pending-target", targetType: "page"}
	m.sessions["ready"] = targetInfo{targetID: "ready-target", targetType: "page"}
	m.networkReady["ready"] = true
	m.sessionsMu.Unlock()

	// Run reconciliation synchronously so all optional tasks have been scheduled
	// before joining them. The pending attachment is deliberately not advanced.
	m.restartMu.Lock()
	require.NoError(t, m.SetTelemetry(true))
	err := m.applyTelemetry(m.desiredTelemetry.Load())
	m.captureWg.Wait()
	m.restartMu.Unlock()
	require.NoError(t, err)
	m.sessionsMu.RLock()
	_, pending := m.optionalSessions["pending"]
	_, ready := m.optionalSessions["ready"]
	_, dirtyPending := m.interactionTargets["pending-target"]
	m.sessionsMu.RUnlock()
	require.False(t, pending, "optional setup ran before attachment recovery")
	require.False(t, dirtyPending)
	require.True(t, ready, "ready attachments must still enable capture")
}

func waitForTelemetryReconcile(t *testing.T, m *Monitor, enabled bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		desired := m.desiredTelemetry.Load()
		if (desired&1 != 0) != enabled || m.appliedTelemetry.Load() != desired || m.telemetryChanging.Load() {
			return false
		}
		m.sessionsMu.RLock()
		cleaned := len(m.optionalSessions) == 0 && len(m.interactionTargets) == 0
		m.sessionsMu.RUnlock()
		return (enabled || cleaned) && m.NetworkSnapshot().Up
	}, 10*time.Second, 10*time.Millisecond, "telemetry reconciliation did not settle")
}
