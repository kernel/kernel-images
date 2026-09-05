package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestNetworkCaptureFromWorkers(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium worker tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const replyScript = `async function reply(marker, port) {
		try {
			const response = await fetch('/post?marker=' + marker, {
				method: 'POST', headers: {'Content-Type': 'application/json'},
				body: JSON.stringify({marker})
			});
			port.postMessage({status: response.status, body: await response.text()});
		} catch (error) { port.postMessage({error: error.message}); }
	}`
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/post" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.Copy(w, r.Body)
			return
		}
		w.Header().Set("Content-Type", "text/javascript")
		switch r.URL.Path {
		case "/worker.js":
			fmt.Fprint(w, replyScript+`; self.onmessage = event => reply(event.data, self);`)
		case "/shared.js":
			fmt.Fprint(w, replyScript+`; self.onconnect = event => {
				const port = event.ports[0]; port.onmessage = event => reply(event.data, port); port.start();
			};`)
		case "/service.js":
			fmt.Fprint(w, replyScript+`;
				self.addEventListener('install', event => event.waitUntil(self.skipWaiting()));
				self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));
				self.addEventListener('message', event => event.waitUntil(reply(event.data, event.ports[0])));`)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>workers</body></html>")
		}
	}))
	defer stub.Close()
	browserWS := launchChromium(t, ctx, findChromium(t))
	cdp := dialCDP(t, ctx, browserWS)
	defer cdp.close()
	ec := newEventCollector()
	m := New(&staticUpstream{url: browserWS}, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	targetID := cdp.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL}).targetID(t)
	sessionID := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}).sessionID(t)
	require.Eventually(t, func() bool {
		return cdp.evalBool(ctx, sessionID, `document.readyState === 'complete' && window.__kernelEventInjected === true`)
	}, 5*time.Second, 50*time.Millisecond)

	for _, worker := range []struct{ targetType, path, create, request string }{
		{"worker", "/worker.js", `window.testWorker = new Worker('/worker.js')`, `testWorker.onmessage = event => resolve(event.data); testWorker.postMessage(marker);`},
		{"shared_worker", "/shared.js", `window.sharedWorker = new SharedWorker('/shared.js'); sharedWorker.port.start()`, `sharedWorker.port.onmessage = event => resolve(event.data); sharedWorker.port.postMessage(marker);`},
		{"service_worker", "/service.js", `await navigator.serviceWorker.register('/service.js'); window.serviceWorker = (await navigator.serviceWorker.ready).active`, `const channel = new MessageChannel(); channel.port1.onmessage = event => resolve(event.data); serviceWorker.postMessage(marker, [channel.port2]);`},
	} {
		t.Run(worker.targetType, func(t *testing.T) {
			evaluateNetworkScript(t, ctx, cdp, sessionID, "(async () => {"+worker.create+"; return true;})()")
			require.Eventually(t, func() bool {
				raw, err := cdp.roundtrip(ctx, "", "Target.getTargets", nil)
				if err != nil {
					return false
				}
				var result struct {
					Result struct {
						Targets []cdpTargetTargetInfo `json:"targetInfos"`
					} `json:"result"`
				}
				if json.Unmarshal(raw, &result) != nil {
					return false
				}
				for _, target := range result.Result.Targets {
					if target.Type == worker.targetType && target.URL == stub.URL+worker.path {
						return true
					}
				}
				return false
			}, 5*time.Second, 50*time.Millisecond, "worker target was not created")
			// Requests during worker startup are outside this settled-target test.
			time.Sleep(500 * time.Millisecond)
			raw := evaluateNetworkScript(t, ctx, cdp, sessionID, fmt.Sprintf(`new Promise(resolve => { const marker = %q; %s })`, worker.targetType, worker.request))
			var result struct {
				Status int    `json:"status"`
				Body   string `json:"body"`
				Error  string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &result))
			require.Empty(t, result.Error)
			require.Equal(t, http.StatusOK, result.Status)
			body := fmt.Sprintf(`{"marker":%q}`, worker.targetType)
			require.JSONEq(t, body, result.Body)
			url := stub.URL + "/post?marker=" + worker.targetType
			requestEvent := waitForNetworkURL(t, ec, EventNetworkRequest, url)
			var request oapi.BrowserNetworkRequestEventData
			require.NoError(t, json.Unmarshal(requestEvent.Data, &request))
			require.Equal(t, worker.targetType, string(request.TargetType))
			require.True(t, request.TargetType.Valid())
			require.NotNil(t, request.PostData)
			require.JSONEq(t, body, *request.PostData)
			responseEvent := waitForNetworkURL(t, ec, EventNetworkResponse, url)
			var response oapi.BrowserNetworkResponseEventData
			require.NoError(t, json.Unmarshal(responseEvent.Data, &response))
			require.Equal(t, request.SessionId, response.SessionId)
			require.Equal(t, request.RequestId, response.RequestId)
			require.NotNil(t, response.Body)
			require.JSONEq(t, body, *response.Body)
		})
	}
}

func evaluateNetworkScript(t *testing.T, ctx context.Context, cdp *cdpConn, sessionID, expression string) json.RawMessage {
	t.Helper()
	raw := cdp.call(t, ctx, sessionID, "Runtime.evaluate", map[string]any{
		"expression": expression, "awaitPromise": true, "returnByValue": true,
	})
	var result struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			ExceptionDetails json.RawMessage `json:"exceptionDetails"`
			Result           struct {
				Value json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw.raw, &result))
	require.Empty(t, result.Error)
	require.Empty(t, result.Result.ExceptionDetails)
	return result.Result.Result.Value
}
