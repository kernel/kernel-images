package cdpmonitor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

func TestNetworkMetricsActualChromeRestart(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			_, _ = io.Copy(io.Discard, r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<link rel="icon" href="data:,">fixture`)
	}))
	defer stub.Close()
	chrome := findChromium(t)
	ws := launchChromium(t, ctx, chrome)
	upstream := newTestUpstream(ws)
	m := New(upstream, newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	reset := func(ws string) {
		driver := dialCDP(t, ctx, ws)
		defer driver.close()
		target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL}).targetID(t)
		session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
		require.Eventually(t, func() bool {
			return m.NetworkSnapshot().Up && driver.evalBool(ctx, session, `document.readyState === 'complete'`)
		}, 10*time.Second, 20*time.Millisecond)
		// Require this particular target, not just the previously-known target set.
		require.Eventually(t, func() bool {
			m.sessionsMu.RLock()
			defer m.sessionsMu.RUnlock()
			for id, info := range m.sessions {
				if info.targetID == target && m.networkReady[id] {
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond)
		b := m.NetworkSnapshot()
		value := evaluateNetworkScript(t, ctx, driver, session, `fetch('/',{method:'POST',body:'fixture'}).then(()=>false,()=>true)`)
		require.JSONEq(t, "true", string(value))
		require.Eventually(t, func() bool { return m.NetworkSnapshot().Resets == b.Resets+1 }, 5*time.Second, 10*time.Millisecond)
	}
	reset(ws)
	before := m.NetworkSnapshot()
	require.Equal(t, uint64(1), before.Resets)
	killer, err := cdpclient.Dial(ctx, ws)
	require.NoError(t, err)
	_, _ = killer.Send(ctx, "Browser.close", nil, "")
	require.Eventually(t, killer.IsClosed, 5*time.Second, 10*time.Millisecond)
	_ = killer.Close()
	require.Eventually(t, func() bool { return !m.NetworkSnapshot().Up }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, before.Resets, m.NetworkSnapshot().Resets)
	ws = launchChromium(t, ctx, chrome)
	upstream.notifyRestart(ws)
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, before.Resets, m.NetworkSnapshot().Resets)
	require.GreaterOrEqual(t, m.NetworkSnapshot().Completed, before.Completed)
	reset(ws)
	require.Equal(t, uint64(2), m.NetworkSnapshot().Resets)
}
