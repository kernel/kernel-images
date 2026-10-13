package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kernel/kernel-images/server/lib/devtoolsproxy"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBrowserLocationBundle(t *testing.T) {
	zone := filepath.Join("/usr/share/zoneinfo", "America", "New_York")
	if _, err := os.Stat(zone); err != nil {
		t.Skip("zoneinfo unavailable")
	}

	for _, locale := range []string{"en-US", "de-DE", "pt-BR", "es-MX", "en-SG", "zh-TW"} {
		raw := fmt.Sprintf(`{"generation":2,"timezone":"America/New_York","locale":%q,"languages":[%q]}`, locale, locale)
		bundle, err := validateBrowserLocationBundle(raw)
		require.NoError(t, err, locale)
		assert.Equal(t, locale, bundle.Locale)
	}
	bundle, err := validateBrowserLocationBundle(`{"generation":2,"timezone":"America/New_York","locale":"en-US","languages":["en-US","en"]}`)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), bundle.Generation)
	assert.Equal(t, []string{"en-US", "en"}, bundle.Languages)

	for _, raw := range []string{
		`{"generation":2,"timezone":"../../etc/passwd","locale":"en-US","languages":["en-US"]}`,
		`{"generation":2,"timezone":"Mars/Olympus_Mons","locale":"en-US","languages":["en-US"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-us","languages":["en-us"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-QQ","languages":["en-QQ"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-US","languages":["de-DE"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-US","languages":["en-US","EN"]}`,
		`{"generation":0,"timezone":"America/New_York","locale":"en-US","languages":["en-US"]}`,
	} {
		_, err := validateBrowserLocationBundle(raw)
		assert.Error(t, err, raw)
	}
}

func TestBrowserLocationBundlesEqual(t *testing.T) {
	a := browserLocationBundle{Generation: 1, TimeZone: "UTC", Locale: "en-US", Languages: []string{"en-US", "en"}}
	b := a
	b.Languages = append([]string(nil), a.Languages...)
	assert.True(t, browserLocationBundlesEqual(a, b))
	b.Generation++
	assert.False(t, browserLocationBundlesEqual(a, b))
}

func newBrowserLocationStateService(t *testing.T) *ApiService {
	t.Helper()
	t.Setenv("KERNEL_BROWSER_LOCATION_STATE_PATH", filepath.Join(t.TempDir(), "state.json"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &ApiService{lifecycleCtx: ctx}
	service.browserLocationReconcile = func(context.Context, browserLocationBundle) {}
	return service
}

func testLocationBundle(generation uint64) browserLocationBundle {
	return browserLocationBundle{Generation: generation, TimeZone: "UTC", Locale: "en-US", Languages: []string{"en-US", "en"}}
}

func TestBrowserLocationGenerationOrdering(t *testing.T) {
	service := newBrowserLocationStateService(t)
	g1 := testLocationBundle(1)
	require.NoError(t, service.acceptBrowserLocation(g1))
	g2 := testLocationBundle(2)
	require.NoError(t, service.acceptBrowserLocation(g2))

	assert.ErrorIs(t, service.acceptBrowserLocation(g1), errStaleBrowserLocation)
	require.NoError(t, service.acceptBrowserLocation(g2), "the accepted bundle can be resent to retry convergence")
	conflict := g2
	conflict.Locale = "de-DE"
	conflict.Languages = []string{"de-DE", "de"}
	assert.ErrorIs(t, service.acceptBrowserLocation(conflict), errConflictBrowserLocation)
	assert.Equal(t, g2, *service.browserLocationSnapshot().Accepted)
}

func TestBrowserLocationStateSurvivesAPIRestart(t *testing.T) {
	service := newBrowserLocationStateService(t)
	bundle := testLocationBundle(1)
	require.NoError(t, service.acceptBrowserLocation(bundle))

	restored := &ApiService{lifecycleCtx: service.lifecycleCtx}
	restored.browserLocationReconcile = func(context.Context, browserLocationBundle) {}
	require.NoError(t, restored.initializeBrowserLocation())
	status := restored.browserLocationSnapshot()
	require.NotNil(t, status.Accepted)
	assert.Equal(t, bundle, *status.Accepted)
}

func TestBrowserLocationReconcilesAfterChromiumRestart(t *testing.T) {
	service := newBrowserLocationStateService(t)
	logPath := filepath.Join(t.TempDir(), "chromium.log")
	require.NoError(t, os.WriteFile(logPath, nil, 0o600))
	manager := devtoolsproxy.NewUpstreamManager(logPath, slog.New(slog.DiscardHandler))
	manager.Start(service.lifecycleCtx)
	service.upstreamMgr = manager

	reconciled := make(chan browserLocationBundle, 4)
	service.browserLocationReconcile = func(_ context.Context, bundle browserLocationBundle) { reconciled <- bundle }
	bundle := testLocationBundle(1)
	require.NoError(t, service.acceptBrowserLocation(bundle))
	<-reconciled
	go service.browserLocationLifecycleLoop()

	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	defer file.Close()
	_, err = file.WriteString("DevTools listening on ws://127.0.0.1:9222/devtools/browser/a\n")
	require.NoError(t, err)
	select {
	case got := <-reconciled:
		assert.Equal(t, bundle, got)
	case <-time.After(3 * time.Second):
		t.Fatal("location was not reconciled after Chromium became ready")
	}
	service.browserLocationMu.Lock()
	applied := bundle
	service.browserLocation.applied = &applied
	service.browserLocation.components = browserLocationComponents{TimeZone: true, Browser: true, Renderers: true, NetworkContexts: true}
	service.browserLocationMu.Unlock()
	_, err = file.WriteString("DevTools listening on ws://127.0.0.1:9222/devtools/browser/b\n")
	require.NoError(t, err)
	select {
	case got := <-reconciled:
		assert.Equal(t, bundle, got)
	case <-time.After(3 * time.Second):
		t.Fatal("location was not reconciled after Chromium restart")
	}
	status := service.browserLocationSnapshot()
	assert.Nil(t, status.Applied, "a restarted Chromium must acknowledge the bundle again")
	assert.Equal(t, browserLocationComponents{}, status.Components)
}

func TestBrowserLocationPersistenceFailureAcceptsNothing(t *testing.T) {
	service := newBrowserLocationStateService(t)
	t.Setenv("KERNEL_BROWSER_LOCATION_STATE_PATH", "/proc/kernel-browser-location-state")
	err := service.acceptBrowserLocation(testLocationBundle(1))
	require.Error(t, err)
	assert.Nil(t, service.browserLocationSnapshot().Accepted)
}

func TestChromiumConfigureLocationPersistenceFailureReportsValidStep(t *testing.T) {
	service := newBrowserLocationStateService(t)
	require.NoError(t, service.acceptBrowserLocation(testLocationBundle(1)))
	t.Setenv("KERNEL_BROWSER_LOCATION_STATE_PATH", "/proc/kernel-browser-location-state")
	bundle := testLocationBundle(2)

	resp := service.applyBrowserLocationConfig(context.Background(), &chromiumConfigureState{browserLocation: &bundle})

	failure, ok := resp.(oapi.ChromiumConfigure500JSONResponse)
	require.True(t, ok, "expected 500 response, got %T", resp)
	require.NotNil(t, failure.Step)
	assert.Equal(t, oapi.BrowserLocation, *failure.Step)
	assert.True(t, failure.Step.Valid())
}

// The API retries after a 409, so stale and conflicting bundles must report it.
func TestChromiumConfigureLocationRejectsStaleGeneration(t *testing.T) {
	service := newBrowserLocationStateService(t)
	require.NoError(t, service.acceptBrowserLocation(testLocationBundle(2)))
	stale := testLocationBundle(1)

	resp := service.applyBrowserLocationConfig(context.Background(), &chromiumConfigureState{browserLocation: &stale})

	require.IsType(t, oapi.ChromiumConfigure409JSONResponse{}, resp)
}

func TestGetBrowserLocationHTTPConfirmsAppliedWithChromium(t *testing.T) {
	service := newBrowserLocationStateService(t)
	bundle := testLocationBundle(1)
	require.NoError(t, service.acceptBrowserLocation(bundle))
	service.browserLocationMu.Lock()
	applied := bundle
	service.browserLocation.applied = &applied
	service.browserLocation.components = browserLocationComponents{TimeZone: true, Browser: true, Renderers: true, NetworkContexts: true}
	service.browserLocationMu.Unlock()

	// Chromium reports this generation until it restarts and reports none.
	var chromiumGeneration atomic.Uint64
	chromiumGeneration.Store(bundle.Generation)
	chromium := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			var command struct {
				ID int `json:"id"`
			}
			if wsjson.Read(r.Context(), conn, &command) != nil {
				return
			}
			_ = wsjson.Write(r.Context(), conn, map[string]any{"id": command.ID, "result": map[string]any{
				"generation": chromiumGeneration.Load(), "locale": "en-US", "acceptLanguages": "en-US,en", "timezone": "UTC",
				"renderersConverged": true, "timezoneConverged": true, "networkContextsConverged": true,
			}})
		}
	}))
	defer chromium.Close()

	logPath := filepath.Join(t.TempDir(), "chromium.log")
	wsURL := "ws" + strings.TrimPrefix(chromium.URL, "http") + "/devtools/browser/a"
	require.NoError(t, os.WriteFile(logPath, []byte("DevTools listening on "+wsURL+"\n"), 0o600))
	manager := devtoolsproxy.NewUpstreamManager(logPath, slog.New(slog.DiscardHandler))
	manager.Start(service.lifecycleCtx)
	service.upstreamMgr = manager
	require.Eventually(t, func() bool { return manager.Current() == wsURL }, 3*time.Second, 10*time.Millisecond)

	status := func() browserLocationStatus {
		response := httptest.NewRecorder()
		service.GetBrowserLocationHTTP(response, httptest.NewRequest(http.MethodGet, "/browser/location", nil))
		var got browserLocationStatus
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
		return got
	}
	got := status()
	require.NotNil(t, got.Applied)
	assert.True(t, got.Components.Renderers)

	chromiumGeneration.Store(0)
	got = status()
	assert.Nil(t, got.Applied, "a restarted Chromium must not be reported as applied")
	assert.Equal(t, browserLocationComponents{}, got.Components)
}
