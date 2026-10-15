package cdpmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestNetworkWireCapture(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var mu sync.Mutex
	var wireHeaders http.Header
	var statuses []int
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			mu.Lock()
			wireHeaders = r.Header.Clone()
			wireHeaders.Set("Host", r.Host)
			status := 200
			if r.Header.Get("If-None-Match") != "" {
				status = 304
			}
			statuses = append(statuses, status)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(status)
			if status == 200 {
				fmt.Fprint(w, `{"ok":true}`)
			}
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>local reproduction</html>")
		}
	}))
	defer stub.Close()
	ws := launchChromium(t, ctx, findChromium(t), "--site-per-process")
	driver := dialCDP(t, ctx, ws)
	defer driver.close()
	target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}).targetID(t)
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	ec := newEventCollector()
	m := New(&staticUpstream{url: ws}, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	require.Eventually(t, func() bool { return driver.evalBool(ctx, session, "window.__kernelEventInjected === true") }, 10*time.Second, 20*time.Millisecond)
	driver.call(t, ctx, session, "Page.navigate", map[string]any{"url": stub.URL + "/document"})
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, fmt.Sprintf(`location.href === %q && document.readyState === "complete"`, stub.URL+"/document"))
	}, 10*time.Second, 20*time.Millisecond)
	driver.call(t, ctx, session, "Runtime.evaluate", map[string]any{"expression": `document.cookie="test_cookie=present; path=/"`})
	for range 2 {
		result := driver.call(t, ctx, session, "Runtime.evaluate", map[string]any{"expression": `fetch("/api").then(r => r.text())`, "awaitPromise": true, "returnByValue": true})
		require.NotContains(t, string(result.raw), "exceptionDetails")
	}
	snapshot := func(typ, url string) []map[string]any {
		ec.mu.Lock()
		defer ec.mu.Unlock()
		var out []map[string]any
		for _, ev := range ec.events {
			if ev.Type != typ {
				continue
			}
			var p map[string]any
			require.NoError(t, json.Unmarshal(ev.Data, &p))
			if p["url"] == url {
				out = append(out, p)
			}
		}
		return out
	}
	require.Eventually(t, func() bool { return len(snapshot(EventNetworkResponse, stub.URL+"/api")) == 2 }, 5*time.Second, 20*time.Millisecond)
	mu.Lock()
	require.Equal(t, []int{200, 304}, statuses)
	require.Equal(t, "test_cookie=present", wireHeaders.Get("Cookie"))
	mu.Unlock()
	t.Run("document_response", func(t *testing.T) {
		require.Eventually(t, func() bool { return len(snapshot(EventNetworkResponse, stub.URL+"/document")) == 1 }, time.Second, 20*time.Millisecond, "completed document load must emit a response")
	})
	t.Run("cookie_header", func(t *testing.T) {
		requests := snapshot(EventNetworkRequest, stub.URL+"/api")
		require.Len(t, requests, 2)
		headers := requests[0]["headers"].(map[string]any)
		require.Contains(t, headers, "Cookie", "server received Cookie, telemetry must capture it")
	})
	t.Run("wire_status", func(t *testing.T) {
		responses := snapshot(EventNetworkResponse, stub.URL+"/api")
		require.ElementsMatch(t, []any{float64(200), float64(304)}, []any{responses[0]["wire_status"], responses[1]["wire_status"]})
		for _, response := range responses {
			require.Equal(t, float64(200), response["status"])
			require.Equal(t, "OK", response["status_text"])
			require.JSONEq(t, `{"ok":true}`, response["body"].(string))
			require.NotContains(t, response, "effective_status")
		}
	})
}

func TestNetworkBodiesSurviveNavigation(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"body":"retained"}`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>body retention</html>")
	}))
	defer stub.Close()
	ws := launchChromium(t, ctx, findChromium(t), "--site-per-process")
	driver := dialCDP(t, ctx, ws)
	defer driver.close()
	target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}).targetID(t)
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	ec := newEventCollector()
	m := New(&staticUpstream{url: ws}, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	require.Eventually(t, func() bool { return driver.evalBool(ctx, session, "window.__kernelEventInjected === true") }, 10*time.Second, 20*time.Millisecond)
	driver.call(t, ctx, session, "Page.navigate", map[string]any{"url": stub.URL + "/document"})
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, fmt.Sprintf(`location.href === %q && document.readyState === "complete"`, stub.URL+"/document"))
	}, 5*time.Second, 20*time.Millisecond)
	evaluateNetworkScript(t, ctx, driver, session, `fetch("/api").then(r=>r.text())`)
	response := waitForNetworkURL(t, ec, EventNetworkResponse, stub.URL+"/api")
	var payload oapi.BrowserNetworkResponseEventData
	require.NoError(t, json.Unmarshal(response.Data, &payload))
	require.NotNil(t, payload.Body)
	require.JSONEq(t, `{"body":"retained"}`, *payload.Body)
	destination := strings.Replace(stub.URL, "127.0.0.1", "localhost", 1) + "/next"
	driver.call(t, ctx, session, "Page.navigate", map[string]any{"url": destination})
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, fmt.Sprintf(`location.href === %q && document.readyState === "complete"`, destination))
	}, 5*time.Second, 20*time.Millisecond)
	// Retrieve through the monitor's own session after the renderer has changed.
	body, err := m.fetchResponseBody(ctx, payload.RequestId, payload.SessionId, networkReqState{resourceType: "Fetch", mimeType: "application/json"})
	require.NoError(t, err)
	require.JSONEq(t, `{"body":"retained"}`, body)
	require.NoError(t, m.SetTelemetry(false))
	waitForTelemetryReconcile(t, m, false)
	observer, err := cdpclient.DialWithEvents(ctx, ws)
	require.NoError(t, err)
	defer observer.Close()
	attached, err := observer.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, "")
	require.NoError(t, err)
	var observedSession struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(attached, &observedSession))
	_, err = observer.Send(ctx, "Network.enable", nil, observedSession.SessionID)
	require.NoError(t, err)
	before := m.NetworkSnapshot().Completed
	evaluateNetworkScript(t, ctx, driver, session, `fetch("/api?after-disable").then(r=>r.text())`)
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Completed > before }, time.Second, 20*time.Millisecond)
	var uncapturedRequestID string
	for uncapturedRequestID == "" {
		select {
		case message := <-observer.Events():
			var p cdpNetworkRequestWillBeSentParams
			if message.Method == "Network.requestWillBeSent" && json.Unmarshal(message.Params, &p) == nil && strings.HasSuffix(p.Request.URL, "/api?after-disable") {
				uncapturedRequestID = p.RequestID
			}
		case <-ctx.Done():
			t.Fatal("missing request after capture was disabled")
		}
	}
	driver.call(t, ctx, session, "Page.navigate", map[string]any{"url": stub.URL + "/back"})
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, fmt.Sprintf(`location.href === %q && document.readyState === "complete"`, stub.URL+"/back"))
	}, 5*time.Second, 20*time.Millisecond)
	_, err = m.fetchResponseBody(ctx, uncapturedRequestID, payload.SessionId, networkReqState{resourceType: "Fetch", mimeType: "application/json"})
	require.Error(t, err, "disabled capture must not retain new bodies across navigation")
}

func TestNetworkExtensionAttribution(t *testing.T) {
	if os.Getenv("KERNEL_CDPMONITOR_CHROME_E2E") == "" {
		t.Skip("set KERNEL_CDPMONITOR_CHROME_E2E=1 to run real-Chromium network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"from":"extension"}`)
	}))
	defer stub.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"manifest_version":3,"name":"telemetry test","version":"1.0","background":{"service_worker":"background.js"},"host_permissions":["http://127.0.0.1/*"]}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "background.js"), []byte(`chrome.runtime.onInstalled.addListener(() => {});`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "page.html"), []byte(`<html>extension page</html>`), 0600))
	ws := launchChromium(t, ctx, findChromium(t), "--enable-unsafe-extension-debugging")
	c, err := cdpclient.Dial(ctx, ws)
	require.NoError(t, err)
	defer c.Close()
	ec := newEventCollector()
	m := New(&staticUpstream{url: ws}, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	extensionID, err := c.LoadUnpackedExtension(ctx, dir)
	require.NoError(t, err)
	var target string
	require.Eventually(t, func() bool {
		raw, err := c.Send(ctx, "Target.getTargets", nil, "")
		if err != nil {
			return false
		}
		var p struct {
			Targets []cdpTargetTargetInfo `json:"targetInfos"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return false
		}
		for _, info := range p.Targets {
			if info.Type == "service_worker" && strings.HasPrefix(info.URL, "chrome-extension://"+extensionID+"/") {
				target = info.TargetID
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	driver := dialCDP(t, ctx, ws)
	defer driver.close()
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	// Wait for the collector's own attachment before driving the request.
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		for sid, info := range m.sessions {
			if info.targetID == target && m.networkReady[sid] {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	evaluateNetworkScript(t, ctx, driver, session, fmt.Sprintf(`fetch(%q).then(r=>r.json())`, stub.URL+"/extension-api"))
	request := waitForNetworkURL(t, ec, EventNetworkRequest, stub.URL+"/extension-api")
	response := waitForNetworkURL(t, ec, EventNetworkResponse, stub.URL+"/extension-api")
	var p map[string]any
	require.NoError(t, json.Unmarshal(request.Data, &p))
	require.Equal(t, "service_worker", p["target_type"])
	require.Equal(t, target, p["target_id"])
	require.Equal(t, extensionID, (*request.Source.Metadata)["extension_id"])
	require.Equal(t, extensionID, (*response.Source.Metadata)["extension_id"])
	require.Equal(t, "false", (*request.Source.Metadata)["kernel_extension"])

	page := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}).targetID(t)
	pageSession := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": page, "flatten": true}).sessionID(t)
	require.Eventually(t, func() bool { return driver.evalBool(ctx, pageSession, "window.__kernelEventInjected === true") }, 5*time.Second, 20*time.Millisecond)
	for index, destination := range []string{"chrome-extension://" + extensionID + "/page.html", stub.URL + "/page"} {
		driver.call(t, ctx, pageSession, "Page.navigate", map[string]any{"url": destination})
		require.Eventually(t, func() bool {
			return driver.evalBool(ctx, pageSession, fmt.Sprintf(`location.href === %q && document.readyState === "complete"`, destination))
		}, 5*time.Second, 20*time.Millisecond)
		url := fmt.Sprintf("%s/page-api-%d", stub.URL, index)
		evaluateNetworkScript(t, ctx, driver, pageSession, fmt.Sprintf(`fetch(%q).then(r=>r.json())`, url))
		for _, typ := range []string{EventNetworkRequest, EventNetworkResponse} {
			event := waitForNetworkURL(t, ec, typ, url)
			if index == 0 {
				require.Equal(t, extensionID, (*event.Source.Metadata)["extension_id"])
			} else {
				require.NotContains(t, *event.Source.Metadata, "extension_id")
				require.NotContains(t, *event.Source.Metadata, "kernel_extension")
			}
		}
		idle := waitForNetworkURL(t, ec, EventNetworkIdle, destination)
		if index == 0 {
			require.Equal(t, extensionID, (*idle.Source.Metadata)["extension_id"])
			require.Equal(t, "false", (*idle.Source.Metadata)["kernel_extension"])
		} else {
			require.NotContains(t, *idle.Source.Metadata, "extension_id")
			require.NotContains(t, *idle.Source.Metadata, "kernel_extension")
		}
	}
}

func TestNetworkBundledExtensionAttribution(t *testing.T) {
	dir := os.Getenv("KERNEL_CDPMONITOR_BUNDLED_EXTENSION_PATH")
	if dir == "" {
		t.Skip("set KERNEL_CDPMONITOR_BUNDLED_EXTENSION_PATH to a bundled extension at its installed path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"from":"bundled-extension"}`)
	}))
	defer stub.Close()
	ws := launchChromium(t, ctx, findChromium(t), "--enable-unsafe-extension-debugging")
	c, err := cdpclient.Dial(ctx, ws)
	require.NoError(t, err)
	defer c.Close()
	extensionID, err := c.LoadUnpackedExtension(ctx, dir)
	require.NoError(t, err)
	t.Logf("Chromium loaded %s as %s", dir, extensionID)
	driver := dialCDP(t, ctx, ws)
	defer driver.close()
	target := driver.call(t, ctx, "", "Target.createTarget", map[string]any{"url": "chrome-extension://" + extensionID + "/manifest.json"}).targetID(t)
	session := driver.call(t, ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}).sessionID(t)
	ec := newEventCollector()
	m := New(&staticUpstream{url: ws}, ec.publishFn(), 99, discardLogger, nil)
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	require.Eventually(t, func() bool {
		return driver.evalBool(ctx, session, fmt.Sprintf(`location.hostname === %q && window.__kernelEventInjected === true`, extensionID))
	}, 5*time.Second, 20*time.Millisecond)
	evaluateNetworkScript(t, ctx, driver, session, fmt.Sprintf(`fetch(%q).then(r=>r.json())`, stub.URL+"/bundled-api"))
	for _, typ := range []string{EventNetworkRequest, EventNetworkResponse} {
		event := waitForNetworkURL(t, ec, typ, stub.URL+"/bundled-api")
		require.Equal(t, extensionID, (*event.Source.Metadata)["extension_id"])
		require.Equal(t, "true", (*event.Source.Metadata)["kernel_extension"])
	}
}
