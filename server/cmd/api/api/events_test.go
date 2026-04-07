package api

import (
	"context"
	"testing"

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
		cats := []oapi.StartCaptureRequestCategories{oapi.Console, oapi.Network}
		body := &oapi.StartCaptureRequest{Categories: &cats}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Len(t, cfg.Categories, 2)
	})

	t.Run("invalid category returns error", func(t *testing.T) {
		cats := []oapi.StartCaptureRequestCategories{"bogus"}
		body := &oapi.StartCaptureRequest{Categories: &cats}
		_, err := captureConfigFrom(body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown category")
	})

	t.Run("valid detail level", func(t *testing.T) {
		dl := oapi.Verbose
		body := &oapi.StartCaptureRequest{DetailLevel: &dl}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Equal(t, "verbose", string(cfg.DetailLevel))
	})

	t.Run("invalid detail level returns error", func(t *testing.T) {
		dl := oapi.StartCaptureRequestDetailLevel("garbage")
		body := &oapi.StartCaptureRequest{DetailLevel: &dl}
		_, err := captureConfigFrom(body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown detail level")
	})

	t.Run("nil categories and nil detail level", func(t *testing.T) {
		body := &oapi.StartCaptureRequest{}
		cfg, err := captureConfigFrom(body)
		require.NoError(t, err)
		assert.Empty(t, cfg.Categories)
		assert.Empty(t, cfg.DetailLevel)
	})
}

func TestStartCapture(t *testing.T) {
	ctx := context.Background()

	t.Run("success with no body", func(t *testing.T) {
		mgr := newMockRecordManager()
		svc := newTestService(t, mgr)
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture200Response{}, resp)
	})

	t.Run("invalid category returns 400", func(t *testing.T) {
		mgr := newMockRecordManager()
		svc := newTestService(t, mgr)
		cats := []oapi.StartCaptureRequestCategories{"badcat"}
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{
			Body: &oapi.StartCaptureRequest{Categories: &cats},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture400JSONResponse{}, resp)
	})

	t.Run("invalid detail level returns 400", func(t *testing.T) {
		mgr := newMockRecordManager()
		svc := newTestService(t, mgr)
		dl := oapi.StartCaptureRequestDetailLevel("garbage")
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{
			Body: &oapi.StartCaptureRequest{DetailLevel: &dl},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture400JSONResponse{}, resp)
	})

	t.Run("restart succeeds", func(t *testing.T) {
		mgr := newMockRecordManager()
		svc := newTestService(t, mgr)
		_, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		resp, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StartCapture200Response{}, resp)
	})
}

func TestStopCapture(t *testing.T) {
	ctx := context.Background()

	t.Run("stop when nothing running", func(t *testing.T) {
		mgr := newMockRecordManager()
		svc := newTestService(t, mgr)
		resp, err := svc.StopCapture(ctx, oapi.StopCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StopCapture200Response{}, resp)
	})

	t.Run("stop after start", func(t *testing.T) {
		mgr := newMockRecordManager()
		svc := newTestService(t, mgr)
		_, err := svc.StartCapture(ctx, oapi.StartCaptureRequestObject{})
		require.NoError(t, err)
		resp, err := svc.StopCapture(ctx, oapi.StopCaptureRequestObject{})
		require.NoError(t, err)
		assert.IsType(t, oapi.StopCapture200Response{}, resp)
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

// newTestService builds an ApiService with minimal dependencies for event tests.
func newTestService(t *testing.T, mgr recorder.RecordManager) *ApiService {
	t.Helper()
	svc, err := New(mgr, newMockFactory(), newTestUpstreamManager(), scaletozero.NewNoopController(), newMockNekoClient(t), newCaptureSession(t), 0)
	require.NoError(t, err)
	return svc
}
