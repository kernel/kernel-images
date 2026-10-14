package api

import (
	"context"
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
		bundle, err := validateBrowserLocationBundle(raw)
		require.NoError(t, err, locale)
		assert.Equal(t, locale, bundle.Locale)
	}
	bundle, err := validateBrowserLocationBundle(`{"generation":2,"timezone":"America/New_York","locale":"en-US","languages":["en-US","en"]}`)
	require.NoError(t, err)
	assert.Equal(t, int64(2), bundle.Generation)
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

// Configure returns only once the running browser confirmed the location, so
// start_url navigation and the 200 come after it.
func TestChromiumConfigureLocationWaitsForApply(t *testing.T) {
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
	resp := service.applyBrowserLocationConfig(context.Background(), &chromiumConfigureState{browserLocation: &bundle})

	require.Nil(t, resp)
	assert.GreaterOrEqual(t, time.Since(started), 150*time.Millisecond)
	assert.Equal(t, &bundle, service.browserLocationSnapshot().Applied)
}

// A location the browser doesn't confirm fails configure with the location
// step, and stays accepted so a resend retries it.
func TestChromiumConfigureLocationReportsApplyFailure(t *testing.T) {
	service := newBrowserLocationStateService(t)
	service.browserLocationReconcile = func(context.Context, oapi.BrowserLocationBundle) {
		service.browserLocationMu.Lock()
		defer service.browserLocationMu.Unlock()
		service.browserLocation.lastError = "browser location components have not converged"
	}
	bundle := testLocationBundle(1)

	resp := service.applyBrowserLocationConfig(context.Background(), &chromiumConfigureState{browserLocation: &bundle})

	failure, ok := resp.(oapi.ChromiumConfigure500JSONResponse)
	require.True(t, ok, "expected 500, got %T", resp)
	require.NotNil(t, failure.Step)
	assert.Equal(t, oapi.BrowserLocation, *failure.Step)
	assert.Equal(t, &bundle, service.browserLocationSnapshot().Accepted)
}

// A browser found unsupported only after the location was accepted keeps it
// accepted, so configure reports the location step rather than a 400.
func TestChromiumConfigureLocationReportsLateUnsupportedBrowser(t *testing.T) {
	service := newBrowserLocationStateService(t)
	service.browserLocationReconcile = func(context.Context, oapi.BrowserLocationBundle) {
		service.browserLocationMu.Lock()
		defer service.browserLocationMu.Unlock()
		service.browserLocation.lastError = errBrowserLocationUnsupported.Error()
	}
	bundle := testLocationBundle(1)

	resp := service.applyBrowserLocationConfig(context.Background(), &chromiumConfigureState{browserLocation: &bundle})

	failure, ok := resp.(oapi.ChromiumConfigure500JSONResponse)
	require.True(t, ok, "expected 500, got %T", resp)
	require.NotNil(t, failure.Step)
	assert.Equal(t, oapi.BrowserLocation, *failure.Step)
	assert.Equal(t, &bundle, service.browserLocationSnapshot().Accepted)
}

// A newer location accepted while configure waits replaces this one, which
// the caller must learn about as a conflict.
func TestChromiumConfigureLocationReportsReplacedLocation(t *testing.T) {
	service := newBrowserLocationStateService(t)
	newer := testLocationBundle(2)
	service.browserLocationReconcile = func(_ context.Context, bundle oapi.BrowserLocationBundle) {
		if bundle.Generation == 1 {
			go func() { _ = service.acceptBrowserLocation(newer) }()
		}
	}
	bundle := testLocationBundle(1)

	resp := service.applyBrowserLocationConfig(context.Background(), &chromiumConfigureState{browserLocation: &bundle})

	require.IsType(t, oapi.ChromiumConfigure409JSONResponse{}, resp)
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
// refuse configure without accepting anything and never be retried.
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
	resp := service.applyBrowserLocationConfig(context.Background(), &chromiumConfigureState{browserLocation: &bundle})
	require.IsType(t, oapi.ChromiumConfigure400JSONResponse{}, resp)
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

func TestValidateBrowserLocationBundleCapsGeneration(t *testing.T) {
	if _, err := os.Stat("/usr/share/zoneinfo/Etc/UTC"); err != nil {
		t.Skip("zoneinfo unavailable")
	}
	_, err := validateBrowserLocationBundle(fmt.Sprintf(`{"generation":%d,"timezone":"Etc/UTC","locale":"en-US","languages":["en-US"]}`, uint64(maxBrowserLocationGeneration)))
	require.NoError(t, err)
	_, err = validateBrowserLocationBundle(fmt.Sprintf(`{"generation":%d,"timezone":"Etc/UTC","locale":"en-US","languages":["en-US"]}`, uint64(maxBrowserLocationGeneration)+1))
	assert.Error(t, err)
}
