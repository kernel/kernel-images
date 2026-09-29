package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/devtoolsproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBrowserLocationBundle(t *testing.T) {
	zone := filepath.Join("/usr/share/zoneinfo", "America", "New_York")
	if _, err := os.Stat(zone); err != nil {
		t.Skip("zoneinfo unavailable")
	}

	bundle, err := validateBrowserLocationBundle(`{"epoch":"lease-1","generation":2,"timezone":"America/New_York","locale":"en-US","languages":["en-US","en"]}`)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), bundle.Generation)
	assert.Equal(t, []string{"en-US", "en"}, bundle.Languages)

	for _, raw := range []string{
		`{"epoch":"lease-1","generation":2,"timezone":"../../etc/passwd","locale":"en-US","languages":["en-US"]}`,
		`{"epoch":"lease-1","generation":2,"timezone":"America/New_York","locale":"en-us","languages":["en-us"]}`,
		`{"epoch":"lease-1","generation":2,"timezone":"America/New_York","locale":"en-US","languages":["de-DE"]}`,
	} {
		_, err := validateBrowserLocationBundle(raw)
		assert.Error(t, err)
	}
}

func TestBrowserLocationBundlesEqual(t *testing.T) {
	a := browserLocationBundle{Epoch: "lease", Generation: 1, TimeZone: "UTC", Locale: "en-US", Languages: []string{"en-US", "en"}}
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

func testLocationBundle(epoch string, generation uint64) browserLocationBundle {
	return browserLocationBundle{Epoch: epoch, Generation: generation, TimeZone: "UTC", Locale: "en-US", Languages: []string{"en-US", "en"}}
}

func TestBrowserLocationEpochOrdering(t *testing.T) {
	service := newBrowserLocationStateService(t)
	a1 := testLocationBundle("lease-a", 1)
	require.NoError(t, service.resetBrowserLocation("", a1))

	a2 := testLocationBundle("lease-a", 2)
	require.NoError(t, service.acceptBrowserLocation(a2))
	assert.ErrorIs(t, service.acceptBrowserLocation(a1), errStaleBrowserLocation)

	b1 := testLocationBundle("lease-b", 1)
	require.NoError(t, service.resetBrowserLocation("lease-a", b1))
	assert.ErrorIs(t, service.acceptBrowserLocation(a2), errBrowserLocationEpoch)
	assert.ErrorIs(t, service.resetBrowserLocation("", a1), errBrowserLocationEpoch)
	assert.Equal(t, b1, *service.browserLocationSnapshot().Accepted)

	require.NoError(t, service.acceptBrowserLocation(b1))
	conflict := b1
	conflict.Locale = "de-DE"
	conflict.Languages = []string{"de-DE", "de"}
	assert.ErrorIs(t, service.acceptBrowserLocation(conflict), errConflictBrowserLocation)
}

func TestBrowserLocationStateSurvivesAPIRestart(t *testing.T) {
	service := newBrowserLocationStateService(t)
	bundle := testLocationBundle("lease-a", 1)
	require.NoError(t, service.resetBrowserLocation("", bundle))

	restored := &ApiService{lifecycleCtx: service.lifecycleCtx}
	restored.browserLocationReconcile = func(context.Context, browserLocationBundle) {}
	require.NoError(t, restored.initializeBrowserLocation())
	status := restored.browserLocationSnapshot()
	assert.Equal(t, "lease-a", status.ActiveEpoch)
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
	bundle := testLocationBundle("lease-a", 1)
	require.NoError(t, service.resetBrowserLocation("", bundle))
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
	_, err = file.WriteString("DevTools listening on ws://127.0.0.1:9222/devtools/browser/b\n")
	require.NoError(t, err)
	select {
	case got := <-reconciled:
		assert.Equal(t, bundle, got)
	case <-time.After(3 * time.Second):
		t.Fatal("location was not reconciled after Chromium restart")
	}
}

func TestResetBrowserLocationHTTPRequiresInstanceIdentity(t *testing.T) {
	service := newBrowserLocationStateService(t)
	service.browserLocationValidate = func(context.Context, browserLocationBundle) error { return nil }
	t.Setenv("KERNEL_INSTANCE_JWT", "instance-token")
	body := `{"previous_epoch":"","bundle":{"epoch":"lease-a","generation":1,"timezone":"UTC","locale":"en-US","languages":["en-US","en"]}}`

	unauthorized := httptest.NewRecorder()
	service.ResetBrowserLocationHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/internal/browser-location/reset", strings.NewReader(body)))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	assert.Empty(t, service.browserLocationSnapshot().ActiveEpoch)

	request := httptest.NewRequest(http.MethodPost, "/internal/browser-location/reset", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer instance-token")
	accepted := httptest.NewRecorder()
	service.ResetBrowserLocationHTTP(accepted, request)
	assert.Equal(t, http.StatusAccepted, accepted.Code)
	assert.Equal(t, "lease-a", service.browserLocationSnapshot().ActiveEpoch)
}

func TestBrowserLocationPersistenceFailureDoesNotChangeEpoch(t *testing.T) {
	service := newBrowserLocationStateService(t)
	t.Setenv("KERNEL_BROWSER_LOCATION_STATE_PATH", "/proc/kernel-browser-location-state")
	err := service.resetBrowserLocation("", testLocationBundle("lease-a", 1))
	require.Error(t, err)
	assert.Empty(t, service.browserLocationSnapshot().ActiveEpoch)
}
