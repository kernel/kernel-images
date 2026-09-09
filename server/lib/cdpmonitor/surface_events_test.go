package cdpmonitor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestSurfaceDropsNetworkEventsForUnknownAndRemovedSessions(t *testing.T) {
	m, ec := newComputedMonitor(t)
	defer m.Stop()
	params, err := json.Marshal(cdpNetworkRequestWillBeSentParams{
		RequestID: "request", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/"},
	})
	require.NoError(t, err)
	event := browsersurface.Event{Kind: browsersurface.EventProtocol, Message: cdpclient.Message{
		Method: "Network.requestWillBeSent", SessionID: "session", Params: params,
	}}
	m.handleSurfaceEvent(nil, event)
	require.Empty(t, m.pendingRequests)
	require.Empty(t, ec.events)

	m.sessions["session"] = targetInfo{targetID: "frame", targetType: "iframe"}
	m.handleSurfaceEvent(nil, event)
	require.Len(t, m.pendingRequests, 1)
	require.Len(t, ec.events, 1)

	m.handleSurfaceEvent(nil, browsersurface.Event{Kind: browsersurface.EventSessionRemoved, SessionID: "session"})
	m.handleSurfaceEvent(nil, event)
	require.Empty(t, m.pendingRequests)
	require.Len(t, ec.events, 1, "a removed session must not resume emitting metadata-free requests")
}

func TestSurfaceCarriesOOPIFParentFrameIntoNavigation(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	m, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()
	srv.sendToMonitor(t, map[string]any{
		"method": "Target.attachedToTarget",
		"params": map[string]any{
			"sessionId":  "iframe-session",
			"targetInfo": map[string]any{"targetId": "frame", "type": "iframe", "parentFrameId": "parent-frame"},
		},
	})
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		return m.sessions["iframe-session"].parentFrameID == "parent-frame"
	}, time.Second, 10*time.Millisecond)

	for _, parentID := range []string{"", "explicit-parent"} {
		checkpoint := ec.checkpoint()
		srv.sendToMonitor(t, map[string]any{
			"method": "Page.frameNavigated", "sessionId": "iframe-session",
			"params": map[string]any{"frame": map[string]any{
				"id": "frame", "url": "https://other.example/", "parentId": parentID,
			}},
		})
		event := ec.waitForNew(t, EventNavigation, checkpoint, time.Second)
		var data oapi.BrowserPageNavigationEventData
		require.NoError(t, json.Unmarshal(event.Data, &data))
		require.NotNil(t, data.ParentFrameId)
		expected := parentID
		if expected == "" {
			expected = "parent-frame"
		}
		require.Equal(t, expected, *data.ParentFrameId)
		require.Equal(t, mainSessionUnset, m.mainSessionID.Load())
	}
}
