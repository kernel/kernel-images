package api

import (
	"context"
	"errors"
	"testing"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allCategoriesDisabled returns a config with every configurable category set
// to enabled:false (the clear signal).
func allCategoriesDisabled() *oapi.BrowserTelemetryCategoriesConfig {
	off := func() *oapi.BrowserTelemetryCategoryConfig {
		f := false
		return &oapi.BrowserTelemetryCategoryConfig{Enabled: &f}
	}
	return &oapi.BrowserTelemetryCategoriesConfig{
		Console:     off(),
		Network:     off(),
		Page:        off(),
		Interaction: off(),
		Control:     off(),
		Connection:  off(),
		System:      off(),
		Screenshot:  off(),
		Captcha:     off(),
	}
}

func TestTelemetryConfigFromOAPI(t *testing.T) {
	t.Run("nil body returns the default set", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(nil)
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.ElementsMatch(t, events.DefaultCategories, cfg.Categories)
	})

	t.Run("nil browser key returns the default set", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{})
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.ElementsMatch(t, events.DefaultCategories, cfg.Categories)
	})

	t.Run("omitted enabled resolves to default state", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console: &oapi.BrowserTelemetryCategoryConfig{}, // Enabled nil → default state (on)
			},
		})
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.Contains(t, cfg.Categories, events.Console)
		// Screenshot is off by default and must stay off when unspecified.
		assert.NotContains(t, cfg.Categories, events.Screenshot)
	})

	t.Run("screenshot is opt-in", func(t *testing.T) {
		tr := true
		cfg, _, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Screenshot: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
			},
		})
		require.NoError(t, err)
		assert.Contains(t, cfg.Categories, events.Screenshot)
	})

	t.Run("all configurable categories false returns allDisabled=true", func(t *testing.T) {
		_, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: allCategoriesDisabled(),
		})
		require.NoError(t, err)
		assert.True(t, allDisabled)
	})

	t.Run("disabling only the default-on categories does not clear", func(t *testing.T) {
		f := false
		_, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
			},
		})
		require.NoError(t, err)
		// control/connection/system/captcha remain at their default-on state.
		assert.False(t, allDisabled)
	})

	t.Run("mixed enabled flags resolve unspecified to default", func(t *testing.T) {
		tr, f := true, false
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				Network: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
			},
		})
		require.NoError(t, err)
		assert.False(t, allDisabled)
		// network off; screenshot default off; the other 7 default-on categories remain.
		assert.Contains(t, cfg.Categories, events.Console)
		assert.NotContains(t, cfg.Categories, events.Network)
		assert.NotContains(t, cfg.Categories, events.Screenshot)
		assert.Len(t, cfg.Categories, len(events.DefaultCategories)-1)
	})
}

func TestPutTelemetry(t *testing.T) {
	ctx := context.Background()

	t.Run("creates session with no body (201)", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)
		r201, ok := resp.(oapi.PutTelemetry201JSONResponse)
		require.True(t, ok, "expected 201, got %T", resp)
		require.NotNil(t, r201.Config.Browser)
		require.NotNil(t, r201.AppliedAt)
		assert.False(t, r201.AppliedAt.IsZero())
	})

	t.Run("creates session with config (201)", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		tr := true
		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				},
			},
		})
		require.NoError(t, err)
		_, ok := resp.(oapi.PutTelemetry201JSONResponse)
		require.True(t, ok, "expected 201, got %T", resp)
	})

	t.Run("replaces config on active session (200)", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)

		tr := true
		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				},
			},
		})
		require.NoError(t, err)
		_, ok := resp.(oapi.PutTelemetry200JSONResponse)
		assert.True(t, ok, "expected 200 on replace, got %T", resp)
	})

	t.Run("all-false clears active configuration (200, all-disabled config)", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.PutTelemetry200JSONResponse)
		require.True(t, ok, "expected 200, got %T", resp)
		require.NotNil(t, r200.Config.Browser)
		require.NotNil(t, r200.Config.Browser.Console)
		assert.False(t, *r200.Config.Browser.Console.Enabled)
		assert.False(t, *r200.Config.Browser.Control.Enabled)
		assert.False(t, *r200.Config.Browser.System.Enabled)
		assert.Nil(t, r200.AppliedAt, "applied_at must be omitted when telemetry is unconfigured")
	})
}

func TestTelemetryHandlersDriveMiddlewareToggle(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(DisableTelemetryMiddleware)

	svc := newTestService(t, newMockRecordManager())

	DisableTelemetryMiddleware()
	tr, f := true, false
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Control: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
			},
		},
	})
	require.NoError(t, err)
	assert.True(t, TelemetryMiddlewareEnabled(), "PUT with control=true should enable middleware")

	_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Control: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
			},
		},
	})
	require.NoError(t, err)
	assert.False(t, TelemetryMiddlewareEnabled(), "PATCH control=false should disable middleware (other categories still active)")

	_, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()},
	})
	require.NoError(t, err)
	assert.False(t, TelemetryMiddlewareEnabled(), "all-disabled PUT should leave middleware off")
}

func TestGetTelemetry(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.GetTelemetry(ctx, oapi.GetTelemetryRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.GetTelemetry404JSONResponse{}, resp)
	})

	t.Run("active session returns 200", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		startResp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)
		started := startResp.(oapi.PutTelemetry201JSONResponse)

		resp, err := svc.GetTelemetry(ctx, oapi.GetTelemetryRequestObject{})
		require.NoError(t, err)
		r200, ok := resp.(oapi.GetTelemetry200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, started.Config, r200.Config)
	})
}

func TestPatchTelemetry(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.PatchTelemetry404JSONResponse{}, resp)
	})

	t.Run("update config", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)

		tr, f := true, false
		resp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
					Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				},
			},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.PatchTelemetry200JSONResponse)
		require.True(t, ok)
		require.NotNil(t, r200.Config.Browser)
		require.NotNil(t, r200.Config.Browser.Console)
		assert.True(t, *r200.Config.Browser.Console.Enabled)
	})

	t.Run("nil body is no-op", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		startResp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)
		started := startResp.(oapi.PutTelemetry201JSONResponse)

		resp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{})
		require.NoError(t, err)
		r200, ok := resp.(oapi.PatchTelemetry200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, started.Config, r200.Config)
	})

	t.Run("all-false clears configuration", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)

		resp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.PatchTelemetry200JSONResponse)
		require.True(t, ok, "expected 200, got %T", resp)
		require.NotNil(t, r200.Config.Browser)
		require.NotNil(t, r200.Config.Browser.Console)
		assert.False(t, *r200.Config.Browser.Console.Enabled)
		assert.False(t, *r200.Config.Browser.Control.Enabled)
		assert.False(t, *r200.Config.Browser.System.Enabled)
	})

	t.Run("put returns 201 after patch clears configuration", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)

		_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()},
		})
		require.NoError(t, err)

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)
		_, ok := resp.(oapi.PutTelemetry201JSONResponse)
		assert.True(t, ok, "expected 201 after clear, got %T", resp)
	})
}

// newMockRecordManager returns a minimal record manager for tests that don't
// exercise recording.
func newMockRecordManager() *mockRecordManager {
	return &mockRecordManager{}
}

type mockRecordManager struct{}

func (m *mockRecordManager) RegisterRecorder(_ context.Context, _ recorder.Recorder) error {
	return nil
}
func (m *mockRecordManager) DeregisterRecorder(_ context.Context, _ recorder.Recorder) error {
	return nil
}
func (m *mockRecordManager) GetRecorder(_ string) (recorder.Recorder, bool)            { return nil, false }
func (m *mockRecordManager) ListActiveRecorders(_ context.Context) []recorder.Recorder { return nil }
func (m *mockRecordManager) StopAll(_ context.Context) error                           { return nil }

// newTestService builds an ApiService with minimal dependencies for telemetry tests.
func newTestService(t *testing.T, mgr recorder.RecordManager) *ApiService {
	t.Helper()
	ts, es := newTelemetrySession(t)
	svc, err := New(mgr, newMockFactory(), newTestUpstreamManager(), scaletozero.NewNoopController(), newMockNekoClient(t), ts, es, 0)
	require.NoError(t, err)
	svc.cdpMonitor = &stubCdpMonitor{}
	return svc
}

type stubCdpMonitor struct{}

func (s *stubCdpMonitor) Start(_ context.Context) error { return nil }
func (s *stubCdpMonitor) Stop()                         {}
func (s *stubCdpMonitor) IsRunning() bool               { return false }

// failingCdpMonitor always fails to start, to exercise the reconcile-before-commit path.
type failingCdpMonitor struct{ running bool }

func (f *failingCdpMonitor) Start(_ context.Context) error {
	return errors.New("collector start failed")
}
func (f *failingCdpMonitor) Stop()           { f.running = false }
func (f *failingCdpMonitor) IsRunning() bool { return f.running }

func TestTelemetryCollectorFailureLeavesConfigUnchanged(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh PUT failure starts no session", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		svc.cdpMonitor = &failingCdpMonitor{}

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.PutTelemetry500JSONResponse{}, resp)
		assert.False(t, svc.telemetrySession.Active(), "failed collector start must not leave a session active")
	})

	t.Run("PATCH failure keeps the prior config", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		// Start a session that does not need the CDP collector (system only).
		tr := true
		start := allCategoriesDisabled()
		start.System = &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{Browser: start},
		})
		require.NoError(t, err)
		before := svc.telemetrySession.Config().Categories

		// Now the collector cannot start; enabling a CDP category must fail
		// without mutating the session config.
		svc.cdpMonitor = &failingCdpMonitor{}
		resp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				},
			},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.PatchTelemetry500JSONResponse{}, resp)
		assert.ElementsMatch(t, before, svc.telemetrySession.Config().Categories, "failed PATCH must not change the persisted config")
	})
}
