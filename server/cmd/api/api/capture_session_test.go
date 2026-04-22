package api

import (
	"context"
	"testing"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureConfigFrom(t *testing.T) {
	t.Run("nil body returns defaults", func(t *testing.T) {
		cfg, err := captureConfigFrom(nil)
		require.NoError(t, err)
		assert.Empty(t, cfg.Categories)
	})

	t.Run("valid categories", func(t *testing.T) {
		cats := []oapi.CaptureConfigCategories{oapi.Console, oapi.Network}
		body := &oapi.StartCaptureSessionRequest{
			Config: &oapi.CaptureConfig{Categories: &cats},
		}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Len(t, cfg.Categories, 2)
	})

	t.Run("invalid category returns error", func(t *testing.T) {
		cats := []oapi.CaptureConfigCategories{"bogus"}
		body := &oapi.StartCaptureSessionRequest{
			Config: &oapi.CaptureConfig{Categories: &cats},
		}
		_, err := captureConfigFrom(body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown category")
	})

	t.Run("nil config returns defaults", func(t *testing.T) {
		body := &oapi.StartCaptureSessionRequest{}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Empty(t, cfg.Categories)
	})
}

func TestStartCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("success with no body", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)
		r201, ok := resp.(oapi.StartCaptureSession201JSONResponse)
		require.True(t, ok)
		assert.NotEmpty(t, r201.Id)
		assert.NotZero(t, r201.CreatedAt)
		// Status depends on cdpMonitor.IsRunning(); the stub monitor doesn't
		// track state, so we only verify the field is populated.
		assert.NotEmpty(t, r201.Status)
	})

	t.Run("success with config", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		cats := []oapi.CaptureConfigCategories{oapi.Console}
		resp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{
			Body: &oapi.StartCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats},
			},
		})
		require.NoError(t, err)
		r201, ok := resp.(oapi.StartCaptureSession201JSONResponse)
		require.True(t, ok)
		assert.NotEmpty(t, r201.Id)
	})

	t.Run("invalid category returns 400", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		cats := []oapi.CaptureConfigCategories{"badcat"}
		resp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{
			Body: &oapi.StartCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats},
			},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCaptureSession400JSONResponse{}, resp)
	})

	t.Run("duplicate returns 409", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)

		resp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCaptureSession409JSONResponse{}, resp)
	})
}

func TestGetCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.GetCaptureSession(ctx, oapi.GetCaptureSessionRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.GetCaptureSession404JSONResponse{}, resp)
	})

	t.Run("active session returns 200", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		startResp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)
		started := startResp.(oapi.StartCaptureSession201JSONResponse)

		resp, err := svc.GetCaptureSession(ctx, oapi.GetCaptureSessionRequestObject{})
		require.NoError(t, err)
		r200, ok := resp.(oapi.GetCaptureSession200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, started.Id, r200.Id)
		assert.Equal(t, started.CreatedAt, r200.CreatedAt)
	})
}

func TestUpdateCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			Body: &oapi.UpdateCaptureSessionRequest{},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.UpdateCaptureSession404JSONResponse{}, resp)
	})

	t.Run("update config", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)

		cats := []oapi.CaptureConfigCategories{oapi.Console}
		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			Body: &oapi.UpdateCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats},
			},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.UpdateCaptureSession200JSONResponse)
		require.True(t, ok)
		require.NotNil(t, r200.Config.Categories)
		assert.Len(t, *r200.Config.Categories, 1)
		assert.Equal(t, oapi.Console, (*r200.Config.Categories)[0])
	})

	t.Run("empty body is no-op", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		startResp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)
		started := startResp.(oapi.StartCaptureSession201JSONResponse)

		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			Body: &oapi.UpdateCaptureSessionRequest{},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.UpdateCaptureSession200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, started.Id, r200.Id)
	})

	t.Run("invalid category returns 400", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)

		cats := []oapi.CaptureConfigCategories{"invalid"}
		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			Body: &oapi.UpdateCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats},
			},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.UpdateCaptureSession400JSONResponse{}, resp)
	})
}

func TestStopCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.StopCaptureSession(ctx, oapi.StopCaptureSessionRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StopCaptureSession404JSONResponse{}, resp)
	})

	t.Run("stop active session", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		startResp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)
		started := startResp.(oapi.StartCaptureSession201JSONResponse)

		resp, err := svc.StopCaptureSession(ctx, oapi.StopCaptureSessionRequestObject{})
		require.NoError(t, err)
		r200, ok := resp.(oapi.StopCaptureSession200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, started.Id, r200.Id)
		assert.Equal(t, oapi.CaptureSessionStatusStopped, r200.Status)
	})

	t.Run("start succeeds after stop", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		startResp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)
		started := startResp.(oapi.StartCaptureSession201JSONResponse)

		_, err = svc.StopCaptureSession(ctx, oapi.StopCaptureSessionRequestObject{})
		require.NoError(t, err)

		resp, err := svc.StartCaptureSession(ctx, oapi.StartCaptureSessionRequestObject{})
		require.NoError(t, err)
		r201, ok := resp.(oapi.StartCaptureSession201JSONResponse)
		require.True(t, ok)
		assert.NotEqual(t, started.Id, r201.Id)
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

// newTestService builds an ApiService with minimal dependencies for capture session tests.
func newTestService(t *testing.T, mgr recorder.RecordManager) *ApiService {
	t.Helper()
	svc, err := New(mgr, newMockFactory(), newTestUpstreamManager(), scaletozero.NewNoopController(), newMockNekoClient(t), newCaptureSession(t), 0)
	require.NoError(t, err)
	svc.cdpMonitor = &stubCdpMonitor{}
	return svc
}

type stubCdpMonitor struct{}

func (s *stubCdpMonitor) Start(_ context.Context) error { return nil }
func (s *stubCdpMonitor) Stop()                         {}
func (s *stubCdpMonitor) IsRunning() bool               { return false }
