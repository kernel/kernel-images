package cdpmonitor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestNetworkRequestIDsAreSessionScoped(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	m, ec, cleanup := startMonitor(t, srv, func(msg cdpMessage) any {
		if msg.Method == "Network.getResponseBody" {
			return map[string]any{"id": msg.ID, "result": map[string]any{"body": msg.SessionID}}
		}
		return nil
	})
	defer cleanup()

	for _, sessionID := range []string{"page-session", "iframe-session"} {
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{
			RequestID: "same-id", Type: "Fetch",
			Request: cdpNetworkRequest{Method: "POST", URL: "https://example.com/" + sessionID, PostData: sessionID},
		}, sessionID)
		m.handleResponseReceived(cdpNetworkResponseReceivedParams{
			RequestID: "same-id", Response: cdpNetworkResponse{Status: 200, MimeType: "text/plain"},
		}, sessionID)
	}
	m.pendReqMu.Lock()
	pending := len(m.pendingRequests)
	m.pendReqMu.Unlock()
	require.Equal(t, 2, pending)

	m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "same-id"}, "page-session")
	m.handleLoadingFailed(cdpNetworkLoadingFailedParams{RequestID: "same-id", Canceled: true, ErrorText: "net::ERR_ABORTED"}, "iframe-session")

	responseEvent := waitForNetworkURL(t, ec, EventNetworkResponse, "https://example.com/page-session")
	var response oapi.BrowserNetworkResponseEventData
	require.NoError(t, json.Unmarshal(responseEvent.Data, &response))
	require.Equal(t, "page-session", response.SessionId)
	require.NotNil(t, response.Body)
	require.Equal(t, "page-session", *response.Body)

	failedEvent := waitForNetworkURL(t, ec, EventNetworkLoadingFailed, "https://example.com/iframe-session")
	var failure oapi.BrowserNetworkLoadingFailedEventData
	require.NoError(t, json.Unmarshal(failedEvent.Data, &failure))
	require.Equal(t, "iframe-session", failure.SessionId)
	require.True(t, failure.Canceled)
	m.pendReqMu.Lock()
	pending = len(m.pendingRequests)
	m.pendReqMu.Unlock()
	require.Zero(t, pending)
}

func TestDetachClearsOnlyThatSessionsRequests(t *testing.T) {
	m, _ := newComputedMonitor(t)
	for _, sessionID := range []string{"s1", "child"} {
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{
			RequestID: "same-id", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/"},
		}, sessionID)
	}
	m.handleDetachedFromTarget(cdpTargetDetachedFromTargetParams{SessionID: "child"})
	m.pendReqMu.Lock()
	_, parent := m.pendingRequests[networkRequestKey{"s1", "same-id"}]
	_, child := m.pendingRequests[networkRequestKey{"child", "same-id"}]
	m.pendReqMu.Unlock()
	require.True(t, parent)
	require.False(t, child)
}

func TestOOPIFLocalRootDoesNotBecomeMainPage(t *testing.T) {
	m, _ := newComputedMonitor(t)
	navigateMonitor(m, "https://example.com/")
	m.sessionsMu.Lock()
	m.sessions["child"] = targetInfo{targetID: "child-target", targetType: "iframe"}
	m.sessionsMu.Unlock()
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{
		Frame: cdpPageFrame{ID: "child-target", URL: "https://other.example/"},
	}, "child")
	require.Equal(t, "s1", m.mainSessionID.Load())
}
