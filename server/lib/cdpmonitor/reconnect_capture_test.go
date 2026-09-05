package cdpmonitor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/require"
)

func TestReconnectDrainsPendingResponseCaptureBeforeOpeningConnection(t *testing.T) {
	oldServer := newTestServer(t)
	defer oldServer.close()
	newServer := newTestServer(t)
	defer newServer.close()
	upstream := newTestUpstream(oldServer.wsURL())
	ec := newEventCollector()
	publish := ec.publishFn()
	publishingResponse := make(chan struct{})
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResponse) }) }
	m := New(upstream, func(event events.Event) (events.Envelope, bool) {
		if event.Type == EventNetworkResponse {
			close(publishingResponse)
			<-releaseResponse
		}
		return publish(event)
	}, 0, discardLogger, nil)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	defer release()

	requestedBody := make(chan struct{})
	stopResponders := make(chan struct{})
	defer close(stopResponders)
	go listenAndRespond(oldServer, stopResponders, func(msg cdpMessage) any {
		if msg.Method == "Network.getResponseBody" {
			close(requestedBody)
			// Leave the command unanswered until closing the connection cancels it.
			return map[string]any{"method": "Test.pendingResponse"}
		}
		return nil
	})
	go listenAndRespond(newServer, stopResponders, nil)
	select {
	case <-oldServer.connCh:
	case <-time.After(time.Second):
		t.Fatal("old connection was not accepted")
	}
	oldServer.sendToMonitor(t, map[string]any{
		"method": "Target.attachedToTarget",
		"params": map[string]any{
			"sessionId": "old-session", "targetInfo": map[string]any{"targetId": "old-target", "type": "page"},
		},
	})
	ec.waitFor(t, EventTabOpened, time.Second)
	for _, message := range []map[string]any{
		{"method": "Network.requestWillBeSent", "params": map[string]any{
			"requestId": "request", "type": "Fetch", "request": map[string]any{"method": "GET", "url": "https://example.com/"},
		}},
		{"method": "Network.responseReceived", "params": map[string]any{
			"requestId": "request", "response": map[string]any{"status": 200, "mimeType": "text/plain"},
		}},
		{"method": "Network.loadingFinished", "params": map[string]any{"requestId": "request"}},
	} {
		message["sessionId"] = "old-session"
		oldServer.sendToMonitor(t, message)
	}
	select {
	case <-requestedBody:
	case <-time.After(time.Second):
		t.Fatal("response capture did not start")
	}
	upstream.notifyRestart(newServer.wsURL())
	select {
	case <-publishingResponse:
	case <-time.After(time.Second):
		t.Fatal("restart did not cancel the pending body command")
	}
	require.Never(t, func() bool {
		select {
		case <-newServer.connCh:
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 10*time.Millisecond, "new connection opened before old capture drained")
	release()
	ec.waitFor(t, EventMonitorReconnected, 3*time.Second)
	m.sessionsMu.RLock()
	_, oldSession := m.sessions["old-session"]
	m.sessionsMu.RUnlock()
	require.False(t, oldSession)

	ec.mu.Lock()
	defer ec.mu.Unlock()
	responseIndex, reconnectedIndex := -1, -1
	for i, event := range ec.events {
		if event.Type == EventNetworkResponse {
			responseIndex = i
		}
		if event.Type == EventMonitorReconnected {
			reconnectedIndex = i
		}
	}
	require.GreaterOrEqual(t, responseIndex, 0)
	require.Greater(t, reconnectedIndex, responseIndex)
}
