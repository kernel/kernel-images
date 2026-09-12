package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLateInjectionAfterTargetDestructionChrome(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1")
	}
	for _, outcome := range []string{"success", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<link rel="icon" href="data:,"><button>go</button>`)
			}))
			defer page.Close()
			ws := launchChromium(t, ctx, findChromium(t))
			driver := dialCDP(t, ctx, ws)
			defer driver.close()
			target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": page.URL}).targetID(t)
			session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
			var armed atomic.Bool
			proxy, blocked, release := delayCommandReplies(t, ctx, ws, func(method string) bool {
				return armed.Load() && method == "Page.addScriptToEvaluateOnNewDocument"
			})
			m := New(newTestUpstream(proxy), newEventCollector().publishFn(), 0, discardLogger, func() bool { return false })
			require.NoError(t, m.Start(ctx))
			defer m.Stop()
			defer release()
			require.Eventually(t, func() bool {
				return m.NetworkSnapshot().Up && driver.evalBool(ctx, session, `window.__kernelEventInjected === true`)
			}, 5*time.Second, time.Millisecond)
			m.captureWg.Wait()
			completed := m.NetworkSnapshot().Completed
			result := evaluateNetworkScript(t, ctx, driver, session, `fetch('/count', {method:'POST'}).then(async r => { await r.text(); return r.ok; })`)
			require.JSONEq(t, "true", string(result))
			require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed == completed+1 }, time.Second, time.Millisecond)
			m.sessionsMu.RLock()
			monitorSession := ""
			for id, info := range m.sessions {
				if info.targetID == target {
					monitorSession = id
				}
			}
			_, dirty := m.interactionTargets[target]
			m.sessionsMu.RUnlock()
			require.NotEmpty(t, monitorSession)
			require.True(t, dirty)
			m.lifeMu.Lock()
			original := m.conn
			m.lifeMu.Unlock()
			before := m.NetworkSnapshot()

			// Retry a registration on an instrumented target. Chrome executes it;
			// only its acknowledgement is held while the independent client closes it.
			armed.Store(true)
			injectCtx, cancelInjection := context.WithCancel(ctx)
			defer cancelInjection()
			done := make(chan error, 1)
			m.captureWg.Go(func() {
				m.telemetryMu.RLock()
				err := m.injectScript(injectCtx, monitorSession)
				m.telemetryMu.RUnlock()
				done <- err
			})
			select {
			case sid := <-blocked:
				require.Equal(t, monitorSession, sid)
			case <-time.After(time.Second):
				t.Fatal("registration acknowledgement was not intercepted")
			}
			raw := driver.call(t, ctx, "", "Target.closeTarget", map[string]any{"targetId": target})
			var closed struct {
				Result struct {
					Success bool `json:"success"`
				} `json:"result"`
			}
			require.NoError(t, json.Unmarshal(raw.raw, &closed))
			require.True(t, closed.Result.Success)
			require.Eventually(t, func() bool {
				m.sessionsMu.RLock()
				defer m.sessionsMu.RUnlock()
				_, dirty := m.interactionTargets[target]
				return !dirty
			}, time.Second, time.Millisecond, "monitor did not process target destruction")
			if outcome == "success" {
				release()
			} else {
				cancelInjection()
			}
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("registration did not finish")
			}
			release()
			require.NoError(t, m.SetTelemetry(false))
			waitForTelemetryReconcile(t, m, false)
			require.Never(t, func() bool {
				m.lifeMu.Lock()
				changed := m.conn != original
				m.lifeMu.Unlock()
				return changed || !m.NetworkSnapshot().Up
			}, 300*time.Millisecond, time.Millisecond, "destroyed target forced healthy connection replacement")
			require.Equal(t, before.Resets, m.NetworkSnapshot().Resets)
			require.Equal(t, before.Completed, m.NetworkSnapshot().Completed)
			driver.call(t, ctx, "", "Browser.getVersion", nil)
		})
	}
}
