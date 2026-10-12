package cdpmonitor

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type extensionRecord struct {
	attached []Session
	removed  []string
	events   []string
	resets   int
}

type recordingExtension struct {
	mu sync.Mutex
	extensionRecord
}

func (e *recordingExtension) SessionAttached(s Session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attached = append(e.attached, s)
}

func (e *recordingExtension) SessionRemoved(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, sessionID)
}

func (e *recordingExtension) ProtocolEvent(sessionID, method string, _ json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, sessionID+" "+method)
}

func (e *recordingExtension) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resets++
}

func (e *recordingExtension) snapshot() extensionRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return extensionRecord{attached: slices.Clone(e.attached), removed: slices.Clone(e.removed), events: slices.Clone(e.events), resets: e.resets}
}

func TestExtensionReceivesSessionsWithTelemetryOff(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	stop := make(chan struct{})
	defer close(stop)
	var mu sync.Mutex
	var sent []cdpMessage
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		mu.Lock()
		sent = append(sent, msg)
		mu.Unlock()
		return nil
	})
	ext := &recordingExtension{}
	m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
	m.SetExtension(ext)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(context.Background()))
	<-srv.connCh

	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{
		"sessionId": "s", "targetInfo": map[string]any{"targetId": "frame", "type": "iframe", "parentFrameId": "parent"},
	}})
	require.Eventually(t, func() bool { return len(ext.snapshot().attached) == 1 }, 3*time.Second, 10*time.Millisecond)
	session := ext.snapshot().attached[0]
	require.Equal(t, "s", session.ID)
	require.Equal(t, "frame", session.TargetID)
	require.Equal(t, "iframe", session.TargetType)
	require.Equal(t, "parent", session.ParentFrameID)

	_, err := session.Send(context.Background(), "Runtime.enable", nil)
	require.NoError(t, err)
	mu.Lock()
	require.True(t, slices.ContainsFunc(sent, func(msg cdpMessage) bool { return msg.Method == "Runtime.enable" && msg.SessionID == "s" }))
	mu.Unlock()

	srv.sendToMonitor(t, map[string]any{"method": "Runtime.bindingCalled", "sessionId": "s", "params": map[string]any{"name": "x", "payload": "{}"}})
	require.Eventually(t, func() bool { return slices.Contains(ext.snapshot().events, "s Runtime.bindingCalled") }, 3*time.Second, 10*time.Millisecond)

	srv.sendToMonitor(t, map[string]any{"method": "Target.detachedFromTarget", "params": map[string]any{"sessionId": "s"}})
	require.Eventually(t, func() bool { return slices.Contains(ext.snapshot().removed, "s") }, 3*time.Second, 10*time.Millisecond)

	m.Stop()
	require.Positive(t, ext.snapshot().resets)
}

func TestRetainedPageDomainsSurviveTelemetryCleanup(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	var mu sync.Mutex
	var methods []string
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		mu.Lock()
		methods = append(methods, msg.Method)
		mu.Unlock()
		if msg.Method == "Page.addScriptToEvaluateOnNewDocument" {
			return map[string]any{"id": msg.ID, "result": map[string]any{"identifier": "script"}}
		}
		return nil
	})
	m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
	m.RetainPageDomains()
	require.NoError(t, m.SetTelemetry(true))
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	<-srv.connCh
	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": "s", "targetInfo": map[string]any{"targetId": "t", "type": "page"}}})
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(methods, "Runtime.evaluate")
	}, 3*time.Second, time.Millisecond)

	require.NoError(t, m.SetTelemetry(false))
	waitForTelemetryReconcile(t, m, false)
	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, methods, "Runtime.removeBinding")
	require.Contains(t, methods, "Page.removeScriptToEvaluateOnNewDocument")
	require.NotContains(t, methods, "Page.disable")
	require.NotContains(t, methods, "Runtime.disable")
}

func TestReleasePageDomainsRestoresTelemetryCleanup(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	var mu sync.Mutex
	var sent []cdpMessage
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		mu.Lock()
		sent = append(sent, msg)
		mu.Unlock()
		if msg.Method == "Page.addScriptToEvaluateOnNewDocument" {
			return map[string]any{"id": msg.ID, "result": map[string]any{"identifier": "script"}}
		}
		return nil
	})
	disables := func(sessionID string) []string {
		mu.Lock()
		defer mu.Unlock()
		var methods []string
		for _, msg := range sent {
			if msg.SessionID == sessionID && (msg.Method == "Page.disable" || msg.Method == "Runtime.disable") {
				methods = append(methods, msg.Method)
			}
		}
		return methods
	}
	m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	<-srv.connCh
	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": "s", "targetInfo": map[string]any{"targetId": "t", "type": "page"}}})
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		return m.networkReady["s"]
	}, 3*time.Second, 10*time.Millisecond)

	// Telemetry is off, so release disables what the extension had enabled.
	m.RetainPageDomains()
	require.NoError(t, m.ReleasePageDomains(context.Background()))
	require.Equal(t, []string{"Page.disable", "Runtime.disable"}, disables("s"))

	// A session telemetry captures keeps its domains until telemetry cleanup,
	// which disables them again once nothing retains them.
	require.NoError(t, m.SetTelemetry(true))
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		return m.optionalSessions["s"] != ""
	}, 3*time.Second, 10*time.Millisecond)
	m.RetainPageDomains()
	require.NoError(t, m.ReleasePageDomains(context.Background()))
	require.Len(t, disables("s"), 2)
	require.NoError(t, m.SetTelemetry(false))
	waitForTelemetryReconcile(t, m, false)
	require.Equal(t, []string{"Page.disable", "Runtime.disable", "Page.disable", "Runtime.disable"}, disables("s"))
}
