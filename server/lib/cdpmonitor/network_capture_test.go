package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func sendNetworkFixture(t *testing.T, srv *testServer, method string, params any) {
	t.Helper()
	srv.sendToMonitor(t, map[string]any{"method": method, "params": params})
}

func TestNetworkTimestampsReflectObservation(t *testing.T) {
	for _, typ := range []string{EventNetworkResponse, EventNetworkLoadingFailed, EventProxyError} {
		t.Run(typ, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, ec := newComputedMonitor(t)
				defer m.Stop()
				requestAt := time.Now().UnixMicro()
				m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/slow"}}, "s")
				time.Sleep(400 * time.Millisecond)
				terminalAt := time.Now().UnixMicro()
				switch typ {
				case EventNetworkResponse:
					m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "r", HasExtraInfo: true, Response: cdpNetworkResponse{Status: 200}}, "s")
					m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "r"}, "s")
				case EventNetworkLoadingFailed:
					m.handleLoadingFailed(cdpNetworkLoadingFailedParams{RequestID: "r", ErrorText: "net::ERR_ABORTED", Canceled: true}, "s")
				case EventProxyError:
					m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "r", HasExtraInfo: true, Response: cdpNetworkResponse{Status: 502, Headers: json.RawMessage(`{"x-kernel-proxy-error":"proxy_connect_failed"}`)}}, "s")
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				require.Equal(t, requestAt, ec.waitFor(t, EventNetworkRequest, time.Second).Ts)
				require.Equal(t, terminalAt, ec.waitFor(t, typ, time.Second).Ts)
			})
		})
	}
}

func TestNetworkRedirectHopsPublishInOrder(t *testing.T) {
	for _, mode := range []string{"late_headers", "missing_headers", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, ec := newComputedMonitor(t)
				defer m.Stop()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				m.telemetryCtx = ctx
				m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/start"}}, "s")
				m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "r", RedirectHasExtraInfo: true, RedirectResponse: json.RawMessage(`{"status":302}`), Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/end"}}, "s")
				m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "r", Response: cdpNetworkResponse{Status: 200}}, "s")
				m.handleLoadingFinished(ctx, cdpNetworkLoadingFinishedParams{RequestID: "r"}, "s")
				time.Sleep(200 * time.Millisecond)
				if mode == "late_headers" {
					m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "r", Headers: json.RawMessage(`{}`)}, "s")
					m.handleResponseExtraInfo(cdpNetworkResponseReceivedExtraInfoParams{RequestID: "r", StatusCode: 302, Headers: json.RawMessage(`{}`)}, "s")
				} else if mode == "canceled" {
					cancel()
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				ec.mu.Lock()
				defer ec.mu.Unlock()
				var sequence []string
				for _, event := range ec.events {
					var payload struct {
						URL string `json:"url"`
					}
					require.NoError(t, json.Unmarshal(event.Data, &payload))
					sequence = append(sequence, event.Type+" "+payload.URL)
				}
				require.Equal(t, []string{
					EventNetworkRequest + " https://example.com/start",
					EventNetworkResponse + " https://example.com/start",
					EventNetworkRequest + " https://example.com/end",
					EventNetworkResponse + " https://example.com/end",
				}, sequence)
			})
		})
	}
}

func TestNetworkHeadersFollowRedirectHops(t *testing.T) {
	for _, order := range []string{"extras_first", "extras_last", "interleaved"} {
		t.Run(order, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.close()
			_, ec, cleanup := startMonitor(t, srv, nil)
			defer cleanup()
			send := func(method string, params any) { sendNetworkFixture(t, srv, method, params) }
			extra := func(index int) {
				send("Network.requestWillBeSentExtraInfo", map[string]any{"requestId": "r", "headers": map[string]string{"Cookie": fmt.Sprintf("hop=%d", index)}})
				send("Network.responseReceivedExtraInfo", map[string]any{"requestId": "r", "statusCode": []int{302, 200}[index], "headers": map[string]string{"X-Hop": fmt.Sprint(index)}})
			}
			if order == "extras_first" {
				extra(0)
				extra(1)
			}
			send("Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/start"}})
			if order == "interleaved" {
				extra(0)
			}
			send("Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", RedirectHasExtraInfo: true, RedirectResponse: json.RawMessage(`{"status":302,"headers":{"Base":"redirect"}}`), Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/cached"}})
			// A cached redirect has no ExtraInfo; it must not consume the next hop's headers.
			send("Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", RedirectResponse: json.RawMessage(`{"status":301,"headers":{"Base":"cached"}}`), Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/final"}})
			if order == "interleaved" {
				extra(1)
			}
			send("Network.responseReceived", cdpNetworkResponseReceivedParams{RequestID: "r", HasExtraInfo: true, Response: cdpNetworkResponse{Status: 200}})
			send("Network.loadingFinished", cdpNetworkLoadingFinishedParams{RequestID: "r"})
			if order == "extras_last" {
				extra(0)
				extra(1)
			}
			for _, tc := range []struct {
				path, cookie, header string
				status               int
			}{{"start", "hop=0", "0", 302}, {"cached", "", "", 301}, {"final", "hop=1", "1", 200}} {
				req := waitForNetworkURL(t, ec, EventNetworkRequest, "https://example.com/"+tc.path)
				var request oapi.BrowserNetworkRequestEventData
				require.NoError(t, json.Unmarshal(req.Data, &request))
				res := waitForNetworkURL(t, ec, EventNetworkResponse, "https://example.com/"+tc.path)
				var response oapi.BrowserNetworkResponseEventData
				require.NoError(t, json.Unmarshal(res.Data, &response))
				require.Equal(t, tc.status, response.Status)
				if tc.cookie != "" {
					require.Equal(t, tc.cookie, request.Headers["Cookie"])
					require.NotNil(t, response.RequestHeaders)
					require.Equal(t, tc.cookie, (*response.RequestHeaders)["Cookie"])
					require.Equal(t, tc.header, response.Headers["X-Hop"])
				} else {
					require.NotContains(t, request.Headers, "Cookie")
					require.Nil(t, response.RequestHeaders)
					require.Equal(t, "cached", response.Headers["Base"])
				}
			}
		})
	}
}

func TestTerminalRequestPreservesPendingRedirectHeaders(t *testing.T) {
	for _, terminal := range []string{EventNetworkLoadingFailed, EventNetworkResponse} {
		t.Run(terminal, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, ec := newComputedMonitor(t)
				defer m.Stop()
				m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/start"}}, "s")
				m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "r", Headers: json.RawMessage(`{"Cookie":"hop=0"}`)}, "s")
				m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "r", RedirectHasExtraInfo: true, RedirectResponse: json.RawMessage(`{"status":302,"headers":{"Base":"redirect"}}`), Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/final"}}, "s")
				if terminal == EventNetworkLoadingFailed {
					m.handleRequestExtraInfo(cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "r", Headers: json.RawMessage(`{"Cookie":"hop=1"}`)}, "s")
					m.handleLoadingFailed(cdpNetworkLoadingFailedParams{RequestID: "r", ErrorText: "net::ERR_ABORTED", Canceled: true}, "s")
				} else {
					m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "r", Response: cdpNetworkResponse{Status: 200}}, "s")
					m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "r"}, "s")
				}
				time.Sleep(200 * time.Millisecond)
				m.handleResponseExtraInfo(cdpNetworkResponseReceivedExtraInfoParams{RequestID: "r", StatusCode: 302, Headers: json.RawMessage(`{"Set-Cookie":"redirect=true","X-Wire":"yes"}`)}, "s")
				synctest.Wait()
				event := waitForNetworkURL(t, ec, EventNetworkResponse, "https://example.com/start")
				var response oapi.BrowserNetworkResponseEventData
				require.NoError(t, json.Unmarshal(event.Data, &response))
				require.Equal(t, "redirect=true", response.Headers["Set-Cookie"])
				require.Equal(t, "yes", response.Headers["X-Wire"])
				waitForNetworkURL(t, ec, terminal, "https://example.com/final")
				require.Empty(t, m.networkExchanges, "completed captures must release their header state")
			})
		})
	}
}

func TestCanceledRequestPublishesBeforeFailure(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()
	sendNetworkFixture(t, srv, "Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/canceled"}})
	sendNetworkFixture(t, srv, "Network.loadingFailed", cdpNetworkLoadingFailedParams{RequestID: "r", ErrorText: "net::ERR_ABORTED", Canceled: true})
	request := ec.waitFor(t, EventNetworkRequest, 2*time.Second)
	failure := ec.waitFor(t, EventNetworkLoadingFailed, 2*time.Second)
	ec.mu.Lock()
	defer ec.mu.Unlock()
	var order []string
	for _, event := range ec.events {
		if event.Category == events.Network {
			order = append(order, event.Type)
		}
	}
	require.Equal(t, []string{EventNetworkRequest, EventNetworkLoadingFailed}, order)
	require.LessOrEqual(t, request.Ts, failure.Ts)
}

func TestResponseWaitsForRequestPublication(t *testing.T) {
	ec := newEventCollector()
	publish := ec.publishFn()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	m := New(newTestUpstream(""), func(event events.Event) (events.Envelope, bool) {
		if event.Type == EventNetworkRequest {
			close(entered)
			<-release
		}
		return publish(event)
	}, 0, discardLogger, nil)
	defer m.Stop()
	defer unblock()
	m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/image"}}, "s")
	m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "r", Response: cdpNetworkResponse{Status: 200}}, "s")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request publication did not start")
	}
	m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "r"}, "s")
	ec.assertNone(t, EventNetworkResponse, 0, 50*time.Millisecond)
	unblock()
	ec.waitFor(t, EventNetworkResponse, time.Second)
	ec.mu.Lock()
	defer ec.mu.Unlock()
	require.Len(t, ec.events, 2)
	require.Equal(t, EventNetworkRequest, ec.events[0].Type)
	require.Equal(t, EventNetworkResponse, ec.events[1].Type)
}

func TestNetworkLateHeadersRemainOnResponse(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	_, ec, cleanup := startMonitor(t, srv, nil)
	defer cleanup()
	sendNetworkFixture(t, srv, "Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/slow", Headers: json.RawMessage(`{"Base":"value"}`)}})
	// A hanging request is still visible without waiting for a server response.
	request := ec.waitFor(t, EventNetworkRequest, 2*time.Second)
	var data oapi.BrowserNetworkRequestEventData
	require.NoError(t, json.Unmarshal(request.Data, &data))
	require.Equal(t, "value", data.Headers["Base"])
	sendNetworkFixture(t, srv, "Network.requestWillBeSentExtraInfo", map[string]any{"requestId": "r", "headers": map[string]string{"Cookie": "late=true"}})
	sendNetworkFixture(t, srv, "Network.responseReceived", cdpNetworkResponseReceivedParams{RequestID: "r", HasExtraInfo: true, Response: cdpNetworkResponse{Status: 200}})
	sendNetworkFixture(t, srv, "Network.loadingFinished", cdpNetworkLoadingFinishedParams{RequestID: "r"})
	sendNetworkFixture(t, srv, "Network.responseReceivedExtraInfo", map[string]any{"requestId": "r", "headers": map[string]string{"X-Wire": "yes"}, "statusCode": 200})
	response := ec.waitFor(t, EventNetworkResponse, 2*time.Second)
	var payload oapi.BrowserNetworkResponseEventData
	require.NoError(t, json.Unmarshal(response.Data, &payload))
	require.NotNil(t, payload.RequestHeaders)
	require.Equal(t, "late=true", (*payload.RequestHeaders)["Cookie"])
	require.Equal(t, "yes", payload.Headers["X-Wire"])
	ec.mu.Lock()
	defer ec.mu.Unlock()
	count := 0
	for _, event := range ec.events {
		if event.Type == EventNetworkRequest {
			count++
		}
	}
	require.Equal(t, 1, count, "late headers must not duplicate the request event")
}

func TestNetworkMalformedWireHeadersPreserveBaseHeaders(t *testing.T) {
	for _, wire := range []string{`[]`, `"invalid"`, `null`} {
		t.Run(wire, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.close()
			_, ec, cleanup := startMonitor(t, srv, nil)
			defer cleanup()
			sendNetworkFixture(t, srv, "Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/", Headers: json.RawMessage(`{"X-Request":"base"}`)}})
			sendNetworkFixture(t, srv, "Network.requestWillBeSentExtraInfo", cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "r", Headers: json.RawMessage(wire)})
			sendNetworkFixture(t, srv, "Network.responseReceived", cdpNetworkResponseReceivedParams{RequestID: "r", HasExtraInfo: true, Response: cdpNetworkResponse{Status: 200, Headers: json.RawMessage(`{"X-Response":"base"}`)}})
			sendNetworkFixture(t, srv, "Network.responseReceivedExtraInfo", cdpNetworkResponseReceivedExtraInfoParams{RequestID: "r", StatusCode: 200, Headers: json.RawMessage(wire)})
			sendNetworkFixture(t, srv, "Network.loadingFinished", cdpNetworkLoadingFinishedParams{RequestID: "r"})
			var request oapi.BrowserNetworkRequestEventData
			require.NoError(t, json.Unmarshal(ec.waitFor(t, EventNetworkRequest, time.Second).Data, &request))
			require.Equal(t, "base", request.Headers["X-Request"])
			var response oapi.BrowserNetworkResponseEventData
			require.NoError(t, json.Unmarshal(ec.waitFor(t, EventNetworkResponse, time.Second).Data, &response))
			require.Equal(t, "base", response.Headers["X-Response"])
			require.Nil(t, response.RequestHeaders, "invalid wire headers must not be exposed as captured headers")
		})
	}
}

func TestNetworkStatusRemainsCompatible(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wireStatus *int
	}{
		{"revalidated", ptrOf(304)},
		{"unchanged", ptrOf(200)},
		{"no_extra_info", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.close()
			_, ec, cleanup := startMonitor(t, srv, nil)
			defer cleanup()
			sendNetworkFixture(t, srv, "Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/"}})
			sendNetworkFixture(t, srv, "Network.responseReceived", cdpNetworkResponseReceivedParams{RequestID: "r", HasExtraInfo: tc.wireStatus != nil, Response: cdpNetworkResponse{Status: 200, StatusText: "Custom OK", Headers: json.RawMessage(`{"ETag":"v1"}`)}})
			if tc.wireStatus != nil {
				sendNetworkFixture(t, srv, "Network.requestWillBeSentExtraInfo", cdpNetworkRequestWillBeSentExtraInfoParams{RequestID: "r", Headers: json.RawMessage(`{}`)})
				sendNetworkFixture(t, srv, "Network.responseReceivedExtraInfo", cdpNetworkResponseReceivedExtraInfoParams{RequestID: "r", StatusCode: *tc.wireStatus, Headers: json.RawMessage(`{"ETag":"v1"}`)})
			}
			sendNetworkFixture(t, srv, "Network.loadingFinished", cdpNetworkLoadingFinishedParams{RequestID: "r"})
			var response map[string]any
			require.NoError(t, json.Unmarshal(ec.waitFor(t, EventNetworkResponse, time.Second).Data, &response))
			require.Equal(t, float64(200), response["status"])
			require.Equal(t, "Custom OK", response["status_text"])
			require.NotContains(t, response, "effective_status")
			if tc.wireStatus != nil {
				require.Equal(t, float64(*tc.wireStatus), response["wire_status"])
			} else {
				require.NotContains(t, response, "wire_status")
			}
		})
	}
}

func TestNetworkBodyErrorsAreObservable(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	_, ec, cleanup := startMonitor(t, srv, func(msg cdpMessage) any {
		if msg.Method == "Network.getResponseBody" {
			return map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "No resource with given identifier found"}}
		}
		return nil
	})
	defer cleanup()
	sendNetworkFixture(t, srv, "Network.requestWillBeSent", cdpNetworkRequestWillBeSentParams{RequestID: "r", Type: "Fetch", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/api"}})
	sendNetworkFixture(t, srv, "Network.responseReceived", cdpNetworkResponseReceivedParams{RequestID: "r", Response: cdpNetworkResponse{Status: 200, MimeType: "application/json"}})
	sendNetworkFixture(t, srv, "Network.loadingFinished", cdpNetworkLoadingFinishedParams{RequestID: "r"})
	event := ec.waitFor(t, EventNetworkResponse, time.Second)
	var response oapi.BrowserNetworkResponseEventData
	require.NoError(t, json.Unmarshal(event.Data, &response))
	require.NotNil(t, response.BodyError)
	require.Contains(t, *response.BodyError, "No resource with given identifier found")
	require.Nil(t, response.Body)
	require.Equal(t, 200, response.Status)
}

func TestNavigationPreservesPendingResponsesAndIdleAccounting(t *testing.T) {
	m, ec := newComputedMonitor(t)
	defer m.Stop()
	navigateMonitor(m, "https://example.com/old")
	request := func(id, loader string) {
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: id, LoaderID: loader, Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/" + id}}, "s1")
	}
	request("old", "")
	request("document", "new-loader")
	m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: "document", Response: cdpNetworkResponse{Status: 200}}, "s1")
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{Frame: cdpPageFrame{ID: "f1", LoaderID: "new-loader", URL: "https://example.com/new"}}, "s1")
	m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "old"}, "s1")
	// Finishing the previous loader must not make the new document idle.
	checkpoint := ec.checkpoint()
	ec.assertNone(t, EventNetworkIdle, checkpoint, 600*time.Millisecond)
	m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "document"}, "s1")
	event := waitForNetworkURL(t, ec, EventNetworkResponse, "https://example.com/document")
	var response oapi.BrowserNetworkResponseEventData
	require.NoError(t, json.Unmarshal(event.Data, &response))
	require.Equal(t, 200, response.Status)
	ec.waitForNew(t, EventNetworkIdle, checkpoint, time.Second)
}

func TestExtensionSourceIdentifiesBundledAndCustomTargets(t *testing.T) {
	for _, tc := range []struct{ url, id, managed string }{
		{"https://example.com/worker.js", "", ""},
		{"chrome-extension://aiefahdofckmjaelnjpjkffghapmnlof/background.js", "aiefahdofckmjaelnjpjkffghapmnlof", "true"},
		{"chrome-extension://" + unpackedExtensionID("/home/kernel/extensions/kernel") + "/background.js", unpackedExtensionID("/home/kernel/extensions/kernel"), "true"},
		{"chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/background.js", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "false"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			metadata := targetMetadata(targetInfo{url: tc.url, targetType: "service_worker", targetID: "target"})
			require.Equal(t, tc.id, metadata["extension_id"])
			require.Equal(t, tc.managed, metadata["kernel_extension"])
		})
	}
}

func TestSameProcessFrameRequestsParticipateInNetworkIdle(t *testing.T) {
	m, ec := newComputedMonitor(t)
	defer m.Stop()
	navigateMonitor(m, "https://example.com/")
	m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{RequestID: "child", LoaderID: "iframe-loader", FrameID: "child", Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/child"}}, "s1")
	ec.assertNone(t, EventNetworkIdle, 0, 600*time.Millisecond)
	m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: "child"}, "s1")
	ec.waitFor(t, EventNetworkIdle, time.Second)
}

func TestNetworkSourceSurvivesOriginChanges(t *testing.T) {
	m, ec := newComputedMonitor(t)
	defer m.Stop()
	navigateMonitor(m, "about:blank")
	extension := "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/page.html"
	for index, document := range []string{extension, "https://example.com/"} {
		id := fmt.Sprint(index)
		m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{
			RequestID: id, DocumentURL: document,
			Request: cdpNetworkRequest{Method: "GET", URL: document},
		}, "s1")
		// Publication is delayed until after the next document replaces the target URL.
		next := "https://example.com/"
		if index == 1 {
			next = extension
		}
		m.handleFrameNavigated(cdpPageFrameNavigatedParams{Frame: cdpPageFrame{ID: "f1", URL: next}}, "s1")
		m.handleResponseReceived(cdpNetworkResponseReceivedParams{RequestID: id, Response: cdpNetworkResponse{Status: 200}}, "s1")
		m.handleLoadingFinished(context.Background(), cdpNetworkLoadingFinishedParams{RequestID: id}, "s1")
		for _, typ := range []string{EventNetworkRequest, EventNetworkResponse} {
			event := waitForNetworkURL(t, ec, typ, document)
			if index == 0 {
				require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", (*event.Source.Metadata)["extension_id"])
			} else {
				require.NotContains(t, *event.Source.Metadata, "extension_id")
				require.NotContains(t, *event.Source.Metadata, "kernel_extension")
			}
		}
	}
}

func TestExtensionSourceTracksTargetAndRootFrameNavigation(t *testing.T) {
	m, ec := newComputedMonitor(t)
	defer m.Stop()
	navigateMonitor(m, "about:blank")
	extension := "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/page.html"
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{Frame: cdpPageFrame{ID: "f1", URL: extension}}, "s1")
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{Frame: cdpPageFrame{ID: "child", ParentID: "f1", URL: "https://example.com/"}}, "s1")
	m.handleTargetCrashed("s1")
	event := ec.waitFor(t, EventPageCrashed, time.Second)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", (*event.Source.Metadata)["extension_id"], "a child frame must not change the target origin")

	params, err := json.Marshal(map[string]any{"targetInfo": cdpTargetTargetInfo{TargetID: m.sessions["s1"].targetID, URL: "https://example.com/"}})
	require.NoError(t, err)
	m.handleSurfaceEvent(nil, browsersurface.Event{Kind: browsersurface.EventProtocol, Message: cdpclient.Message{Method: "Target.targetInfoChanged", Params: params}})
	checkpoint := ec.checkpoint()
	m.handleTargetCrashed("s1")
	event = ec.waitForNew(t, EventPageCrashed, checkpoint, time.Second)
	require.NotContains(t, *event.Source.Metadata, "extension_id")
}

func TestExtensionSourceTracksOOPIFRootNavigation(t *testing.T) {
	m, ec := newComputedMonitor(t)
	defer m.Stop()
	m.sessions["s1"] = targetInfo{targetID: "child", targetType: "iframe", parentFrameID: "parent"}
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{Frame: cdpPageFrame{ID: "child", ParentID: "parent", URL: "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/page.html"}}, "s1")
	m.handleFrameNavigated(cdpPageFrameNavigatedParams{Frame: cdpPageFrame{ID: "nested", ParentID: "child", URL: "https://example.com/"}}, "s1")
	m.handleTargetCrashed("s1")
	event := ec.waitFor(t, EventPageCrashed, time.Second)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", (*event.Source.Metadata)["extension_id"])
}

func TestProxyErrorSourceSurvivesOriginChanges(t *testing.T) {
	extension := "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/page.html"
	for _, document := range []string{extension, "https://example.com/"} {
		t.Run(document, func(t *testing.T) {
			m, ec := newComputedMonitor(t)
			defer m.Stop()
			navigateMonitor(m, document)
			m.handleNetworkRequest(cdpNetworkRequestWillBeSentParams{
				RequestID: "r", DocumentURL: document,
				Request: cdpNetworkRequest{Method: "GET", URL: "https://example.com/api"},
			}, "s1")
			next := extension
			if document == extension {
				next = "https://example.com/"
			}
			navigateMonitor(m, next)
			m.handleResponseReceived(cdpNetworkResponseReceivedParams{
				RequestID: "r", Response: cdpNetworkResponse{Status: 403, Headers: json.RawMessage(`{"x-kernel-proxy-error":"network_policy_denied"}`)},
			}, "s1")
			event := ec.waitFor(t, EventProxyError, time.Second)
			if document == extension {
				require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", (*event.Source.Metadata)["extension_id"])
			} else {
				require.NotContains(t, *event.Source.Metadata, "extension_id")
				require.NotContains(t, *event.Source.Metadata, "kernel_extension")
			}
		})
	}
}
