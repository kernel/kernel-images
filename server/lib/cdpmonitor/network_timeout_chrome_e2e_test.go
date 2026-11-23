package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

func TestNetworkFailureTLSHandshakeTimeoutChrome(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	// Accept TCP but never answer TLS. Chromium's SSLConnectJob has a 30s
	// handshake timeout, unlike the platform-dependent TCP SYN timeout.
	stalled, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer stalled.Close()
	go func() {
		for {
			conn, err := stalled.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				<-ctx.Done()
			}()
		}
	}()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<link rel="icon" href="data:,">timeout fixture`)
	}))
	defer origin.Close()
	browserWS := launchChromium(t, ctx, findChromium(t))
	driver := dialCDP(t, ctx, browserWS)
	defer driver.close()
	target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": origin.URL}).targetID(t)
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	require.Eventually(t, func() bool { return driver.evalBool(ctx, session, `document.readyState === 'complete'`) }, 10*time.Second, 20*time.Millisecond)
	m := New(newTestUpstream(browserWS), newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, 10*time.Second, 20*time.Millisecond)

	observer, err := cdpclient.DialWithEvents(ctx, browserWS)
	require.NoError(t, err)
	defer observer.Close()
	attached, err := observer.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, "")
	require.NoError(t, err)
	var observed struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(attached, &observed))
	failures := make(chan cdpNetworkLoadingFailedParams, 1)
	go func() {
		for message := range observer.Events() {
			if message.Method == "Network.loadingFailed" && message.SessionID == observed.SessionID {
				var p cdpNetworkLoadingFailedParams
				if json.Unmarshal(message.Params, &p) == nil {
					select {
					case failures <- p:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	_, err = observer.Send(ctx, "Network.enable", nil, observed.SessionID)
	require.NoError(t, err)
	before := m.NetworkSnapshot()
	script := fmt.Sprintf(`fetch(%q).then(()=>"unexpected success", e=>e.name)`, "https://"+stalled.Addr().String()+"/timeout")
	require.JSONEq(t, `"TypeError"`, string(evaluateNetworkScript(t, ctx, driver, session, script)))
	select {
	case failure := <-failures:
		require.Equal(t, "net::ERR_TIMED_OUT", failure.ErrorText)
		require.False(t, failure.Canceled)
		t.Logf("independent CDP observed %s (canceled=%t)", failure.ErrorText, failure.Canceled)
	case <-ctx.Done():
		t.Fatal("missing independent TLS timeout outcome")
	}
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed == before.Completed+1 }, 5*time.Second, 10*time.Millisecond)
	after := m.NetworkSnapshot()
	assertNetworkTotals(t, after, failureTotal(before)+1, before.Completed+1)
	require.Equal(t, before.Failures["ERR_TIMED_OUT"][0]+1, after.Failures["ERR_TIMED_OUT"][0])
	require.Equal(t, before.Resets, after.Resets)
}
