package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/recorder"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureConfigFrom(t *testing.T) {
	t.Run("nil body returns defaults", func(t *testing.T) {
		cfg, err := captureConfigFrom(nil)
		require.NoError(t, err)
		assert.Empty(t, cfg.Categories)
		assert.Empty(t, cfg.DetailLevel)
	})

	t.Run("valid categories", func(t *testing.T) {
		cats := []oapi.CaptureConfigCategories{oapi.Console, oapi.Network}
		body := &oapi.CreateCaptureSessionRequest{
			Config: &oapi.CaptureConfig{Categories: &cats},
		}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Len(t, cfg.Categories, 2)
	})

	t.Run("invalid category returns error", func(t *testing.T) {
		cats := []oapi.CaptureConfigCategories{"bogus"}
		body := &oapi.CreateCaptureSessionRequest{
			Config: &oapi.CaptureConfig{Categories: &cats},
		}
		_, err := captureConfigFrom(body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown category")
	})

	t.Run("valid detail level", func(t *testing.T) {
		dl := oapi.Verbose
		body := &oapi.CreateCaptureSessionRequest{
			Config: &oapi.CaptureConfig{DetailLevel: &dl},
		}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Equal(t, "verbose", string(cfg.DetailLevel))
	})

	t.Run("invalid detail level returns error", func(t *testing.T) {
		dl := oapi.CaptureConfigDetailLevel("garbage")
		body := &oapi.CreateCaptureSessionRequest{
			Config: &oapi.CaptureConfig{DetailLevel: &dl},
		}
		_, err := captureConfigFrom(body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown detail level")
	})

	t.Run("nil config returns defaults", func(t *testing.T) {
		body := &oapi.CreateCaptureSessionRequest{}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Empty(t, cfg.Categories)
		assert.Empty(t, cfg.DetailLevel)
	})
}

func TestCreateCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("success with no body", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		r201, ok := resp.(oapi.CreateCaptureSession201JSONResponse)
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
		dl := oapi.Minimal
		resp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{
			Body: &oapi.CreateCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats, DetailLevel: &dl},
			},
		})
		require.NoError(t, err)
		r201, ok := resp.(oapi.CreateCaptureSession201JSONResponse)
		require.True(t, ok)
		assert.NotEmpty(t, r201.Id)
	})

	t.Run("invalid category returns 400", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		cats := []oapi.CaptureConfigCategories{"badcat"}
		resp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{
			Body: &oapi.CreateCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats},
			},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.CreateCaptureSession400JSONResponse{}, resp)
	})

	t.Run("duplicate returns 409", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)

		resp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.CreateCaptureSession409JSONResponse{}, resp)
	})
}

func TestGetCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.GetCaptureSession(ctx, oapi.GetCaptureSessionRequestObject{
			CaptureSessionId: uuid.New(),
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.GetCaptureSession404JSONResponse{}, resp)
	})

	t.Run("wrong ID returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)

		resp, err := svc.GetCaptureSession(ctx, oapi.GetCaptureSessionRequestObject{
			CaptureSessionId: uuid.New(),
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.GetCaptureSession404JSONResponse{}, resp)
	})

	t.Run("correct ID returns 200", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		createResp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		created := createResp.(oapi.CreateCaptureSession201JSONResponse)

		resp, err := svc.GetCaptureSession(ctx, oapi.GetCaptureSessionRequestObject{
			CaptureSessionId: created.Id,
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.GetCaptureSession200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, created.Id, r200.Id)
		assert.Equal(t, created.CreatedAt, r200.CreatedAt)
	})
}

func TestUpdateCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			CaptureSessionId: uuid.New(),
			Body:             &oapi.UpdateCaptureSessionRequest{},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.UpdateCaptureSession404JSONResponse{}, resp)
	})

	t.Run("update config", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		createResp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		created := createResp.(oapi.CreateCaptureSession201JSONResponse)

		cats := []oapi.CaptureConfigCategories{oapi.Console}
		dl := oapi.Verbose
		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			CaptureSessionId: created.Id,
			Body: &oapi.UpdateCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats, DetailLevel: &dl},
			},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.UpdateCaptureSession200JSONResponse)
		require.True(t, ok)
		require.NotNil(t, r200.Config.Categories)
		assert.Len(t, *r200.Config.Categories, 1)
		assert.Equal(t, oapi.Console, (*r200.Config.Categories)[0])
		require.NotNil(t, r200.Config.DetailLevel)
		assert.Equal(t, oapi.Verbose, *r200.Config.DetailLevel)
	})

	t.Run("empty body is no-op", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		createResp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		created := createResp.(oapi.CreateCaptureSession201JSONResponse)

		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			CaptureSessionId: created.Id,
			Body:             &oapi.UpdateCaptureSessionRequest{},
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.UpdateCaptureSession200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, created.Id, r200.Id)
	})

	t.Run("invalid category returns 400", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		createResp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		created := createResp.(oapi.CreateCaptureSession201JSONResponse)

		cats := []oapi.CaptureConfigCategories{"invalid"}
		resp, err := svc.UpdateCaptureSession(ctx, oapi.UpdateCaptureSessionRequestObject{
			CaptureSessionId: created.Id,
			Body: &oapi.UpdateCaptureSessionRequest{
				Config: &oapi.CaptureConfig{Categories: &cats},
			},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.UpdateCaptureSession400JSONResponse{}, resp)
	})
}

func TestDeleteCaptureSession(t *testing.T) {
	ctx := context.Background()

	t.Run("no session returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		resp, err := svc.DeleteCaptureSession(ctx, oapi.DeleteCaptureSessionRequestObject{
			CaptureSessionId: uuid.New(),
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.DeleteCaptureSession404JSONResponse{}, resp)
	})

	t.Run("delete active session", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		createResp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		created := createResp.(oapi.CreateCaptureSession201JSONResponse)

		resp, err := svc.DeleteCaptureSession(ctx, oapi.DeleteCaptureSessionRequestObject{
			CaptureSessionId: created.Id,
		})
		require.NoError(t, err)
		r200, ok := resp.(oapi.DeleteCaptureSession200JSONResponse)
		require.True(t, ok)
		assert.Equal(t, created.Id, r200.Id)
		assert.Equal(t, oapi.CaptureSessionStatusStopped, r200.Status)
	})

	t.Run("create succeeds after delete", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		createResp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		created := createResp.(oapi.CreateCaptureSession201JSONResponse)

		_, err = svc.DeleteCaptureSession(ctx, oapi.DeleteCaptureSessionRequestObject{
			CaptureSessionId: created.Id,
		})
		require.NoError(t, err)

		resp, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)
		r201, ok := resp.(oapi.CreateCaptureSession201JSONResponse)
		require.True(t, ok)
		assert.NotEqual(t, created.Id, r201.Id)
	})

	t.Run("wrong ID returns 404", func(t *testing.T) {
		svc := newTestService(t, newMockRecordManager())
		_, err := svc.CreateCaptureSession(ctx, oapi.CreateCaptureSessionRequestObject{})
		require.NoError(t, err)

		resp, err := svc.DeleteCaptureSession(ctx, oapi.DeleteCaptureSessionRequestObject{
			CaptureSessionId: uuid.New(),
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.DeleteCaptureSession404JSONResponse{}, resp)
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
	return svc
}
