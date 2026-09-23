package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpmonitor"
	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/kernel/kernel-images/server/lib/telemetry"
	"github.com/samber/lo"
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
		Control:     &oapi.BrowserTelemetryControlConfig{Enabled: lo.ToPtr(false)},
		Platform:    off(),
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

	t.Run("opt-in captures exactly the enabled categories", func(t *testing.T) {
		tr := true
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				Network: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
			},
		})
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.ElementsMatch(t, []oapi.TelemetryEventCategory{events.Console, events.Network}, cfg.Categories)
	})

	t.Run("omitted category is off (opt-in)", func(t *testing.T) {
		tr := true
		cfg, _, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
			},
		})
		require.NoError(t, err)
		// Only console is enabled; default-bundle categories are not added in.
		assert.Equal(t, []oapi.TelemetryEventCategory{events.Console}, cfg.Categories)
	})

	t.Run("enabled:nil is treated as off", func(t *testing.T) {
		_, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Console: &oapi.BrowserTelemetryCategoryConfig{}, // Enabled nil → off
			},
		})
		require.NoError(t, err)
		assert.True(t, allDisabled, "a browser config that enables nothing clears telemetry")
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

	t.Run("empty browser config clears", func(t *testing.T) {
		_, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{},
		})
		require.NoError(t, err)
		assert.True(t, allDisabled)
	})
}

func TestPutTelemetryIgnoresUnknownCategory(t *testing.T) {
	// Forward-compat: a newer control plane may send a telemetry category this
	// image does not yet know. The strict handler decodes the body with
	// encoding/json (no DisallowUnknownFields), so an unknown category must be
	// ignored, not rejected, and known categories must still apply.
	ctx := context.Background()
	svc := newTestService(t, newMockRecordManager())

	var body oapi.PutTelemetryJSONRequestBody
	raw := []byte(`{"browser":{"console":{"enabled":true},"future_category":{"enabled":true}}}`)
	require.NoError(t, json.Unmarshal(raw, &body))

	resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: &body})
	require.NoError(t, err)
	r201, ok := resp.(oapi.PutTelemetry201JSONResponse)
	require.True(t, ok, "expected 201, got %T", resp)
	require.NotNil(t, r201.Config.Browser)
	require.NotNil(t, r201.Config.Browser.Console)
	require.NotNil(t, r201.Config.Browser.Console.Enabled)
	assert.True(t, *r201.Config.Browser.Console.Enabled, "known category should be captured")
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
				Control: &oapi.BrowserTelemetryControlConfig{Enabled: &tr},
			},
		},
	})
	require.NoError(t, err)
	assert.True(t, TelemetryMiddlewareEnabled(), "PUT with control=true should enable middleware")

	_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Control: &oapi.BrowserTelemetryControlConfig{Enabled: &f},
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

// The middleware is the sole producer of platform_api_call as well as api_call,
// so a reader who migrates from control to platform must still get events.
func TestTelemetryHandlersEnableMiddlewareForPlatformOnly(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(DisableTelemetryMiddleware)

	svc := newTestService(t, newMockRecordManager())

	DisableTelemetryMiddleware()
	tr, f := true, false
	_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Platform: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
			},
		},
	})
	require.NoError(t, err)
	assert.True(t, TelemetryMiddlewareEnabled(), "PUT with platform=true should enable middleware")

	_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Control: &oapi.BrowserTelemetryControlConfig{Enabled: &f},
			},
		},
	})
	require.NoError(t, err)
	assert.True(t, TelemetryMiddlewareEnabled(), "dropping control should leave the middleware on for platform")

	_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
		Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Platform: &oapi.BrowserTelemetryCategoryConfig{Enabled: &f},
			},
		},
	})
	require.NoError(t, err)
	assert.False(t, TelemetryMiddlewareEnabled(), "dropping platform too should disable middleware")
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
		// Optional in the schema so an older image's response still validates,
		// but always set here: absent would mean "not reported", not zero.
		require.NotNil(t, r200.DroppedEvents)
		assert.Zero(t, *r200.DroppedEvents)
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
	svc, err := New(mgr, newMockFactory(), newTestUpstreamManager(), scaletozero.NewNoopController(), newMockNekoClient(t), ts, es, 0, nil, nil)
	require.NoError(t, err)
	svc.cdpMonitor = &stubCdpMonitor{}
	return svc
}

type stubCdpMonitor struct{}

func (s *stubCdpMonitor) SetTelemetry(bool) error { return nil }
func (s *stubCdpMonitor) NetworkSnapshot() cdpmonitor.NetworkSnapshot {
	return cdpmonitor.NetworkSnapshot{}
}

func (s *stubCdpMonitor) Start(_ context.Context) error { return nil }
func (s *stubCdpMonitor) Stop()                         {}
func (s *stubCdpMonitor) IsRunning() bool               { return false }

// failingCdpMonitor rejects optional capture to exercise configuration rollback.
type failingCdpMonitor struct{ running bool }

func (f *failingCdpMonitor) SetTelemetry(enabled bool) error {
	if enabled {
		return errors.New("collector configuration failed")
	}
	return nil
}
func (f *failingCdpMonitor) NetworkSnapshot() cdpmonitor.NetworkSnapshot {
	return cdpmonitor.NetworkSnapshot{}
}

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

		// Enable a CDP category so the (failing) collector start is attempted.
		tr := true
		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{
					Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr},
				},
			},
		})
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

// stubOTLPExporter records start/stop calls for export-toggle tests.
type stubOTLPExporter struct {
	running  bool
	starts   int
	stops    int
	startErr error
}

func (s *stubOTLPExporter) Start(context.Context) error {
	if s.startErr != nil {
		return s.startErr
	}
	s.starts++
	s.running = true
	return nil
}

func (s *stubOTLPExporter) Stop(context.Context) error {
	s.stops++
	s.running = false
	return nil
}

func (s *stubOTLPExporter) Running() bool { return s.running }

func TestTelemetryExportToggle(t *testing.T) {
	ctx := context.Background()
	tr, fa := true, false
	exportOn := func() *oapi.BrowserTelemetryConfig {
		return &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
			Export:  &oapi.BrowserTelemetryExportConfig{Otlp: &oapi.BrowserTelemetryOTLPExportConfig{Enabled: &tr}},
		}
	}

	t.Run("PUT enables export, PATCH disables it", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		exp := &stubOTLPExporter{}
		svc.otlpExport = exp

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: exportOn()})
		require.NoError(t, err)
		r := resp.(oapi.PutTelemetry201JSONResponse)
		assert.True(t, exp.Running(), "export should be running after enable")
		assert.True(t, *r.Config.Export.Otlp.Enabled, "response should report export enabled")

		presp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{
			Body: &oapi.BrowserTelemetryConfig{Export: &oapi.BrowserTelemetryExportConfig{Otlp: &oapi.BrowserTelemetryOTLPExportConfig{Enabled: &fa}}},
		})
		require.NoError(t, err)
		p := presp.(oapi.PatchTelemetry200JSONResponse)
		assert.False(t, exp.Running(), "export should stop after disable")
		assert.False(t, *p.Config.Export.Otlp.Enabled)
	})

	t.Run("export defaults off when unspecified", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		exp := &stubOTLPExporter{}
		svc.otlpExport = exp

		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{})
		require.NoError(t, err)
		assert.False(t, exp.Running(), "export must stay off without an explicit toggle")
		assert.Equal(t, 0, exp.starts)
	})

	t.Run("enabling export with no endpoint provisioned is a no-op", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager()) // otlpExport left nil
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: exportOn()})
		require.NoError(t, err, "a missing endpoint must not fail the telemetry apply")
	})

	t.Run("clearing the session stops export", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		exp := &stubOTLPExporter{}
		svc.otlpExport = exp

		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: exportOn()})
		require.NoError(t, err)
		require.True(t, exp.Running())

		_, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()}})
		require.NoError(t, err)
		assert.False(t, exp.Running(), "clearing telemetry should stop export")
	})

	t.Run("toggle-off drain does not block concurrent reads", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		exp := &blockingStopExporter{running: true, stopEntered: make(chan struct{}), release: make(chan struct{})}
		svc.otlpExport = exp

		// Disable export while keeping a category on, so reconcileExport hits the
		// drain path; run it in the background since Stop blocks.
		off := &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
			Export:  &oapi.BrowserTelemetryExportConfig{Otlp: &oapi.BrowserTelemetryOTLPExportConfig{Enabled: &fa}},
		}
		putDone := make(chan struct{})
		go func() {
			_, _ = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: off})
			close(putDone)
		}()

		<-exp.stopEntered // drain is now in progress, holding exportMu (not monitorMu)

		// A concurrent read must not block behind the drain.
		readReturned := make(chan struct{})
		go func() {
			_, _ = svc.GetTelemetry(ctx, oapi.GetTelemetryRequestObject{})
			close(readReturned)
		}()
		select {
		case <-readReturned:
		case <-time.After(2 * time.Second):
			t.Fatal("GET blocked on the export drain (monitorMu held during Stop)")
		}

		close(exp.release)
		<-putDone
	})
}

// blockingStopExporter blocks in Stop until released, to prove the drain does
// not hold monitorMu.
type blockingStopExporter struct {
	mu          sync.Mutex
	running     bool
	stopEntered chan struct{}
	release     chan struct{}
}

func (e *blockingStopExporter) Start(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = true
	return nil
}

func (e *blockingStopExporter) Stop(context.Context) error {
	close(e.stopEntered)
	<-e.release
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = false
	return nil
}

func (e *blockingStopExporter) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

func TestCdpExcludedMethodsRoundTrip(t *testing.T) {
	ctx := context.Background()
	excluded := []oapi.BrowserCdpCommandMethod{"Input.dispatchMouseEvent", "Page.captureScreenshot"}
	withExclusions := func() *oapi.BrowserTelemetryConfig {
		return &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Control: &oapi.BrowserTelemetryControlConfig{
					Enabled: lo.ToPtr(true),
					Cdp:     &oapi.BrowserTelemetryCdpControlConfig{ExcludedMethods: &excluded},
				},
			},
		}
	}

	t.Run("put stores them and the session exposes them to the proxy", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: withExclusions()})
		require.NoError(t, err)
		created := resp.(oapi.PutTelemetry201JSONResponse)
		require.NotNil(t, created.Config.Browser.Control.Cdp)
		assert.Equal(t, excluded, *created.Config.Browser.Control.Cdp.ExcludedMethods)

		// The proxy reads this set per command, so it has to reflect the config.
		assert.Equal(t, map[string]struct{}{
			"Input.dispatchMouseEvent": {},
			"Page.captureScreenshot":   {},
		}, svc.telemetrySession.ExcludedCdpMethods())
	})

	t.Run("patch leaves an omitted list alone and an empty list clears it", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: withExclusions()})
		require.NoError(t, err)

		// Category toggle only: the exclusions are not mentioned, so they stand.
		_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				System: &oapi.BrowserTelemetryCategoryConfig{Enabled: lo.ToPtr(true)},
			},
		}})
		require.NoError(t, err)
		assert.Len(t, svc.telemetrySession.ExcludedCdpMethods(), 2)

		empty := []oapi.BrowserCdpCommandMethod{}
		_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{
				Control: &oapi.BrowserTelemetryControlConfig{
					Cdp: &oapi.BrowserTelemetryCdpControlConfig{ExcludedMethods: &empty},
				},
			},
		}})
		require.NoError(t, err)
		assert.Empty(t, svc.telemetrySession.ExcludedCdpMethods())
	})
}

// dropped_events was added to TelemetryState after it shipped, so it stays
// optional: a response from an image that predates it must still decode, and
// an old client's control block must still be a valid request.
func TestTelemetryStateStaysCompatibleWithOlderImages(t *testing.T) {
	var state oapi.TelemetryState
	err := json.Unmarshal([]byte(`{"config":{},"seq":42}`), &state)
	require.NoError(t, err)
	assert.Nil(t, state.DroppedEvents, "absent means not reported, which is not zero")
	assert.EqualValues(t, 42, state.Seq)

	// A client that predates control.cdp sends only enabled, and still parses.
	var cfg oapi.BrowserTelemetryConfig
	err = json.Unmarshal([]byte(`{"browser":{"control":{"enabled":true}}}`), &cfg)
	require.NoError(t, err)
	require.NotNil(t, cfg.Browser.Control)
	assert.True(t, *cfg.Browser.Control.Enabled)
	assert.Nil(t, cfg.Browser.Control.Cdp)
}

func TestTelemetryStorageToggle(t *testing.T) {
	ctx := context.Background()
	tr, fa := true, false
	storageOff := func() *oapi.BrowserTelemetryConfig {
		return &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Network: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &fa},
		}
	}
	storageOmitted := func() *oapi.BrowserTelemetryConfig {
		return &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Network: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
		}
	}
	storageOf := func(t *testing.T, cfg oapi.BrowserTelemetryConfig) bool {
		t.Helper()
		require.NotNil(t, cfg.Storage, "storage must always be echoed")
		require.NotNil(t, cfg.Storage.Enabled)
		return *cfg.Storage.Enabled
	}

	t.Run("omitted storage means on", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(nil)
		require.NoError(t, err)
		assert.False(t, allDisabled)
		assert.True(t, cfg.StoreS2)

		cfg, _, err = telemetryConfigFromOAPI(storageOmitted())
		require.NoError(t, err)
		assert.True(t, cfg.StoreS2)

		cfg, _, err = telemetryConfigFromOAPI(storageOff())
		require.NoError(t, err)
		assert.False(t, cfg.StoreS2)
	})

	t.Run("a clear keeps the storage toggle it carried", func(t *testing.T) {
		cfg, allDisabled, err := telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{
			Browser: allCategoriesDisabled(),
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &fa},
		})
		require.NoError(t, err)
		assert.True(t, allDisabled)
		assert.False(t, cfg.StoreS2)

		// The zero value is off, so the positive case is what proves the
		// toggle is carried rather than dropped.
		cfg, allDisabled, err = telemetryConfigFromOAPI(&oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()})
		require.NoError(t, err)
		assert.True(t, allDisabled)
		assert.True(t, cfg.StoreS2)

		for _, current := range []bool{false, true} {
			merged, allDisabled := mergeTelemetryConfig(telemetry.TelemetryConfig{Categories: events.DefaultCategories, StoreS2: current}, &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()})
			assert.True(t, allDisabled)
			assert.Equal(t, current, merged.StoreS2, "an all-disabled patch must keep the current storage toggle")
		}
	})

	t.Run("patch leaves an omitted toggle alone", func(t *testing.T) {
		current := telemetry.TelemetryConfig{Categories: []oapi.TelemetryEventCategory{events.Network}, StoreS2: false}
		merged, _ := mergeTelemetryConfig(current, &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
		})
		assert.False(t, merged.StoreS2)

		merged, _ = mergeTelemetryConfig(current, &oapi.BrowserTelemetryConfig{
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &tr},
		})
		assert.True(t, merged.StoreS2)
	})

	t.Run("GET, PUT and PATCH echo storage exactly", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: storageOff()})
		require.NoError(t, err)
		r201, ok := resp.(oapi.PutTelemetry201JSONResponse)
		require.True(t, ok, "expected 201, got %T", resp)
		assert.False(t, storageOf(t, r201.Config))

		got, err := svc.GetTelemetry(ctx, oapi.GetTelemetryRequestObject{})
		require.NoError(t, err)
		assert.False(t, storageOf(t, got.(oapi.GetTelemetry200JSONResponse).Config))

		// A category-only PATCH is not a storage change.
		presp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
		}})
		require.NoError(t, err)
		assert.False(t, storageOf(t, presp.(oapi.PatchTelemetry200JSONResponse).Config))

		// A storage-only PATCH is applied, not treated as an empty body.
		presp, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &tr},
		}})
		require.NoError(t, err)
		assert.True(t, storageOf(t, presp.(oapi.PatchTelemetry200JSONResponse).Config))
		assert.True(t, svc.telemetrySession.Config().StoreS2)

		resp, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: storageOmitted()})
		require.NoError(t, err)
		assert.True(t, storageOf(t, resp.(oapi.PutTelemetry200JSONResponse).Config))
	})

	t.Run("a clear echoes the storage toggle it carried", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: storageOff()})
		require.NoError(t, err)

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: allCategoriesDisabled(),
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &fa},
		}})
		require.NoError(t, err)
		assert.False(t, storageOf(t, resp.(oapi.PutTelemetry200JSONResponse).Config))
		assert.False(t, svc.telemetrySession.Active())

		_, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: storageOff()})
		require.NoError(t, err)
		presp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()}})
		require.NoError(t, err)
		assert.False(t, storageOf(t, presp.(oapi.PatchTelemetry200JSONResponse).Config), "a clearing PATCH keeps the session's storage toggle")
	})
}

// stubS2Storage records starts for storage-toggle tests. Locked because
// reconcileStorage runs outside monitorMu.
type stubS2Storage struct {
	mu          sync.Mutex
	starts      int
	running     bool
	everStarted bool
	startErr    error
	afterSeq    uint64
}

func (s *stubS2Storage) Start(_ context.Context, afterSeq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return s.startErr
	}
	s.starts++
	s.afterSeq = afterSeq
	s.running, s.everStarted = true, true
	return nil
}

func (s *stubS2Storage) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	return nil
}

func (s *stubS2Storage) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *stubS2Storage) EverStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everStarted
}

func (s *stubS2Storage) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

func TestTelemetryStorageStart(t *testing.T) {
	ctx := context.Background()
	tr, fa := true, false
	networkOn := func(storage *bool) *oapi.BrowserTelemetryConfig {
		cfg := &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Network: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
		}
		if storage != nil {
			cfg.Storage = &oapi.BrowserTelemetryStorageConfig{Enabled: storage}
		}
		return cfg
	}
	cleared := func(storage *bool) *oapi.BrowserTelemetryConfig {
		cfg := &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()}
		if storage != nil {
			cfg.Storage = &oapi.BrowserTelemetryStorageConfig{Enabled: storage}
		}
		return cfg
	}
	newStorageService := func(t *testing.T) (*ApiService, *stubS2Storage) {
		svc := newTestService(t, newMockRecordManager())
		st := &stubS2Storage{}
		svc.s2Storage = st
		return svc, st
	}
	publishNetwork := func(t *testing.T, svc *ApiService) {
		t.Helper()
		_, ok := svc.telemetrySession.Publish(events.Event{Type: "network.request", Category: events.Network, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}})
		require.True(t, ok, "the session must admit the event")
	}

	t.Run("storage off never opens the sink", func(t *testing.T) {
		svc, st := newStorageService(t)

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(&fa)})
		require.NoError(t, err)
		r201, ok := resp.(oapi.PutTelemetry201JSONResponse)
		require.True(t, ok, "expected 201, got %T", resp)
		assert.False(t, *r201.Config.Storage.Enabled)
		require.True(t, svc.telemetrySession.Active(), "the session runs; only storage is off")

		// Events flow to the ring (and so to the live stream and export) but
		// nothing is opened that could persist them.
		publishNetwork(t, svc)
		assert.False(t, st.EverStarted())
		assert.Equal(t, 0, st.startCount())
	})

	t.Run("storage omitted opens the sink once", func(t *testing.T) {
		svc, st := newStorageService(t)

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry201JSONResponse{}, resp)
		assert.Equal(t, 1, st.startCount())
		assert.True(t, st.Running())

		resp, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry200JSONResponse{}, resp)
		_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
		}})
		require.NoError(t, err)
		assert.Equal(t, 1, st.startCount(), "the sink opens at most once per process")
	})

	t.Run("storage off after the sink opened is rejected", func(t *testing.T) {
		svc, st := newStorageService(t)
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		require.True(t, st.EverStarted())
		publishNetwork(t, svc)
		before := svc.telemetrySession.Config()
		seq := svc.eventStream.Seq()
		appliedAt := svc.telemetrySession.AppliedAt()

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &fa},
		}})
		require.NoError(t, err)
		r409, ok := resp.(oapi.PutTelemetry409JSONResponse)
		require.True(t, ok, "expected 409, got %T", resp)
		assert.Equal(t, storageDisableConflict, r409.Message)

		after := svc.telemetrySession.Config()
		assert.ElementsMatch(t, before.Categories, after.Categories, "a 409 must not change the categories")
		assert.True(t, after.StoreS2, "a 409 must not change the storage toggle")
		assert.Equal(t, appliedAt, svc.telemetrySession.AppliedAt(), "a 409 must not re-apply the session")
		assert.Equal(t, seq, svc.eventStream.Seq())
		assert.True(t, st.Running(), "the sink keeps running")
		assert.True(t, svc.telemetrySession.Active())

		// A clear that carries storage off is refused the same way: the request
		// asks for a state this instance can no longer honor.
		resp, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: cleared(&fa)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry409JSONResponse{}, resp)
		assert.True(t, svc.telemetrySession.Active(), "a refused clear leaves the session running")

		// A clear that leaves storage alone still works.
		resp, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: cleared(nil)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry200JSONResponse{}, resp)
		assert.False(t, svc.telemetrySession.Active())
		assert.True(t, st.Running(), "the handler never stops the sink")
	})

	t.Run("patch off after the sink opened is rejected", func(t *testing.T) {
		svc, st := newStorageService(t)
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		require.True(t, st.EverStarted())

		resp, err := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &fa},
		}})
		require.NoError(t, err)
		r409, ok := resp.(oapi.PatchTelemetry409JSONResponse)
		require.True(t, ok, "expected 409, got %T", resp)
		assert.Equal(t, storageDisableConflict, r409.Message)
		assert.True(t, svc.telemetrySession.Config().StoreS2)

		resp, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: allCategoriesDisabled(),
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &fa},
		}})
		require.NoError(t, err)
		require.IsType(t, oapi.PatchTelemetry409JSONResponse{}, resp)
		assert.True(t, svc.telemetrySession.Active(), "a refused clearing patch leaves the session running")

		// Omitting storage keeps it on and is not a conflict.
		resp, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
		}})
		require.NoError(t, err)
		r200, ok := resp.(oapi.PatchTelemetry200JSONResponse)
		require.True(t, ok, "expected 200, got %T", resp)
		assert.True(t, *r200.Config.Storage.Enabled)
	})

	t.Run("a clear does not open the sink", func(t *testing.T) {
		svc, st := newStorageService(t)

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: cleared(nil)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry200JSONResponse{}, resp)
		resp, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: cleared(&fa)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry200JSONResponse{}, resp, "storage off is fine while nothing has started")
		assert.Equal(t, 0, st.startCount())
		assert.False(t, st.EverStarted())
	})

	t.Run("the first storing session stores from its start", func(t *testing.T) {
		svc, st := newStorageService(t)
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		assert.Zero(t, st.afterSeq, "nothing precedes the first session, so nothing is skipped")
	})

	t.Run("a session that never stored can start storing, without what it captured", func(t *testing.T) {
		svc, st := newStorageService(t)
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(&fa)})
		require.NoError(t, err)
		publishNetwork(t, svc)
		publishNetwork(t, svc)
		require.Equal(t, 0, st.startCount())

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry200JSONResponse{}, resp)
		assert.Equal(t, 1, st.startCount())
		assert.EqualValues(t, 2, st.afterSeq, "events captured with storage off must not be stored")
	})

	t.Run("a patch that turns storage on skips what was captured before it", func(t *testing.T) {
		svc, st := newStorageService(t)
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(&fa)})
		require.NoError(t, err)
		publishNetwork(t, svc)

		_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &tr},
		}})
		require.NoError(t, err)
		assert.Equal(t, 1, st.startCount())
		assert.EqualValues(t, 1, st.afterSeq)
	})

	t.Run("a storing session after a cleared storage-off one skips it", func(t *testing.T) {
		svc, st := newStorageService(t)
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(&fa)})
		require.NoError(t, err)
		publishNetwork(t, svc)
		publishNetwork(t, svc)
		publishNetwork(t, svc)
		_, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: cleared(&fa)})
		require.NoError(t, err)

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry201JSONResponse{}, resp)
		assert.EqualValues(t, 3, st.afterSeq)
	})

	t.Run("a retried start keeps the session's floor", func(t *testing.T) {
		svc, st := newStorageService(t)
		st.startErr = errors.New("basin unreachable")
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		publishNetwork(t, svc)

		st.mu.Lock()
		st.startErr = nil
		st.mu.Unlock()
		_, err = svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		assert.Zero(t, st.afterSeq, "events captured while storage was on but failing to open are still stored")
	})

	t.Run("a failed start is retried by the next request", func(t *testing.T) {
		svc, st := newStorageService(t)
		st.startErr = errors.New("basin unreachable")

		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry201JSONResponse{}, resp, "storage is best-effort")
		assert.False(t, st.EverStarted())

		st.mu.Lock()
		st.startErr = nil
		st.mu.Unlock()
		_, err = svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
			Browser: &oapi.BrowserTelemetryCategoriesConfig{Console: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
		}})
		require.NoError(t, err)
		assert.Equal(t, 1, st.startCount())
	})

	t.Run("no controller means no storage and no conflict", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager()) // s2Storage left nil
		_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
		require.NoError(t, err)
		resp, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(&fa)})
		require.NoError(t, err)
		require.IsType(t, oapi.PutTelemetry200JSONResponse{}, resp)
	})

	t.Run("storage off waits for a start in flight", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		st := &blockingStartS2Storage{entered: make(chan struct{}), release: make(chan struct{})}
		svc.s2Storage = st

		// The PUT that turns storage on does not return until its deferred
		// reconcile has run Start, which this stub holds open.
		onDone := make(chan oapi.PutTelemetryResponseObject, 1)
		go func() {
			resp, _ := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(nil)})
			onDone <- resp
		}()
		select {
		case <-st.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("PUT did not reach the storage start")
		}

		// A storage-off request arriving now must not slip past the guard: it
		// waits for the start to settle, then sees it and refuses.
		offDone := make(chan oapi.PutTelemetryResponseObject, 1)
		go func() {
			resp, _ := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: networkOn(&fa)})
			offDone <- resp
		}()
		select {
		case resp := <-offDone:
			t.Fatalf("storage-off request returned %T while the sink was still opening", resp)
		case <-time.After(100 * time.Millisecond):
		}

		close(st.release)
		require.IsType(t, oapi.PutTelemetry201JSONResponse{}, <-onDone)
		require.IsType(t, oapi.PutTelemetry409JSONResponse{}, <-offDone)
		assert.True(t, svc.telemetrySession.Config().StoreS2, "the committed config never said off while the sink was open")
	})
}

// blockingStartS2Storage holds Start open until released, to prove the guard
// waits on storageMu rather than reading EverStarted mid-start.
type blockingStartS2Storage struct {
	mu          sync.Mutex
	entered     chan struct{}
	release     chan struct{}
	everStarted bool
}

func (s *blockingStartS2Storage) Start(context.Context, uint64) error {
	close(s.entered)
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	s.everStarted = true
	return nil
}

func (s *blockingStartS2Storage) Stop(context.Context) error { return nil }
func (s *blockingStartS2Storage) Running() bool              { return s.EverStarted() }
func (s *blockingStartS2Storage) EverStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everStarted
}

// gatedCdpMonitor holds the first optional-capture enable open until released,
// then fails it, so a test can act while a PUT or PATCH is mid-apply and then
// drive its rollback.
type gatedCdpMonitor struct {
	stubCdpMonitor
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedCdpMonitor) SetTelemetry(enabled bool) error {
	if !enabled {
		return nil
	}
	gated := false
	g.once.Do(func() { gated = true })
	if !gated {
		return nil
	}
	close(g.entered)
	<-g.release
	return errors.New("collector configuration failed")
}

func TestTelemetryStorageNeverReadsAnUnsettledConfig(t *testing.T) {
	ctx := context.Background()
	tr, fa := true, false
	for _, method := range []string{"PUT", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			svc := newTestService(t, newMockRecordManager())
			st := &stubS2Storage{}
			svc.s2Storage = st
			// A non-CDP session with storage off, so the collector is not needed yet.
			_, err := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{System: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
				Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &fa},
			}})
			require.NoError(t, err)

			// Turn storage on together with a CDP category whose capture will
			// fail, so the request commits provisionally and then rolls back.
			gate := &gatedCdpMonitor{entered: make(chan struct{}), release: make(chan struct{})}
			svc.cdpMonitor = gate
			on := &oapi.BrowserTelemetryConfig{
				Browser: &oapi.BrowserTelemetryCategoriesConfig{Network: &oapi.BrowserTelemetryCategoryConfig{Enabled: &tr}},
				Storage: &oapi.BrowserTelemetryStorageConfig{Enabled: &tr},
			}
			done := make(chan any, 1)
			go func() {
				if method == "PATCH" {
					resp, _ := svc.PatchTelemetry(ctx, oapi.PatchTelemetryRequestObject{Body: on})
					done <- resp
					return
				}
				resp, _ := svc.PutTelemetry(ctx, oapi.PutTelemetryRequestObject{Body: on})
				done <- resp
			}()
			select {
			case <-gate.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not reach the capture apply")
			}

			// A reconcile deferred from an earlier request runs now. It must wait
			// for this request to settle rather than read storage-on mid-apply.
			reconciled := make(chan struct{})
			go func() {
				svc.reconcileStorage(ctx)
				close(reconciled)
			}()
			select {
			case <-reconciled:
				t.Fatal("reconcileStorage ran while the config was provisional")
			case <-time.After(100 * time.Millisecond):
			}

			close(gate.release)
			resp := <-done
			if method == "PATCH" {
				require.IsType(t, oapi.PatchTelemetry500JSONResponse{}, resp)
			} else {
				require.IsType(t, oapi.PutTelemetry500JSONResponse{}, resp)
			}
			<-reconciled
			assert.False(t, svc.telemetrySession.Config().StoreS2, "the failed request rolled storage back off")
			assert.Equal(t, 0, st.startCount(), "the sink must not open for a config that was rolled back")
		})
	}
}
