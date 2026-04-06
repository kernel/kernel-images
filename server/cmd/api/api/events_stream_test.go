package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
)

func TestStreamEvents(t *testing.T) {
	t.Run("delivers_events", func(t *testing.T) {
		logDir := t.TempDir()
		svc, cs := newPublishTestService(t, logDir)

		// Publish 2 events before streaming
		cs.Publish(events.Event{
			Type:     "console.log",
			Category: events.CategoryConsole,
			Source:   events.Source{Kind: events.KindCDP},
		})
		cs.Publish(events.Event{
			Type:     "console.log",
			Category: events.CategoryConsole,
			Source:   events.Source{Kind: events.KindCDP},
		})

		// Create a request context that cancels after a short window
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		req := httptest.NewRequest(http.MethodGet, "/events/stream", nil).WithContext(ctx)
		w := httptest.NewRecorder()

		svc.StreamEvents(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		body := w.Body.String()
		assert.Contains(t, body, "id: 1", "should contain event with seq 1")
		assert.Contains(t, body, "id: 2", "should contain event with seq 2")
	})

	t.Run("last_event_id_reconnect", func(t *testing.T) {
		logDir := t.TempDir()
		svc, cs := newPublishTestService(t, logDir)

		// Publish 3 events
		cs.Publish(events.Event{Type: "console.log", Category: events.CategoryConsole, Source: events.Source{Kind: events.KindCDP}})
		cs.Publish(events.Event{Type: "console.log", Category: events.CategoryConsole, Source: events.Source{Kind: events.KindCDP}})
		cs.Publish(events.Event{Type: "console.log", Category: events.CategoryConsole, Source: events.Source{Kind: events.KindCDP}})

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		req := httptest.NewRequest(http.MethodGet, "/events/stream", nil).WithContext(ctx)
		req.Header.Set("Last-Event-ID", "2")
		w := httptest.NewRecorder()

		svc.StreamEvents(w, req)

		body := w.Body.String()
		assert.Contains(t, body, "id: 3", "should contain event with seq 3")
		assert.NotContains(t, body, "id: 1", "should not re-send seq 1")
		assert.NotContains(t, body, "id: 2", "should not re-send seq 2")
	})

	t.Run("clean_disconnect", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		// Context already cancelled before calling handler
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		req := httptest.NewRequest(http.MethodGet, "/events/stream", nil).WithContext(ctx)
		w := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			svc.StreamEvents(w, req)
			close(done)
		}()

		select {
		case <-done:
			// Good: handler returned promptly
		case <-time.After(100 * time.Millisecond):
			t.Error("StreamEvents did not return promptly after context cancellation")
		}
	})

	t.Run("no_flusher_returns_500", func(t *testing.T) {
		logDir := t.TempDir()
		svc, _ := newPublishTestService(t, logDir)

		req := httptest.NewRequest(http.MethodGet, "/events/stream", nil)
		// Use a non-flusher ResponseWriter
		w := &nonFlusherWriter{header: make(http.Header)}

		svc.StreamEvents(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.code)
	})
}

// nonFlusherWriter is a ResponseWriter that does NOT implement http.Flusher.
type nonFlusherWriter struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (w *nonFlusherWriter) Header() http.Header        { return w.header }
func (w *nonFlusherWriter) WriteHeader(code int)       { w.code = code }
func (w *nonFlusherWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}
