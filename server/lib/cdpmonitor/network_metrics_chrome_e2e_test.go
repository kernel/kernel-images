package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/metrics"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/telemetry"
	"github.com/stretchr/testify/require"
)

func TestAlwaysOnNetworkMetricsChrome(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	const workerFetch = `async function reply(path, port) { try { const r = await fetch(path, {method:'POST', body:'fixture'}); await r.text(); port.postMessage(r.status); } catch (e) { port.postMessage(e.name); } }`
	var crossOrigin string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/reset":
			_, _ = io.Copy(io.Discard, r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
		case "/ok":
			fmt.Fprint(w, "ok")
		case "/500":
			w.WriteHeader(500)
			fmt.Fprint(w, "fixture error")
		case "/cancel":
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-ctx.Done():
			}
		case "/worker.js":
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, workerFetch+`; self.onmessage = e => reply(e.data, self);`)
		case "/shared.js":
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, workerFetch+`; self.onconnect = e => { const p=e.ports[0]; p.onmessage=e=>reply(e.data,p); p.start(); };`)
		case "/service.js":
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, workerFetch+`; self.addEventListener('install',e=>e.waitUntil(self.skipWaiting())); self.addEventListener('activate',e=>e.waitUntil(self.clients.claim())); self.addEventListener('message',e=>e.waitUntil(reply(e.data,e.ports[0])));`)
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<link rel="icon" href="data:,"><button id="go">go</button><iframe id="same" src="/frame"></iframe><iframe src="%s/frame"></iframe>`, crossOrigin)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<link rel="icon" href="data:,"><button id="go">go</button>`)
		}
	}))
	defer stub.Close()
	crossOrigin = strings.Replace(stub.URL, "127.0.0.1", "localhost", 1)
	browserWS := launchChromium(t, ctx, findChromium(t), "--site-per-process")
	driver := dialCDP(t, ctx, browserWS)
	defer driver.close()
	target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL}).targetID(t)
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, `document.readyState === 'complete' && document.querySelector('#same') !== null`)
	}, 10*time.Second, 20*time.Millisecond)
	crossTarget := findNetworkFrameTarget(t, ctx, driver, crossOrigin+"/frame")
	crossSession := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": crossTarget, "flatten": true}).sessionID(t)
	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: 128})
	require.NoError(t, err)
	ts := telemetry.NewTelemetrySession(es)
	m := New(newTestUpstream(browserWS), ts.Publish, 0, discardLogger, func() bool { return false })
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, 10*time.Second, 20*time.Millisecond)
	require.False(t, driver.evalBool(ctx, session, `window.__kernelEventInjected === true`))

	// An independent user connection confirms the browser's actual error text.
	observer, err := cdpclient.DialWithEvents(ctx, browserWS)
	require.NoError(t, err)
	defer observer.Close()
	attached, err := observer.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, "")
	require.NoError(t, err)
	var observedSession struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(attached, &observedSession))
	failures := make(chan string, 64)
	go func() {
		for message := range observer.Events() {
			if message.Method == "Network.loadingFailed" {
				var p cdpNetworkLoadingFailedParams
				if json.Unmarshal(message.Params, &p) == nil {
					select {
					case failures <- p.ErrorText:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	_, err = observer.Send(ctx, "Network.enable", nil, observedSession.SessionID)
	require.NoError(t, err)
	scrape := func() NetworkSnapshot {
		t.Helper()
		snapshot := m.NetworkSnapshot()
		h := metrics.Handler(discardLogger, metrics.NewNetworkCollector(func() (uint64, uint64, bool) {
			s := m.NetworkSnapshot()
			return s.Resets, s.Completed, s.Up
		}))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
		require.Contains(t, rr.Body.String(), fmt.Sprintf("kernel_chromium_connection_resets_total %d\n", snapshot.Resets))
		require.Contains(t, rr.Body.String(), fmt.Sprintf("kernel_chromium_network_requests_completed_total %d\n", snapshot.Completed))
		return snapshot
	}
	settle := func() NetworkSnapshot {
		t.Helper()
		time.Sleep(300 * time.Millisecond)
		return scrape()
	}
	fetch := func(sessionID, realm, path string, aborted bool) json.RawMessage {
		t.Helper()
		return evaluateNetworkScript(t, ctx, driver, sessionID, fmt.Sprintf(`(async()=>{const c=new AbortController(); const timer=%t?setTimeout(()=>c.abort(),100):null; try {const r=await %s.fetch(%q,{method:'POST',body:'fixture',signal:c.signal}); await r.text(); return r.status;} catch(e){return e.name;} finally {clearTimeout(timer);} })()`, aborted, realm, path))
	}
	before := settle()
	for range 10 {
		require.JSONEq(t, `"TypeError"`, string(fetch(session, "window", "/reset", false)))
	}
	for range 10 {
		select {
		case errorText := <-failures:
			require.Equal(t, "net::ERR_CONNECTION_RESET", errorText)
		case <-ctx.Done():
			t.Fatal("missing confirmed reset")
		}
	}
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed == before.Completed+10 }, 5*time.Second, 10*time.Millisecond)
	after := scrape()
	require.Equal(t, before.Resets+10, after.Resets)
	require.Equal(t, before.Completed+10, after.Completed)
	require.Equal(t, after, settle(), "scraping must not increment counters")
	require.Zero(t, es.Seq(), "telemetry off must not export customer data")

	// HTTP status errors are completed requests, not transport resets.
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refusedURL := "http://" + refused.Addr().String()
	require.NoError(t, refused.Close())
	for _, test := range []struct {
		path, want string
		abort      bool
	}{{"/ok", "200", false}, {"/500", "500", false}, {"/cancel", `"AbortError"`, true}, {refusedURL, `"TypeError"`, false}} {
		require.JSONEq(t, test.want, string(fetch(session, "window", test.path, test.abort)))
	}
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed == after.Completed+4 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, after.Resets, scrape().Resets)

	for _, frame := range []struct{ name, session, realm string }{{"same-origin", session, `document.querySelector('#same').contentWindow`}, {"oopif", crossSession, "window"}} {
		t.Run(frame.name, func(t *testing.T) {
			b := settle()
			require.JSONEq(t, `"TypeError"`, string(fetch(frame.session, frame.realm, "/reset", false)))
			require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed == b.Completed+1 }, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, b.Resets+1, scrape().Resets)
		})
	}
	for _, worker := range []struct{ kind, create, request string }{
		{"worker", `window.w=new Worker('/worker.js')`, `w.onmessage=e=>resolve(e.data); w.postMessage('/reset');`},
		{"shared_worker", `window.sw=new SharedWorker('/shared.js'); sw.port.start()`, `sw.port.onmessage=e=>resolve(e.data); sw.port.postMessage('/reset');`},
		{"service_worker", `await navigator.serviceWorker.register('/service.js'); window.service=(await navigator.serviceWorker.ready).active`, `const c=new MessageChannel(); c.port1.onmessage=e=>resolve(e.data); service.postMessage('/reset',[c.port2]);`},
	} {
		t.Run(worker.kind, func(t *testing.T) {
			evaluateNetworkScript(t, ctx, driver, session, `(async()=>{`+worker.create+`;return true;})()`)
			require.Eventually(t, func() bool {
				m.sessionsMu.RLock()
				defer m.sessionsMu.RUnlock()
				for id, info := range m.sessions {
					if info.targetType == worker.kind && m.networkReady[id] {
						return true
					}
				}
				return false
			}, 5*time.Second, 10*time.Millisecond)
			b := settle()
			require.JSONEq(t, `"TypeError"`, string(evaluateNetworkScript(t, ctx, driver, session, `new Promise(resolve=>{`+worker.request+`})`)))
			require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed == b.Completed+1 }, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, b.Resets+1, scrape().Resets)
		})
	}

	// Optional capture toggles on/off without replacing the monitor connection.
	m.lifeMu.Lock()
	original := m.conn
	m.lifeMu.Unlock()
	ts.Start("fixture", telemetry.TelemetryConfig{Categories: []oapi.TelemetryEventCategory{events.Interaction}})
	require.NoError(t, m.SetTelemetry(true))
	require.Eventually(t, func() bool { return driver.evalBool(ctx, session, `window.__kernelEventInjected === true`) }, 5*time.Second, 10*time.Millisecond)
	evaluateNetworkScript(t, ctx, driver, session, `document.querySelector('#same').src = '/frame?telemetry=on'; true`)
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, `document.querySelector('#same').contentWindow.__kernelEventInjected === true`)
	}, 5*time.Second, 10*time.Millisecond)
	require.Zero(t, es.Seq(), "page/network categories must remain gated while interaction capture is enabled")
	evaluateNetworkScript(t, ctx, driver, session, `document.querySelector('#go').click(); true`)
	require.Eventually(t, func() bool { return es.Seq() > 0 }, time.Second, 10*time.Millisecond)
	ts.Stop()
	require.NoError(t, m.SetTelemetry(false))
	waitForTelemetryReconcile(t, m, false)
	seq := es.Seq()
	require.False(t, driver.evalBool(ctx, session, `window.__kernelEventInjected === true`))
	require.False(t, driver.evalBool(ctx, session, `document.querySelector('#same').contentWindow.__kernelEventInjected === true`))
	require.False(t, driver.evalBool(ctx, crossSession, `window.__kernelEventInjected === true`))
	m.lifeMu.Lock()
	current := m.conn
	m.lifeMu.Unlock()
	require.Same(t, original, current)
	b := settle()
	fetch(session, "window", "/reset", false)
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Resets == b.Resets+1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, seq, es.Seq())
	m.sessionsMu.RLock()
	computedCount := len(m.computedStates)
	m.sessionsMu.RUnlock()
	m.pendReqMu.Lock()
	pendingCount := len(m.pendingRequests)
	m.pendReqMu.Unlock()
	require.Zero(t, computedCount)
	require.Zero(t, pendingCount)

	// Drop only the monitor's socket; the independent user connection still works.
	m.lifeMu.Lock()
	old := m.conn
	m.lifeMu.Unlock()
	require.NoError(t, old.protocol.Close())
	require.False(t, m.NetworkSnapshot().Up)
	require.Eventually(t, func() bool {
		m.lifeMu.Lock()
		fresh := m.conn != nil && m.conn != old
		m.lifeMu.Unlock()
		return fresh && m.NetworkSnapshot().Up
	}, 10*time.Second, 10*time.Millisecond)
	_, err = observer.GetBrowserVersion(ctx)
	require.NoError(t, err)
	b = settle()
	fetch(session, "window", "/reset", false)
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Resets == b.Resets+1 }, time.Second, 10*time.Millisecond)
	// A fresh API-owned monitor starts with zero counters, even on the same Chrome.
	m.Stop()
	fresh := New(newTestUpstream(browserWS), ts.Publish, 0, discardLogger, nil)
	require.Equal(t, NetworkSnapshot{}, fresh.NetworkSnapshot())
	require.NoError(t, fresh.SetTelemetry(false))
	require.NoError(t, fresh.Start(ctx))
	defer fresh.Stop()
	require.Eventually(t, func() bool { return fresh.NetworkSnapshot().Up }, 5*time.Second, 10*time.Millisecond)
	require.Zero(t, fresh.NetworkSnapshot().Resets)
}
