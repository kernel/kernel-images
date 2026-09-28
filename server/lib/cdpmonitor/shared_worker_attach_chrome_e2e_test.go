package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A client that connects after a monitored SharedWorker has ended must not
// crash the browser. Chromium keeps the worker's DevTools host for existing
// sessions, and browser-level auto-attach on that host dereferences the
// destroyed worker.
func TestNewClientAutoAttachAfterMonitoredSharedWorkerEnds(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium worker tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shared.js" {
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, `self.onconnect = event => {
				const port = event.ports[0]; port.onmessage = event => event.data === 'close' && self.close(); port.start();
			};`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>shared worker</body></html>")
	}))
	defer stub.Close()
	browserWS := launchChromium(t, ctx, findChromium(t))
	cdp := dialCDP(t, ctx, browserWS)
	defer cdp.close()
	m := New(&staticUpstream{url: browserWS}, newEventCollector().publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	targetID := cdp.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL}).targetID(t)
	sessionID := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}).sessionID(t)
	require.Eventually(t, func() bool {
		return cdp.evalBool(ctx, sessionID, fmt.Sprintf(`location.href === %q && document.readyState === 'complete' && window.__kernelEventInjected === true`, stub.URL+"/"))
	}, 5*time.Second, 50*time.Millisecond)

	sharedWorker := func() (found, attached bool) {
		raw, err := cdp.roundtrip(ctx, "", "Target.getTargets", nil)
		if err != nil {
			return false, false
		}
		var result struct {
			Result struct {
				Targets []struct {
					Type     string `json:"type"`
					Attached bool   `json:"attached"`
				} `json:"targetInfos"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &result) != nil {
			return false, false
		}
		for _, target := range result.Result.Targets {
			if target.Type == "shared_worker" {
				return true, target.Attached
			}
		}
		return false, false
	}
	evaluateNetworkScript(t, ctx, cdp, sessionID, `(() => { window.sharedWorker = new SharedWorker('/shared.js'); sharedWorker.port.start(); return true; })()`)
	// The test client never attaches to the worker, so only the monitor can.
	require.Eventually(t, func() bool {
		_, attached := sharedWorker()
		return attached
	}, 5*time.Second, 50*time.Millisecond, "monitor did not attach to the shared worker")

	evaluateNetworkScript(t, ctx, cdp, sessionID, `(() => { sharedWorker.port.postMessage('close'); return true; })()`)
	require.Eventually(t, func() bool {
		found, _ := sharedWorker()
		return !found
	}, 5*time.Second, 50*time.Millisecond, "shared worker did not end")

	// Playwright's connectOverCDP sends this first.
	client := dialCDP(t, ctx, browserWS)
	defer client.close()
	attachCtx, attachCancel := context.WithTimeout(ctx, 5*time.Second)
	defer attachCancel()
	_, err := client.roundtrip(attachCtx, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true,
	})
	require.NoError(t, err, "browser exited or did not answer Target.setAutoAttach")
	versionCtx, versionCancel := context.WithTimeout(ctx, 5*time.Second)
	defer versionCancel()
	_, err = cdp.roundtrip(versionCtx, "", "Browser.getVersion", nil)
	require.NoError(t, err, "browser stopped responding after a new client auto-attached")
}
