package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chromium sends Inspector.targetCrashed on a shared worker's session when the
// worker ends and on a service worker's session when the browser stops it.
// Neither is a crash, so both must surface as page_worker_ended. A stopped
// service worker restarts on the same session, so its telemetry must keep
// flowing.
func TestPageWorkerEndedChrome(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium worker tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/shared.js":
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, `self.onconnect = event => {
				const port = event.ports[0]; port.onmessage = () => self.close(); port.start();
			};`)
		case "/service.js":
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, `self.addEventListener('install', event => event.waitUntil(self.skipWaiting()));
				self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));
				self.addEventListener('message', event => event.waitUntil(
					fetch('/ping?marker=' + event.data).then(() => event.ports[0].postMessage('ok'))));`)
		case "/ping":
			fmt.Fprint(w, "pong")
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
		return cdp.evalBool(ctx, sessionID, fmt.Sprintf(`location.href === %q && document.readyState === 'complete' && window.__kernelEventInjected === true`, stub.URL+"/"))
	}, 15*time.Second, 50*time.Millisecond)

	t.Run("shared worker closes itself", func(t *testing.T) {
		cp := ec.checkpoint()
		evaluateNetworkScript(t, ctx, cdp, sessionID, `(async () => {
			window.sharedWorker = new SharedWorker('/shared.js'); sharedWorker.port.start(); return true;
		})()`)
		waitForWorkerTarget(t, ctx, cdp, "shared_worker", stub.URL+"/shared.js")
		evaluateNetworkScript(t, ctx, cdp, sessionID, `(async () => { sharedWorker.port.postMessage('close'); return true; })()`)
		waitForPageWorkerEnded(t, ec, cp, "shared_worker", stub.URL+"/shared.js")
	})

	t.Run("service worker stops and restarts", func(t *testing.T) {
		evaluateNetworkScript(t, ctx, cdp, sessionID, `(async () => {
			await navigator.serviceWorker.register('/service.js'); await navigator.serviceWorker.ready; return true;
		})()`)
		cdp.call(t, ctx, sessionID, "ServiceWorker.enable", nil)
		wake := func(marker string) {
			evaluateNetworkScript(t, ctx, cdp, sessionID, fmt.Sprintf(`(async () => {
				const registration = await navigator.serviceWorker.ready;
				const channel = new MessageChannel();
				const replied = new Promise(resolve => channel.port1.onmessage = resolve);
				registration.active.postMessage(%q, [channel.port2]);
				await replied; return true;
			})()`, marker))
		}
		wake("first")
		first := waitForNetworkURL(t, ec, EventNetworkRequest, stub.URL+"/ping?marker=first")

		cp := ec.checkpoint()
		cdp.call(t, ctx, sessionID, "ServiceWorker.stopAllWorkers", nil)
		ended := waitForPageWorkerEnded(t, ec, cp, "service_worker", stub.URL+"/service.js")
		require.NotNil(t, ended.Source.Metadata)
		workerSession := (*ended.Source.Metadata)["cdp_session_id"]
		require.NotEmpty(t, workerSession)
		assert.Equal(t, sessionIDOf(t, first), workerSession)

		wake("second")
		second := waitForNetworkURL(t, ec, EventNetworkRequest, stub.URL+"/ping?marker=second")
		assert.Equal(t, workerSession, sessionIDOf(t, second), "restarted service worker must keep reporting on its session")

		cp = ec.checkpoint()
		cdp.call(t, ctx, sessionID, "ServiceWorker.stopAllWorkers", nil)
		again := waitForPageWorkerEnded(t, ec, cp, "service_worker", stub.URL+"/service.js")
		assert.Equal(t, workerSession, (*again.Source.Metadata)["cdp_session_id"])
	})

	t.Run("page renderer crash", func(t *testing.T) {
		cp := ec.checkpoint()
		crashID := cdp.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL + "/crash"}).targetID(t)
		crashSession := cdp.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": crashID, "flatten": true}).sessionID(t)
		require.Eventually(t, func() bool {
			return cdp.evalBool(ctx, crashSession, `document.readyState === 'complete' && window.__kernelEventInjected === true`)
		}, 15*time.Second, 50*time.Millisecond)
		// SIGKILL every renderer rather than sending Page.crash: a killed renderer
		// is reported without waiting on Chrome's crash handler.
		raw := cdp.call(t, ctx, "", "SystemInfo.getProcessInfo", nil).raw
		var info struct {
			Result struct {
				ProcessInfo []struct {
					Type string `json:"type"`
					ID   int    `json:"id"`
				} `json:"processInfo"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &info))
		killed := 0
		for _, process := range info.Result.ProcessInfo {
			if process.Type == "renderer" {
				require.NoError(t, syscall.Kill(process.ID, syscall.SIGKILL))
				killed++
			}
		}
		require.NotZero(t, killed, "no renderer processes reported")
		require.Eventually(t, func() bool {
			ec.mu.Lock()
			defer ec.mu.Unlock()
			for _, ev := range ec.events[cp:] {
				if ev.Type != EventPageCrashed {
					continue
				}
				var data struct {
					TargetID   string `json:"target_id"`
					TargetType string `json:"target_type"`
				}
				if json.Unmarshal(ev.Data, &data) == nil && data.TargetID == crashID {
					return data.TargetType == "page"
				}
			}
			return false
		}, 10*time.Second, 25*time.Millisecond, "missing page_crashed for the killed page")
	})

	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, ev := range ec.events {
		if ev.Type != EventPageCrashed {
			continue
		}
		var data struct {
			TargetType string `json:"target_type"`
		}
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "page", data.TargetType, "page_crashed reported for a non-page target")
	}
}

func waitForWorkerTarget(t *testing.T, ctx context.Context, cdp *cdpConn, targetType, url string) {
	t.Helper()
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
			if target.Type == targetType && target.URL == url {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "%s target was not created", targetType)
}

func waitForPageWorkerEnded(t *testing.T, ec *eventCollector, since int, targetType, url string) events.Event {
	t.Helper()
	var found events.Event
	require.Eventually(t, func() bool {
		ec.mu.Lock()
		defer ec.mu.Unlock()
		for _, ev := range ec.events[since:] {
			if ev.Type != EventPageWorkerEnded {
				continue
			}
			var data struct {
				TargetType string `json:"target_type"`
				URL        string `json:"url"`
			}
			if json.Unmarshal(ev.Data, &data) == nil && data.TargetType == targetType && data.URL == url {
				found = ev
				return true
			}
		}
		return false
	}, 10*time.Second, 25*time.Millisecond, "missing page_worker_ended for %s %s", targetType, url)
	assert.Equal(t, events.Page, found.Category)
	return found
}

func sessionIDOf(t *testing.T, ev events.Event) string {
	t.Helper()
	var data struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	return data.SessionID
}
