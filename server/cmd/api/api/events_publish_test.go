package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/onkernel/kernel-images/server/lib/recorder"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPublishTestService(t *testing.T, logDir string) (*ApiService, *events.CaptureSession) {
	t.Helper()
	ring := events.NewRingBuffer(16)
	fw := events.NewFileWriter(logDir)
	cs := events.NewCaptureSession(ring, fw)
	cs.Start("test-session-123")
	svc, err := New(
		recorder.NewFFmpegManager(),
		newMockFactory(),
		newTestUpstreamManager(),
		scaletozero.NewNoopController(),
		newMockNekoClient(t),
		cs,
		0,
	)
	require.NoError(t, err)
	return svc, cs
}

func TestPublishEvent(t *testing.T) {
	t.Run("happy_path", func(t *testing.T) {
		logDir := t.TempDir()
		svc, cs := newPublishTestService(t, logDir)

		b, _ := json.Marshal(events.Event{
			Type:     "liveview_click",
			Category: events.CategoryLiveview,
			Source:   events.Source{Kind: events.KindKernelAPI},
			Data:     json.RawMessage(`{"x":100}`),
		})
		req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		reader := cs.NewReader(0)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		res, err := reader.Read(ctx)
		require.NoError(t, err)
		require.NotNil(t, res.Envelope)
		assert.Equal(t, "liveview_click", res.Envelope.Event.Type)
		assert.Equal(t, events.CategoryLiveview, res.Envelope.Event.Category)
	})

	t.Run("invalid_json", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader([]byte(`not-json`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("empty_type_rejected", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		b, _ := json.Marshal(events.Event{
			Category: events.CategoryConsole,
			Data:     json.RawMessage(`{"x":1}`),
		})
		req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("liveview_routes_correctly", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		b, _ := json.Marshal(events.Event{
			Type:     "liveview_click",
			Category: events.CategoryLiveview,
			Source:   events.Source{Kind: events.KindKernelAPI},
			Data:     json.RawMessage(`{"x":100}`),
		})
		req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		entries, err := os.ReadDir(logDir)
		require.NoError(t, err)
		found := false
		for _, e := range entries {
			if e.Name() == "liveview.log" {
				info, _ := e.Info()
				assert.Greater(t, info.Size(), int64(0))
				found = true
			}
		}
		assert.True(t, found, "liveview.log should exist in logDir")
	})

	t.Run("captcha_routes_correctly", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		b, _ := json.Marshal(events.Event{
			Type:     "captcha_solve",
			Category: events.CategoryCaptcha,
			Source:   events.Source{Kind: events.KindKernelAPI},
			Data:     json.RawMessage(`{"token":"abc"}`),
		})
		req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		entries, err := os.ReadDir(logDir)
		require.NoError(t, err)
		found := false
		for _, e := range entries {
			if e.Name() == "captcha.log" {
				info, _ := e.Info()
				assert.Greater(t, info.Size(), int64(0))
				found = true
			}
		}
		assert.True(t, found, "captcha.log should exist in logDir")
	})

	t.Run("category_derived_from_type", func(t *testing.T) {
		logDir := t.TempDir()
		svc, cs := newPublishTestService(t, logDir)

		// No Category field set — should be derived from Type prefix (underscore separator)
		b, _ := json.Marshal(events.Event{
			Type: "liveview_click",
			Data: json.RawMessage(`{"x":50}`),
		})
		req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		reader := cs.NewReader(0)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		res, err := reader.Read(ctx)
		require.NoError(t, err)
		require.NotNil(t, res.Envelope)
		assert.Equal(t, events.CategoryLiveview, res.Envelope.Event.Category)

		// liveview.log should also exist
		_, statErr := os.Stat(filepath.Join(logDir, "liveview.log"))
		assert.NoError(t, statErr, "liveview.log should exist after category derivation")
	})
}
