package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/devtoolsproxy"
	"github.com/stretchr/testify/require"
)

func TestRewriteChromeURLs_RewritesNestedMapsAndArrays(t *testing.T) {
	chromeHost := "127.0.0.1:9223"
	proxyHost := "127.0.0.1:4444"

	payload := map[string]interface{}{
		"webSocketDebuggerUrl": "ws://127.0.0.1:9223/devtools/browser/root",
		"nested": map[string]interface{}{
			"devtoolsFrontendUrl": "https://chrome-devtools-frontend.appspot.com/serve_rev/@abc/inspector.html?ws=127.0.0.1:9223/devtools/page/nested",
		},
		"targets": []interface{}{
			map[string]interface{}{
				"webSocketDebuggerUrl": "ws://127.0.0.1:9223/devtools/page/in-array",
			},
		},
	}

	rewriteChromeURLs(payload, chromeHost, proxyHost)

	require.Equal(t,
		"ws://127.0.0.1:4444/devtools/browser/root",
		payload["webSocketDebuggerUrl"],
	)

	nested, ok := payload["nested"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t,
		"https://chrome-devtools-frontend.appspot.com/serve_rev/@abc/inspector.html?ws=127.0.0.1%3A4444%2Fdevtools%2Fpage%2Fnested",
		nested["devtoolsFrontendUrl"],
	)

	targets, ok := payload["targets"].([]interface{})
	require.True(t, ok)
	first, ok := targets[0].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t,
		"ws://127.0.0.1:4444/devtools/page/in-array",
		first["webSocketDebuggerUrl"],
	)
}

func TestChromeJSONProxyHandler_CancelsUpstreamRequestWithCallerContext(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	parsedUpstream, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	logPath := filepath.Join(t.TempDir(), "chromium.log")
	require.NoError(t, os.WriteFile(logPath, nil, 0o644))

	mgr := devtoolsproxy.NewUpstreamManager(logPath, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(mgr.Stop)
	mgr.Start(ctx)

	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = logFile.WriteString("DevTools listening on ws://" + parsedUpstream.Host + "/devtools/browser/root\n")
	require.NoError(t, err)
	require.NoError(t, logFile.Close())

	_, err = mgr.WaitForInitial(2 * time.Second)
	require.NoError(t, err)

	handler := chromeJSONProxyHandler(mgr, logger, "/json/version")

	reqCtx, reqCancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://proxy/json/version", nil).WithContext(reqCtx)
	req.Host = "proxy.local"
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request to start")
	}

	reqCancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to return")
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request context cancellation")
	}

	require.Equal(t, http.StatusBadGateway, rec.Code)
}
