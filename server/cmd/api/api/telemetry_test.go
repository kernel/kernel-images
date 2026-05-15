package api

import (
	"context"
	"testing"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTelemetryConfigFromOAPI(t *testing.T) {
	t.Run("nil body returns defaults (all categories)", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(nil)
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.Empty(t, cfg.Categories)
	})

	t.Run("nil browser key returns defaults", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{})
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.Empty(t, cfg.Categories)
	})

	t.Run("omitted enabled defaults to true", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console: &oapi.BrowserTelemetryCategoryConfig{}, // Enabled is nil → defaults to true
			},
		})
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.Contains(t, cfg.Categories, events.Console)
	})

	t.Run("all false returns allDisabled=true", func(t *testing.T) {
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
		assert.True(t, allDisabled)
	})

	t.Run("mixed enabled flags", func(t *testing.T) {
		tr, f := true, false
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				Network: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
			},
		})
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.Len(t, cfg.Categories, 3) // console + page + interaction (network=false, others default true)
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

		f := false
		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				},
			},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.PutTelemetry200JSONResponse)
		require.True(t, ok, "expected 200, got %T", resp)
		require.NotNil(t, r200.Config.Browser)
		require.NotNil(t, r200.Config.Browser.Console)
		assert.False(t, *r200.Config.Browser.Console.Enabled)
		assert.False(t, *r200.Config.Browser.Network.Enabled)
		assert.False(t, *r200.Config.Browser.Page.Enabled)
		assert.False(t, *r200.Config.Browser.Interaction.Enabled)
		assert.Nil(t, r200.AppliedAt, "applied_at must be omitted when telemetry is unconfigured")
	})
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

		f := false
		resp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				},
			},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.PatchTelemetry200JSONResponse)
		require.True(t, ok, "expected 200, got %T", resp)
		require.NotNil(t, r200.Config.Browser)
		require.NotNil(t, r200.Config.Browser.Console)
		assert.False(t, *r200.Config.Browser.Console.Enabled)
		assert.False(t, *r200.Config.Browser.Network.Enabled)
		assert.False(t, *r200.Config.Browser.Page.Enabled)
		assert.False(t, *r200.Config.Browser.Interaction.Enabled)
	})

	t.Run("put returns 201 after patch clears configuration", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)

		f := false
		_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
					Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
				},
			},
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

func (m *mockRecordManager) RegisterRecorder(_ context.Context, _ recorder.Recorder) error { return nil }
func (m *mockRecordManager) DeregisterRecorder(_ context.Context, _ recorder.Recorder) error {
	return nil
}
func (m *mockRecordManager) GetRecorder(_ string) (recorder.Recorder, bool) { return nil, false }
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
