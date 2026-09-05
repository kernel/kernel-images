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

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestTelemetryConnectionOwnershipAndReconnect(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run the connection ownership test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.Copy(w, r.Body)
			return
		}
		fmt.Fprint(w, "<html><body>connection ownership</body></html>")
	}))
	defer stub.Close()
	browserWS := launchChromium(t, ctx, findChromium(t))
	driver := dialCDP(t, ctx, browserWS)
	defer driver.close()
	targetID := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": stub.URL}).targetID(t)
	driverSession := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}).sessionID(t)

	// This follows WebMCP's ownership pattern: its own protocol and default tracker.
	otherProtocol, err := cdpclient.DialWithEvents(ctx, browserWS)
	require.NoError(t, err)
	defer otherProtocol.Close()
	otherSurface := browsersurface.New(otherProtocol)
	otherEvents, unsubscribe := otherSurface.Subscribe()
	defer unsubscribe()
	require.NoError(t, otherSurface.Start(ctx))
	var otherSession string
	for otherSession == "" {
		select {
		case event := <-otherEvents:
			if event.Kind == browsersurface.EventSessionAttached && event.Target.ID == targetID {
				otherSession = event.SessionID
			}
		case <-ctx.Done():
			t.Fatal("independent tracker did not attach the page")
		}
	}

	upstream := newTestUpstream(browserWS)
	ec := newEventCollector()
	m := New(upstream, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, driverSession, `document.readyState === 'complete' && window.__kernelEventInjected === true`)
	}, 5*time.Second, 50*time.Millisecond)
	m.Stop()
	require.True(t, otherSurface.SessionExists(otherSession))
	result, err := otherProtocol.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "6 * 7", "returnByValue": true}, otherSession)
	require.NoError(t, err)
	var evaluation struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(result, &evaluation))
	require.Equal(t, 42, evaluation.Result.Value)

	require.NoError(t, m.Start(ctx))
	require.NoError(t, otherProtocol.Close())
	<-otherSurface.Done()
	capture := func(marker string) string {
		t.Helper()
		time.Sleep(500 * time.Millisecond)
		raw := evaluateNetworkScript(t, ctx, driver, driverSession, fmt.Sprintf(`(async () => {
			const response = await fetch('/post?marker=%s', {method: 'POST', body: %q});
			return {status: response.status, body: await response.text()};
		})()`, marker, marker))
		var result struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, 200, result.Status)
		require.Equal(t, marker, result.Body)
		event := waitForNetworkURL(t, ec, EventNetworkResponse, stub.URL+"/post?marker="+marker)
		var response oapi.BrowserNetworkResponseEventData
		require.NoError(t, json.Unmarshal(event.Data, &response))
		require.NotNil(t, response.Body)
		require.Equal(t, marker, *response.Body)
		return response.SessionId
	}
	before := capture("before-reconnect")
	require.NotEqual(t, otherSession, before)
	checkpoint := ec.checkpoint()
	upstream.notifyRestart(browserWS)
	ec.waitForNew(t, EventMonitorReconnected, checkpoint, 5*time.Second)
	after := capture("after-reconnect")
	require.NotEmpty(t, after)
	require.NotEqual(t, before, after, "reconnect must use fresh telemetry sessions")
}
