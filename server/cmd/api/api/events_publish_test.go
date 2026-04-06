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
	cs.Start("test-capture")
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

func publishEvent(t *testing.T, svc *ApiService, ev events.Event) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(ev)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	svc.PublishEvent(w, req)
	return w
}

func readEnvelope(t *testing.T, cs *events.CaptureSession) events.Envelope {
	t.Helper()
	reader := cs.NewReader(0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := reader.Read(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Envelope)
	return *res.Envelope
}

func assertLogFileExists(t *testing.T, logDir, filename string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(logDir, filename))
	require.NoError(t, err, "%s should exist", filename)
	assert.Greater(t, info.Size(), int64(0), "%s should be non-empty", filename)
}

func TestPublishEvent(t *testing.T) {
	t.Run("valid_event_published_to_ring", func(t *testing.T) {
		logDir := t.TempDir()
		svc, cs := newPublishTestService(t, logDir)

		w := publishEvent(t, svc, events.Event{
			Type:     "liveview_click",
			Category: events.CategoryLiveview,
			Source:   events.Source{Kind: events.KindKernelAPI},
			Data:     json.RawMessage(`{"x":100}`),
		})
		assert.Equal(t, http.StatusOK, w.Code)

		env := readEnvelope(t, cs)
		assert.Equal(t, "liveview_click", env.Event.Type)
		assert.Equal(t, events.CategoryLiveview, env.Event.Category)
	})

	t.Run("invalid_json", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		req := httptest.NewRequest(http.MethodPost, "/events/publish", bytes.NewReader([]byte(`not-json`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("empty_type_rejected", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		w := publishEvent(t, svc, events.Event{
			Category: events.CategoryConsole,
			Data:     json.RawMessage(`{"x":1}`),
		})
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("liveview_routes_to_log", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		w := publishEvent(t, svc, events.Event{
			Type:     "liveview_click",
			Category: events.CategoryLiveview,
			Source:   events.Source{Kind: events.KindKernelAPI},
			Data:     json.RawMessage(`{"x":100}`),
		})
		require.Equal(t, http.StatusOK, w.Code)
		assertLogFileExists(t, logDir, "liveview.log")
	})

	t.Run("captcha_routes_to_log", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		w := publishEvent(t, svc, events.Event{
			Type:     "captcha_solve",
			Category: events.CategoryCaptcha,
			Source:   events.Source{Kind: events.KindKernelAPI},
			Data:     json.RawMessage(`{"token":"abc"}`),
		})
		require.Equal(t, http.StatusOK, w.Code)
		assertLogFileExists(t, logDir, "captcha.log")
	})

	t.Run("category_derived_from_type", func(t *testing.T) {
		logDir := t.TempDir()
		svc, cs := newPublishTestService(t, logDir)

		w := publishEvent(t, svc, events.Event{
			Type: "liveview_click",
			Data: json.RawMessage(`{"x":50}`),
		})
		require.Equal(t, http.StatusOK, w.Code)

		env := readEnvelope(t, cs)
		assert.Equal(t, events.CategoryLiveview, env.Event.Category)
		assertLogFileExists(t, logDir, "liveview.log")
	})
}
