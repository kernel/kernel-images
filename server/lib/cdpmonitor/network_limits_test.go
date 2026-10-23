package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestExpiredRequestCannotMakeNewCaptureIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		defer m.Stop()
		navigateMonitor(m, "https://example.com/old")
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "old"}, "s1")
		time.Sleep(pendingRequestTTL + time.Second)
		expired := m.takeExpiredNetworkRequests(time.Now())
		require.Len(t, expired, 1)

		// Reconnect replaces capture state after expiry removes the request but
		// before the sweeper applies its pending-count decrement.
		m.clearState()
		navigateMonitor(m, "https://example.com/new")
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "new"}, "s1")
		m.finishNetworkRequest(expired[0])
		time.Sleep(networkIdleDebounce + time.Millisecond)
		synctest.Wait()
		ec.mu.Lock()
		idleEvents := 0
		for _, event := range ec.events {
			if event.Type == EventNetworkIdle {
				idleEvents++
			}
		}
		ec.mu.Unlock()
		require.Zero(t, idleEvents, "an old expiry made the new capture idle")

		m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "new"}, "s1")
		time.Sleep(networkIdleDebounce + time.Millisecond)
		event := ec.waitFor(t, EventNetworkIdle, time.Second)
		var payload struct {
			URL string `json:"url"`
		}
		require.NoError(t, json.Unmarshal(event.Data, &payload))
		require.Equal(t, "https://example.com/new", payload.URL)
	})
}

func TestNetworkExchangeLimitPreservesCapture(t *testing.T) {
	for _, terminal := range []string{EventNetworkResponse, EventNetworkLoadingFailed} {
		t.Run(terminal, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, ec := newComputedMonitor(t)
				defer m.Stop()
				for i := range maxNetworkExchanges {
					m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: fmt.Sprint(i)}, "s")
				}
				m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "overflow", Request: cdpNetworkRequest{URL: "https://example.com/start", Headers: json.RawMessage(`{"Base":"start"}`)}}, "s")
				m.pendReqMu.Lock()
				size := len(m.networkExchanges)
				m.pendReqMu.Unlock()
				require.Equal(t, maxNetworkExchanges, size, "ordinary requests must respect the header registry limit")

				// Free a slot while the overflow chain remains active. Its later
				// hops must not start consuming headers from the middle of the chain.
				m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "0"}, "s")
				m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "0"}, "s")
				time.Sleep(networkHeaderWait + time.Millisecond)
				synctest.Wait()
				m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "overflow", Headers: json.RawMessage(`{"Cookie":"wrong-hop"}`)}, "s")
				m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "overflow", RedirectHasExtraInfo: true, RedirectResponse: json.RawMessage(`{"status":302}`), Request: cdpNetworkRequest{URL: "https://example.com/end", Headers: json.RawMessage(`{"Base":"end"}`)}}, "s")
				m.pendReqMu.Lock()
				_, readmitted := m.networkExchanges[networkRequestKey{"s", "overflow"}]
				m.pendReqMu.Unlock()
				require.False(t, readmitted, "an overflow redirect must not restart header correlation mid-chain")
				if terminal == EventNetworkResponse {
					m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "overflow", HasExtraInfo: true, Response: cdpNetworkResponse{Status: 200}}, "s")
					m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "overflow"}, "s")
				} else {
					m.handleLoadingFailed(cdpNetworkLoadingFailedParams{RequestID: "overflow", ErrorText: "net::ERR_ABORTED", Canceled: true}, "s")
				}
				// An admitted chain still receives late headers at capacity.
				m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "1", Headers: json.RawMessage(`{"Cookie":"retained"}`)}, "s")
				m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "1", HasExtraInfo: true}, "s")
				m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "1"}, "s")
				time.Sleep(networkHeaderWait + time.Millisecond)
				synctest.Wait()
				ec.mu.Lock()
				defer ec.mu.Unlock()
				var sequence []string
				retainedHeaders := false
				for _, event := range ec.events {
					var payload struct {
						RequestID string `json:"request_id"`
					}
					require.NoError(t, json.Unmarshal(event.Data, &payload))
					if payload.RequestID == "1" && event.Type == EventNetworkResponse {
						var response oapi.BrowserNetworkResponseEventData
						require.NoError(t, json.Unmarshal(event.Data, &response))
						require.NotNil(t, response.RequestHeaders)
						require.Equal(t, "retained", (*response.RequestHeaders)["Cookie"])
						retainedHeaders = true
					}
					if payload.RequestID != "overflow" {
						continue
					}
					sequence = append(sequence, event.Type)
					if event.Type == EventNetworkRequest {
						var request oapi.BrowserNetworkRequestEventData
						require.NoError(t, json.Unmarshal(event.Data, &request))
						require.NotNil(t, request.Url)
						require.Equal(t, strings.TrimPrefix(*request.Url, "https://example.com/"), request.Headers["Base"])
						require.NotContains(t, request.Headers, "Cookie")
					}
					if event.Type == EventNetworkResponse {
						var response oapi.BrowserNetworkResponseEventData
						require.NoError(t, json.Unmarshal(event.Data, &response))
						require.Nil(t, response.RequestHeaders)
					}
				}
				require.Equal(t, []string{EventNetworkRequest, EventNetworkResponse, EventNetworkRequest, terminal}, sequence)
				require.True(t, retainedHeaders, "capacity fallback affected an admitted chain")
				require.Less(t, len(m.networkExchanges), maxNetworkExchanges, "terminal publications must release slots")
			})
		})
	}
}

func TestTelemetryRestartRestoresHeadersAfterSaturation(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	m, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()
	for i := range maxNetworkExchanges + 1 {
		m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: fmt.Sprint(i), Headers: json.RawMessage(`{}`)}, "")
	}
	require.NoError(t, m.SetTelemetry(false))
	waitForTelemetryReconcile(t, m, false)
	require.NoError(t, m.SetTelemetry(true))
	waitForTelemetryReconcile(t, m, true)

	sendNetworkFixture(t, srv, "Network.requestWillBeSentExtraInfo", cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "fresh", Headers: json.RawMessage(`{"Cookie":"fresh-capture"}`)})
	sendNetworkFixture(t, srv, "Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "fresh", Request: cdpNetworkRequest{URL: "https://example.com/fresh"}})
	sendNetworkFixture(t, srv, "Network.responseReceived", cdpNetworkResponseReceivedParams{RequestID: "fresh", HasExtraInfo: true})
	sendNetworkFixture(t, srv, "Network.loadingFinished", cdpNetworkLoadingFinishedParams{RequestID: "fresh"})
	event := ec.waitFor(t, EventNetworkResponse, 2*time.Second)
	var response oapi.BrowserNetworkResponseEventData
	require.NoError(t, json.Unmarshal(event.Data, &response))
	require.NotNil(t, response.RequestHeaders)
	require.Equal(t, "fresh-capture", (*response.RequestHeaders)["Cookie"])
}

func TestNetworkExtraInfoOverflowCannotShiftRedirectHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		defer m.Stop()
		for i := range maxNetworkExchanges {
			m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: fmt.Sprint(i)}, "s")
		}
		// The first hop's actual headers arrive while all slots are occupied.
		m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "late-base", Headers: json.RawMessage(`{"Cookie":"first-hop"}`)}, "s")
		m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "0"}, "s")
		m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "0"}, "s")
		time.Sleep(networkHeaderWait + time.Millisecond)
		synctest.Wait()
		// Capacity becomes available before the chain's base event is received.
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "late-base", Request: cdpNetworkRequest{URL: "https://example.com/start", Headers: json.RawMessage(`{"Base":"start"}`)}}, "s")
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "late-base", RedirectHasExtraInfo: true, RedirectResponse: json.RawMessage(`{"status":302}`), Request: cdpNetworkRequest{URL: "https://example.com/end", Headers: json.RawMessage(`{"Base":"end"}`)}}, "s")
		m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "late-base", Headers: json.RawMessage(`{"Cookie":"second-hop"}`)}, "s")
		m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "late-base", HasExtraInfo: true, Response: cdpNetworkResponse{Status: 200}}, "s")
		m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "late-base"}, "s")
		time.Sleep(networkHeaderWait + time.Millisecond)
		synctest.Wait()
		event := waitForNetworkURL(t, ec, EventNetworkResponse, "https://example.com/start")
		var response oapi.BrowserNetworkResponseEventData
		require.NoError(t, json.Unmarshal(event.Data, &response))
		require.Nil(t, response.RequestHeaders, "a rejected ExtraInfo-first chain must not resume correlation from its second hop")

		// A fresh capture can safely enrich new chains again.
		m.clearState()
		m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "fresh", Headers: json.RawMessage(`{"Cookie":"fresh-capture"}`)}, "s")
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "fresh", Request: cdpNetworkRequest{URL: "https://example.com/fresh"}}, "s")
		m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "fresh", HasExtraInfo: true}, "s")
		m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "fresh"}, "s")
		synctest.Wait()
		event = waitForNetworkURL(t, ec, EventNetworkResponse, "https://example.com/fresh")
		require.NoError(t, json.Unmarshal(event.Data, &response))
		require.NotNil(t, response.RequestHeaders)
		require.Equal(t, "fresh-capture", (*response.RequestHeaders)["Cookie"])
	})
}
