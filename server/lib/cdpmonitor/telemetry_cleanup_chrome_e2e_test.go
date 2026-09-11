package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/telemetry"
	"github.com/stretchr/testify/require"
)

func TestTelemetryCleanupAfterSocketLossChrome(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1")
	}
	for _, mode := range []string{"disable_during_disconnect", "disable_after_reconnect", "failed_listener_cleanup", "failed_registration_removal"} {
		t.Run(mode, func(t *testing.T) { testTelemetryCleanupRecovery(t, mode) })
	}
}

func testTelemetryCleanupRecovery(t *testing.T, mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<link rel="icon" href="data:,"><button id="go">go</button>`)
	}))
	defer stub.Close()
	ws := launchChromium(t, ctx, findChromium(t), "--site-per-process")
	driver := dialCDP(t, ctx, ws)
	defer driver.close()
	target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL}).targetID(t)
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	stream, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: 128})
	require.NoError(t, err)
	ts := telemetry.NewTelemetrySession(stream)
	monitorURL := ws
	var failed *atomic.Bool
	if strings.HasPrefix(mode, "failed_") {
		monitorURL, failed = failCleanupProxy(t, ctx, ws, mode == "failed_registration_removal")
	}
	mon := New(newTestUpstream(monitorURL), ts.Publish, 0, discardLogger, func() bool { return false })
	require.NoError(t, mon.SetTelemetry(false))
	require.NoError(t, mon.Start(ctx))
	defer mon.Stop()
	ts.Start("fixture", telemetry.TelemetryConfig{Categories: []oapi.TelemetryEventCategory{events.Interaction}})
	require.NoError(t, mon.SetTelemetry(true))
	require.Eventually(t, func() bool { return driver.evalBool(ctx, session, `window.__kernelEventInjected === true`) }, 5*time.Second, 10*time.Millisecond)
	cross := strings.Replace(stub.URL, "127.0.0.1", "localhost", 1) + "/frame"
	evaluateNetworkScript(t, ctx, driver, session, fmt.Sprintf(`document.body.insertAdjacentHTML('beforeend', '<iframe id="same" src="/frame"></iframe><iframe src="%s"></iframe>'); true`, cross))
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, `document.querySelector('#same').contentWindow.__kernelEventInjected === true`)
	}, 5*time.Second, 10*time.Millisecond)
	crossTarget := findNetworkFrameTarget(t, ctx, driver, cross)
	crossSession := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": crossTarget, "flatten": true}).sessionID(t)
	require.Eventually(t, func() bool { return driver.evalBool(ctx, crossSession, `window.__kernelEventInjected === true`) }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	before := mon.NetworkSnapshot()
	mon.lifeMu.Lock()
	old := mon.conn
	mon.lifeMu.Unlock()
	if failed == nil {
		require.NoError(t, old.protocol.Close())
	}
	if mode == "disable_after_reconnect" {
		require.Eventually(t, func() bool {
			mon.lifeMu.Lock()
			fresh := mon.conn != nil && mon.conn != old
			mon.lifeMu.Unlock()
			return fresh && mon.NetworkSnapshot().Up && driver.evalBool(ctx, session, `window.__kernelEventInjected === true`)
		}, 10*time.Second, 10*time.Millisecond)
	}
	ts.Stop()
	require.NoError(t, mon.SetTelemetry(false))
	waitForTelemetryReconcile(t, mon, false)
	if failed != nil {
		require.True(t, failed.Load(), "cleanup failure was not injected")
	}
	after := mon.NetworkSnapshot()
	require.Equal(t, before.Resets, after.Resets)
	require.Equal(t, before.Completed, after.Completed, "cleanup/reconnect must not reset or increment request counters")
	for _, frame := range []struct{ session, realm string }{{session, "window"}, {session, `document.querySelector('#same').contentWindow`}, {crossSession, "window"}} {
		raw := driver.call(t, ctx, frame.session, "Runtime.evaluate", map[string]any{
			"expression":            fmt.Sprintf(`({injected:!!%s.__kernelEventInjected, clicks:(getEventListeners(%s.document).click||[]).length})`, frame.realm, frame.realm),
			"includeCommandLineAPI": true, "returnByValue": true,
		})
		var result struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				ExceptionDetails json.RawMessage `json:"exceptionDetails"`
				Result           struct {
					Value *struct {
						Injected bool `json:"injected"`
						Clicks   int  `json:"clicks"`
					} `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw.raw, &result))
		require.Empty(t, result.Error)
		require.Empty(t, result.Result.ExceptionDetails)
		require.NotNil(t, result.Result.Result.Value)
		require.False(t, result.Result.Result.Value.Injected, "orphan interaction instrumentation in %s", frame.realm)
		require.Zero(t, result.Result.Result.Value.Clicks, "orphan click listener in %s", frame.realm)
	}
	seq := stream.Seq()
	evaluateNetworkScript(t, ctx, driver, session, `document.querySelector('#go').click(); true`)
	require.Equal(t, seq, stream.Seq())
	// The user's sessions must still navigate, without resurrecting registrations
	// in either existing child frame or the top-level document.
	evaluateNetworkScript(t, ctx, driver, session, `document.querySelector('#same').src = '/frame-next'; true`)
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, `document.querySelector('#same').contentWindow.location.pathname === '/frame-next' && document.querySelector('#same').contentDocument.readyState === 'complete'`)
	}, 5*time.Second, 10*time.Millisecond)
	require.False(t, driver.evalBool(ctx, session, `document.querySelector('#same').contentWindow.__kernelEventInjected === true`))
	driver.call(t, ctx, crossSession, "Page.navigate", map[string]any{"url": cross + "-next"})
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, crossSession, `location.pathname === '/frame-next' && document.readyState === 'complete'`)
	}, 5*time.Second, 10*time.Millisecond)
	require.False(t, driver.evalBool(ctx, crossSession, `window.__kernelEventInjected === true`))
	driver.call(t, ctx, session, "Page.navigate", map[string]any{"url": stub.URL + "/next"})
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, `location.pathname === '/next' && document.readyState === 'complete'`)
	}, 5*time.Second, 10*time.Millisecond)
	require.False(t, driver.evalBool(ctx, session, `window.__kernelEventInjected === true`))
}

// Fail registration removal or listener cleanup once, leaving the user CDP client
// untouched. The replacement monitor socket must retry the retained obligation.
func failCleanupProxy(t *testing.T, parent context.Context, upstreamURL string, failRemoval bool) (string, *atomic.Bool) {
	t.Helper()
	failed := &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer client.CloseNow()
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		upstream, _, err := websocket.Dial(ctx, upstreamURL, nil)
		if err != nil {
			return
		}
		defer upstream.CloseNow()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer cancel()
			for {
				kind, data, err := upstream.Read(ctx)
				if err != nil {
					return
				}
				if client.Write(ctx, kind, data) != nil {
					return
				}
			}
		}()
		defer func() { cancel(); <-done }()
		for {
			kind, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			var command struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params struct {
					Source string `json:"source"`
				} `json:"params"`
			}
			if json.Unmarshal(data, &command) != nil {
				return
			}
			failCommand := command.Method == "Page.addScriptToEvaluateOnNewDocument" && command.Params.Source == cleanupInteractionJS
			if failRemoval {
				failCommand = command.Method == "Page.removeScriptToEvaluateOnNewDocument"
			}
			if failCommand && failed.CompareAndSwap(false, true) {
				if wsjson.Write(ctx, client, map[string]any{"id": command.ID, "error": map[string]any{"code": -32000, "message": "fixture cleanup failure"}}) != nil {
					return
				}
				continue
			}
			if upstream.Write(ctx, kind, data) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), failed
}
