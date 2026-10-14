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
	"sync"
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
		require.NoError(t, validateBrowserLocationBundle(decodeLocationBundle(t, raw)), locale)
	}
	require.NoError(t, validateBrowserLocationBundle(decodeLocationBundle(t, `{"generation":2,"timezone":"America/New_York","locale":"en-US","languages":["en-US","en"]}`)))

	for _, raw := range []string{
		`{"generation":2,"timezone":"../../etc/passwd","locale":"en-US","languages":["en-US"]}`,
		`{"generation":2,"timezone":"Mars/Olympus_Mons","locale":"en-US","languages":["en-US"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-us","languages":["en-us"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-QQ","languages":["en-QQ"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-US","languages":["de-DE"]}`,
		`{"generation":2,"timezone":"America/New_York","locale":"en-US","languages":["en-US","EN"]}`,
		`{"generation":0,"timezone":"America/New_York","locale":"en-US","languages":["en-US"]}`,
	} {
		assert.Error(t, validateBrowserLocationBundle(decodeLocationBundle(t, raw)), raw)
	}
}

func decodeLocationBundle(t *testing.T, raw string) oapi.BrowserLocationBundle {
	t.Helper()
	var bundle oapi.BrowserLocationBundle
	require.NoError(t, json.Unmarshal([]byte(raw), &bundle))
	return bundle
}

func TestBrowserLocationBundlesEqual(t *testing.T) {
	a := oapi.BrowserLocationBundle{Generation: 1, Timezone: "UTC", Locale: "en-US", Languages: []string{"en-US", "en"}}
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
	service.browserLocationReconcile = func(context.Context, oapi.BrowserLocationBundle) {}
	return service
}

func testLocationBundle(generation int64) oapi.BrowserLocationBundle {
	return oapi.BrowserLocationBundle{Generation: generation, Timezone: "UTC", Locale: "en-US", Languages: []string{"en-US", "en"}}
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
	restored.browserLocationReconcile = func(context.Context, oapi.BrowserLocationBundle) {}
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

	reconciled := make(chan oapi.BrowserLocationBundle, 4)
	service.browserLocationReconcile = func(_ context.Context, bundle oapi.BrowserLocationBundle) { reconciled <- bundle }
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
	service.browserLocation.components = oapi.BrowserLocationComponents{Timezone: true, Browser: true, Renderers: true, NetworkContexts: true}
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
	assert.Equal(t, oapi.BrowserLocationComponents{}, status.Components)
}

func TestBrowserLocationPersistenceFailureAcceptsNothing(t *testing.T) {
	service := newBrowserLocationStateService(t)
	t.Setenv("KERNEL_BROWSER_LOCATION_STATE_PATH", "/proc/kernel-browser-location-state")
	err := service.acceptBrowserLocation(testLocationBundle(1))
	require.Error(t, err)
	assert.Nil(t, service.browserLocationSnapshot().Accepted)
}

func TestSetBrowserLocationPersistenceFailure(t *testing.T) {
	service := newBrowserLocationStateService(t)
	require.NoError(t, service.acceptBrowserLocation(testLocationBundle(1)))
	t.Setenv("KERNEL_BROWSER_LOCATION_STATE_PATH", "/proc/kernel-browser-location-state")
	bundle := testLocationBundle(2)

	resp := service.applyBrowserLocation(context.Background(), bundle)

	require.IsType(t, oapi.SetBrowserLocation500JSONResponse{}, resp)
}

// The API retries after a 409, so stale and conflicting bundles must report it.
func TestSetBrowserLocationRejectsStaleGeneration(t *testing.T) {
	service := newBrowserLocationStateService(t)
	require.NoError(t, service.acceptBrowserLocation(testLocationBundle(2)))
	stale := testLocationBundle(1)

	resp := service.applyBrowserLocation(context.Background(), stale)

	require.IsType(t, oapi.SetBrowserLocation409JSONResponse{}, resp)
}

// The request returns only once the running browser confirmed the location.
func TestSetBrowserLocationWaitsForApply(t *testing.T) {
	service := newBrowserLocationStateService(t)
	service.browserLocationReconcile = func(_ context.Context, bundle oapi.BrowserLocationBundle) {
		time.Sleep(150 * time.Millisecond)
		service.browserLocationMu.Lock()
		defer service.browserLocationMu.Unlock()
		applied := bundle
		service.browserLocation.applied = &applied
	}
	bundle := testLocationBundle(1)

	started := time.Now()
	resp := service.applyBrowserLocation(context.Background(), bundle)

	require.IsType(t, oapi.SetBrowserLocation200JSONResponse{}, resp)
	assert.GreaterOrEqual(t, time.Since(started), 150*time.Millisecond)
	assert.Equal(t, &bundle, service.browserLocationSnapshot().Applied)
}

// A location the browser doesn't confirm fails the request and stays
// accepted, so a resend retries it.
func TestSetBrowserLocationReportsApplyFailure(t *testing.T) {
	service := newBrowserLocationStateService(t)
	service.browserLocationReconcile = func(context.Context, oapi.BrowserLocationBundle) {
		service.browserLocationMu.Lock()
		defer service.browserLocationMu.Unlock()
		service.browserLocation.lastError = "browser location components have not converged"
	}
	bundle := testLocationBundle(1)

	resp := service.applyBrowserLocation(context.Background(), bundle)

	require.IsType(t, oapi.SetBrowserLocation500JSONResponse{}, resp)
	assert.Equal(t, &bundle, service.browserLocationSnapshot().Accepted)
}

// A browser found unsupported only after the location was accepted keeps it
// accepted, so the request fails with 500 rather than 501.
func TestSetBrowserLocationReportsLateUnsupportedBrowser(t *testing.T) {
	service := newBrowserLocationStateService(t)
	service.browserLocationReconcile = func(context.Context, oapi.BrowserLocationBundle) {
		service.browserLocationMu.Lock()
		defer service.browserLocationMu.Unlock()
		service.browserLocation.lastError = errBrowserLocationUnsupported.Error()
	}
	bundle := testLocationBundle(1)

	resp := service.applyBrowserLocation(context.Background(), bundle)

	require.IsType(t, oapi.SetBrowserLocation500JSONResponse{}, resp)
	assert.Equal(t, &bundle, service.browserLocationSnapshot().Accepted)
}

// Resending an applied location waits for the browser to confirm it again
// rather than trusting the earlier confirmation.
func TestSetBrowserLocationResendConfirmsAgain(t *testing.T) {
	service := newBrowserLocationStateService(t)
	service.browserLocationReconcile = func(_ context.Context, bundle oapi.BrowserLocationBundle) {
		service.browserLocationMu.Lock()
		defer service.browserLocationMu.Unlock()
		applied := bundle
		service.browserLocation.applied = &applied
	}
	bundle := testLocationBundle(1)
	require.IsType(t, oapi.SetBrowserLocation200JSONResponse{}, service.applyBrowserLocation(context.Background(), bundle))

	// The browser lost the location and the resend can't apply it.
	service.browserLocationReconcile = func(context.Context, oapi.BrowserLocationBundle) {
		service.browserLocationMu.Lock()
		defer service.browserLocationMu.Unlock()
		service.browserLocation.lastError = "browser location components have not converged"
	}
	resp := service.applyBrowserLocation(context.Background(), bundle)

	require.IsType(t, oapi.SetBrowserLocation500JSONResponse{}, resp)
	assert.Nil(t, service.browserLocationSnapshot().Applied)
}

// A newer location accepted while the request waits replaces this one, which
// the caller must learn about as a conflict.
func TestSetBrowserLocationReportsReplacedLocation(t *testing.T) {
	service := newBrowserLocationStateService(t)
	newer := testLocationBundle(2)
	service.browserLocationReconcile = func(_ context.Context, bundle oapi.BrowserLocationBundle) {
		if bundle.Generation == 1 {
			go func() { _ = service.acceptBrowserLocation(newer) }()
		}
	}
	bundle := testLocationBundle(1)

	resp := service.applyBrowserLocation(context.Background(), bundle)

	require.IsType(t, oapi.SetBrowserLocation409JSONResponse{}, resp)
}

func TestGetBrowserLocationConfirmsAppliedWithChromium(t *testing.T) {
	service := newBrowserLocationStateService(t)
	bundle := testLocationBundle(1)
	require.NoError(t, service.acceptBrowserLocation(bundle))
	service.browserLocationMu.Lock()
	applied := bundle
	service.browserLocation.applied = &applied
	service.browserLocation.components = oapi.BrowserLocationComponents{Timezone: true, Browser: true, Renderers: true, NetworkContexts: true}
	service.browserLocationMu.Unlock()

	// Chromium reports this generation until it restarts and reports none.
	var chromiumGeneration atomic.Uint64
	chromiumGeneration.Store(uint64(bundle.Generation))
	fakeChromium(t, service, func(method string, _ map[string]any) map[string]any {
		return map[string]any{"result": map[string]any{
			"generation": chromiumGeneration.Load(), "locale": "en-US", "acceptLanguages": "en-US,en", "timezone": "UTC",
			"renderersConverged": true, "timezoneConverged": true, "networkContextsConverged": true,
		}}
	})

	got := getBrowserLocationStatus(t, service)
	require.NotNil(t, got.Applied)
	assert.True(t, got.Components.Renderers)

	chromiumGeneration.Store(0)
	got = getBrowserLocationStatus(t, service)
	assert.Nil(t, got.Applied, "a restarted Chromium must not be reported as applied")
	assert.Equal(t, oapi.BrowserLocationComponents{}, got.Components)
}

// fakeChromium serves CDP at a DevTools URL the service's upstream manager
// reports. respond returns the "result" or "error" member for each command.
func fakeChromium(t *testing.T, service *ApiService, respond func(method string, params map[string]any) map[string]any) {
	t.Helper()
	chromium := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			var command struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if wsjson.Read(r.Context(), conn, &command) != nil {
				return
			}
			response := respond(command.Method, command.Params)
			response["id"] = command.ID
			_ = wsjson.Write(r.Context(), conn, response)
		}
	}))
	t.Cleanup(chromium.Close)

	logPath := filepath.Join(t.TempDir(), "chromium.log")
	wsURL := "ws" + strings.TrimPrefix(chromium.URL, "http") + "/devtools/browser/a"
	require.NoError(t, os.WriteFile(logPath, []byte("DevTools listening on "+wsURL+"\n"), 0o600))
	manager := devtoolsproxy.NewUpstreamManager(logPath, slog.New(slog.DiscardHandler))
	manager.Start(service.lifecycleCtx)
	service.upstreamMgr = manager
	require.Eventually(t, func() bool { return manager.Current() == wsURL }, 3*time.Second, 10*time.Millisecond)
}

func getBrowserLocationStatus(t *testing.T, service *ApiService) oapi.BrowserLocationStatus {
	t.Helper()
	resp, err := service.GetBrowserLocation(context.Background(), oapi.GetBrowserLocationRequestObject{})
	require.NoError(t, err)
	status, ok := resp.(oapi.GetBrowserLocation200JSONResponse)
	require.True(t, ok, "expected 200, got %T", resp)
	return oapi.BrowserLocationStatus(status)
}

func snapshotError(service *ApiService) string {
	if message := service.browserLocationSnapshot().Error; message != nil {
		return *message
	}
	return ""
}

// stubLocaltime keeps reconciles from replacing the host's /etc/localtime.
func stubLocaltime(t *testing.T) {
	t.Helper()
	previous := setBrowserLocaltime
	setBrowserLocaltime = func(string) error { return nil }
	t.Cleanup(func() { setBrowserLocaltime = previous })
}

// A Chromium without the location commands must be reported as unsupported,
// refuse the location without accepting anything and never be retried.
func TestBrowserLocationReportsChromiumWithoutLocationCommands(t *testing.T) {
	stubLocaltime(t)
	service := newBrowserLocationStateService(t)
	var commands atomic.Int32
	fakeChromium(t, service, func(method string, _ map[string]any) map[string]any {
		commands.Add(1)
		return map[string]any{"error": map[string]any{"code": -32601, "message": "'" + method + "' wasn't found"}}
	})

	resp501, err := service.GetBrowserLocation(context.Background(), oapi.GetBrowserLocationRequestObject{})
	require.NoError(t, err)
	assert.IsType(t, oapi.GetBrowserLocation501JSONResponse{}, resp501)

	bundle := testLocationBundle(1)
	resp := service.applyBrowserLocation(context.Background(), bundle)
	require.IsType(t, oapi.SetBrowserLocation501JSONResponse{}, resp)
	assert.Nil(t, service.browserLocationSnapshot().Accepted)
	assert.Equal(t, int32(1), commands.Load(), "each Chromium process is probed once")

	// A bundle accepted before the probe, for example from the persisted
	// state, fails once without retrying.
	service.browserLocation.probedUpstream = ""
	service.browserLocationReconcile = nil
	require.NoError(t, service.acceptBrowserLocation(bundle))
	require.Eventually(t, func() bool { return snapshotError(service) != "" }, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, errBrowserLocationUnsupported.Error(), snapshotError(service))
	attempts := commands.Load()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, attempts, commands.Load(), "an unsupported browser must not be retried")
}

// Resending the accepted bundle after a failure retries it, and the failure
// must not be reported while the retry runs.
func TestBrowserLocationRetryClearsPreviousError(t *testing.T) {
	service := newBrowserLocationStateService(t)
	bundle := testLocationBundle(1)
	require.NoError(t, service.acceptBrowserLocation(bundle))
	service.browserLocationMu.Lock()
	service.browserLocation.lastError = "browser location components have not converged"
	service.browserLocationMu.Unlock()

	require.NoError(t, service.acceptBrowserLocation(bundle))
	assert.Empty(t, snapshotError(service))
}

// Chromium can hold a generation the image never sent, for example after a
// client reached it directly. The image must move above it rather than retry
// a stale generation until it times out.
func TestBrowserLocationMovesAboveChromiumGeneration(t *testing.T) {
	stubLocaltime(t)
	service := newBrowserLocationStateService(t)
	var mu sync.Mutex
	held := map[string]any{"generation": float64(7), "locale": "fr-FR", "acceptLanguages": "fr-FR,fr", "timezone": "UTC"}
	fakeChromium(t, service, func(method string, params map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "Browser.setKernelBrowserLocation":
			if params["generation"].(float64) <= held["generation"].(float64) {
				return map[string]any{"error": map[string]any{"code": -32602, "message": "generation is stale"}}
			}
			held = map[string]any{"generation": params["generation"], "locale": params["locale"], "acceptLanguages": params["acceptLanguages"], "timezone": params["timezone"]}
			return map[string]any{"result": map[string]any{}}
		default:
			result := map[string]any{"renderersConverged": true, "timezoneConverged": true, "networkContextsConverged": true}
			for key, value := range held {
				result[key] = value
			}
			return map[string]any{"result": result}
		}
	})
	service.browserLocationReconcile = nil

	bundle := testLocationBundle(2)
	require.NoError(t, service.acceptBrowserLocation(bundle))
	require.Eventually(t, func() bool { return getBrowserLocationStatus(t, service).Applied != nil }, 3*time.Second, 10*time.Millisecond)
	mu.Lock()
	assert.Equal(t, float64(8), held["generation"])
	mu.Unlock()
	assert.Nil(t, getBrowserLocationStatus(t, service).Error)
}

// fakeLocationChromium holds the last location set on it and reports it with
// the renderer acknowledgements in renderers.
func fakeLocationChromium(t *testing.T, service *ApiService, renderers map[string]any) *atomic.Int32 {
	t.Helper()
	var mu sync.Mutex
	var sets atomic.Int32
	held := map[string]any{"generation": float64(0), "locale": "", "acceptLanguages": "", "timezone": "UTC"}
	fakeChromium(t, service, func(method string, params map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if method == "Browser.setKernelBrowserLocation" {
			sets.Add(1)
			held = map[string]any{"generation": params["generation"], "locale": params["locale"], "acceptLanguages": params["acceptLanguages"], "timezone": params["timezone"]}
			return map[string]any{"result": map[string]any{}}
		}
		result := map[string]any{"timezoneConverged": true, "networkContextsConverged": true}
		for key, value := range held {
			result[key] = value
		}
		for key, value := range renderers {
			result[key] = value
		}
		return map[string]any{"result": result}
	})
	service.browserLocationReconcile = nil
	return &sets
}

// A renderer blocked by a dialog or a long task has the location queued and
// applies it when it resumes, so it doesn't fail the request.
func TestSetBrowserLocationAllowsPendingRenderers(t *testing.T) {
	stubLocaltime(t)
	service := newBrowserLocationStateService(t)
	fakeLocationChromium(t, service, map[string]any{"renderersConverged": false, "renderersPending": 1})
	bundle := testLocationBundle(1)

	started := time.Now()
	resp := service.applyBrowserLocation(context.Background(), bundle)

	require.IsType(t, oapi.SetBrowserLocation200JSONResponse{}, resp)
	assert.GreaterOrEqual(t, time.Since(started), browserLocationPendingRendererWait, "renderers get a chance to acknowledge first")
	status := getBrowserLocationStatus(t, service)
	require.NotNil(t, status.Applied)
	assert.False(t, status.Components.Renderers)
	assert.Equal(t, 1, status.Components.RenderersPending)
}

func TestBrowserLocationConfirmed(t *testing.T) {
	converged := oapi.BrowserLocationComponents{Browser: true, Timezone: true, Renderers: true, NetworkContexts: true}
	pending := converged
	pending.Renderers = false
	pending.RenderersPending = 2
	unacknowledged := pending
	unacknowledged.RenderersPending = 0
	network := converged
	network.NetworkContexts = false

	assert.True(t, browserLocationConfirmed(converged, false))
	assert.False(t, browserLocationConfirmed(pending, false))
	assert.True(t, browserLocationConfirmed(pending, true))
	assert.False(t, browserLocationConfirmed(unacknowledged, true), "a Chromium that reports no pending count has not confirmed")
	assert.False(t, browserLocationConfirmed(network, true))
}

// A restart the image performs applies the accepted location to the new
// Chromium before it completes, rather than trusting the previous Chromium's
// confirmation.
func TestReapplyBrowserLocationAfterRestart(t *testing.T) {
	stubLocaltime(t)
	service := newBrowserLocationStateService(t)
	sets := fakeLocationChromium(t, service, map[string]any{"renderersConverged": true, "renderersPending": 0})

	service.reapplyBrowserLocation(context.Background())
	assert.Zero(t, sets.Load(), "nothing to apply before a location is accepted")

	bundle := testLocationBundle(1)
	require.NoError(t, service.acceptBrowserLocation(bundle))
	service.browserLocationMu.Lock()
	service.browserLocation.cancel()
	previous := bundle
	service.browserLocation.applied = &previous
	service.browserLocationMu.Unlock()
	sent := sets.Load()

	service.reapplyBrowserLocation(context.Background())

	assert.Greater(t, sets.Load(), sent, "the new Chromium must be sent the location")
	assert.Equal(t, &bundle, service.browserLocationSnapshot().Applied)
}

func TestValidateBrowserLocationBundleCapsGeneration(t *testing.T) {
	if _, err := os.Stat("/usr/share/zoneinfo/Etc/UTC"); err != nil {
		t.Skip("zoneinfo unavailable")
	}
	require.NoError(t, validateBrowserLocationBundle(decodeLocationBundle(t, fmt.Sprintf(`{"generation":%d,"timezone":"Etc/UTC","locale":"en-US","languages":["en-US"]}`, uint64(maxBrowserLocationGeneration)))))
	assert.Error(t, validateBrowserLocationBundle(decodeLocationBundle(t, fmt.Sprintf(`{"generation":%d,"timezone":"Etc/UTC","locale":"en-US","languages":["en-US"]}`, uint64(maxBrowserLocationGeneration)+1))))
}
