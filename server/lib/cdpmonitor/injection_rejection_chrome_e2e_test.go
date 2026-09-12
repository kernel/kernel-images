package cdpmonitor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRejectedInjectionDoesNotReconnectChrome(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<link rel="icon" href="data:,"><button id="go">go</button>`)
	}))
	defer page.Close()
	ws := launchChromium(t, ctx, findChromium(t))
	driver := dialCDP(t, ctx, ws)
	defer driver.close()
	target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": page.URL}).targetID(t)
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, `document.readyState === 'complete' && !!document.querySelector('#go')`)
	}, 5*time.Second, time.Millisecond)
	var rejections atomic.Int32
	proxy := rejectPageCommandProxy(t, ctx, ws, func(method, source string) bool {
		if method == "Page.addScriptToEvaluateOnNewDocument" {
			rejections.Add(1)
			return true
		}
		return false
	})
	defer func() { t.Logf("injected Page command rejections: %d", rejections.Load()) }()
	m := New(newTestUpstream(proxy), newEventCollector().publishFn(), 0, discardLogger, func() bool { return false })
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	waitForTelemetryReconcile(t, m, false)
	for cycle := 0; cycle < 2; cycle++ {
		prior := rejections.Load()
		require.NoError(t, m.SetTelemetry(true))
		require.Eventually(t, func() bool { return rejections.Load() > prior && !m.telemetryChanging.Load() }, time.Second, time.Millisecond)
		m.captureWg.Wait()
		injected := driver.evalBool(ctx, session, `window.__kernelEventInjected === true`)
		t.Logf("injected after rejected registration: %t", injected)
		m.lifeMu.Lock()
		original := m.conn
		m.lifeMu.Unlock()
		rejected := rejections.Load()
		require.NoError(t, m.SetTelemetry(false))
		waitForTelemetryReconcile(t, m, false)
		require.False(t, injected, "confirmed rejection must not install live-document listeners")
		require.Equal(t, rejected, rejections.Load(), "no cleanup registration is needed without injection")
		assertStable := func(conn *monitorConnection) {
			t.Helper()
			require.Never(t, func() bool {
				m.lifeMu.Lock()
				changed := m.conn != conn
				m.lifeMu.Unlock()
				return changed || !m.NetworkSnapshot().Up
			}, time.Second, time.Millisecond, "optional rejection caused monitor reconnects")
		}
		assertStable(original)
		completed := m.NetworkSnapshot().Completed
		result := evaluateNetworkScript(t, ctx, driver, session, `fetch('/observed', {method:'POST'}).then(async r => { await r.text(); return r.ok; })`)
		require.JSONEq(t, "true", string(result))
		require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed == completed+1 }, time.Second, time.Millisecond)
		before := m.NetworkSnapshot()
		require.NoError(t, original.protocol.Close())
		require.Eventually(t, func() bool {
			m.lifeMu.Lock()
			fresh := m.conn != nil && m.conn != original
			m.lifeMu.Unlock()
			return fresh && m.NetworkSnapshot().Up
		}, 3*time.Second, time.Millisecond)
		waitForTelemetryReconcile(t, m, false)
		m.lifeMu.Lock()
		fresh := m.conn
		m.lifeMu.Unlock()
		assertStable(fresh)
		require.Equal(t, rejected, rejections.Load())
		require.Equal(t, before.Resets, m.NetworkSnapshot().Resets)
		require.Equal(t, before.Completed, m.NetworkSnapshot().Completed)
		// The independent user connection remains usable, including navigation.
		driver.call(t, ctx, session, "Page.navigate", map[string]any{"url": page.URL + fmt.Sprintf("/next-%d", cycle)})
		require.Eventually(t, func() bool {
			return driver.evalBool(ctx, session, fmt.Sprintf(`location.pathname === '/next-%d' && document.readyState === 'complete'`, cycle))
		}, time.Second, time.Millisecond)
		require.False(t, driver.evalBool(ctx, session, `window.__kernelEventInjected === true`))
	}
}
