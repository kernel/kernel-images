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
	t.Cleanup(func() { cs.Close() })
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

func requireLogFileExists(t *testing.T, logDir, filename string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(logDir, filename))
	require.NoError(t, err, "%s should exist", filename)
	require.Greater(t, info.Size(), int64(0), "%s should be non-empty", filename)
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
		require.Equal(t, http.StatusOK, w.Code)

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

		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "invalid JSON body")
	})

	t.Run("empty_body_rejected", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		req := httptest.NewRequest(http.MethodPost, "/events/publish", http.NoBody)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		svc.PublishEvent(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "invalid JSON body")
	})

	t.Run("empty_type_rejected", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		w := publishEvent(t, svc, events.Event{
			Category: events.CategoryConsole,
			Data:     json.RawMessage(`{"x":1}`),
		})
		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "type is required")
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
		requireLogFileExists(t, logDir, "liveview.log")
	})

	t.Run("source_kind_defaults_to_kernel_api", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())

		w := publishEvent(t, svc, events.Event{
			Type: "console_log",
			Data: json.RawMessage(`{"msg":"hi"}`),
		})
		require.Equal(t, http.StatusOK, w.Code)

		env := readEnvelope(t, cs)
		assert.Equal(t, events.KindKernelAPI, env.Event.Source.Kind)
	})

	t.Run("source_kind_preserved_when_set", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())

		w := publishEvent(t, svc, events.Event{
			Type:   "captcha_solve",
			Source: events.Source{Kind: events.KindExtension},
			Data:   json.RawMessage(`{"token":"abc"}`),
		})
		require.Equal(t, http.StatusOK, w.Code)

		env := readEnvelope(t, cs)
		assert.Equal(t, events.KindExtension, env.Event.Source.Kind)
	})

	t.Run("unknown_type_prefix_defaults_to_system", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())

		w := publishEvent(t, svc, events.Event{
			Type: "custom_something",
			Data: json.RawMessage(`{"k":"v"}`),
		})
		require.Equal(t, http.StatusOK, w.Code)

		env := readEnvelope(t, cs)
		assert.Equal(t, events.CategorySystem, env.Event.Category)
	})

	t.Run("nil_data_accepted", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())

		w := publishEvent(t, svc, events.Event{
			Type:     "page_load",
			Category: events.CategoryPage,
		})
		require.Equal(t, http.StatusOK, w.Code)

		env := readEnvelope(t, cs)
		assert.Equal(t, "page_load", env.Event.Type)
	})

	t.Run("routes_to_category_log_files", func(t *testing.T) {
		tests := []struct {
			name    string
			event   events.Event
			logFile string
		}{
			{
				name: "liveview",
				event: events.Event{
					Type:     "liveview_click",
					Category: events.CategoryLiveview,
					Source:   events.Source{Kind: events.KindKernelAPI},
					Data:     json.RawMessage(`{"x":100}`),
				},
				logFile: "liveview.log",
			},
			{
				name: "captcha",
				event: events.Event{
					Type:     "captcha_solve",
					Category: events.CategoryCaptcha,
					Source:   events.Source{Kind: events.KindKernelAPI},
					Data:     json.RawMessage(`{"token":"abc"}`),
				},
				logFile: "captcha.log",
			},
			{
				name: "network",
				event: events.Event{
					Type:     "network_request",
					Category: events.CategoryNetwork,
					Source:   events.Source{Kind: events.KindKernelAPI},
					Data:     json.RawMessage(`{"url":"https://example.com"}`),
				},
				logFile: "network.log",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				logDir := t.TempDir()
				svc, _ := newPublishTestService(t, logDir)

				w := publishEvent(t, svc, tt.event)
				require.Equal(t, http.StatusOK, w.Code)
				requireLogFileExists(t, logDir, tt.logFile)
			})
		}
	})
}
