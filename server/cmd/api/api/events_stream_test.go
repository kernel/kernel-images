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

var testEvent = events.Event{
	Type:     "console_log",
	Category: events.CategoryConsole,
	Source:   events.Source{Kind: events.KindCDP},
}

func streamRequest(ctx context.Context) (*httptest.ResponseRecorder, *http.Request) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events/stream", nil).WithContext(ctx)
	return w, req
}

func TestStreamEvents(t *testing.T) {
	t.Run("delivers_buffered_events", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())
		cs.Publish(testEvent)
		cs.Publish(testEvent)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		w, req := streamRequest(ctx)

		svc.StreamEvents(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		body := w.Body.String()
		assert.Contains(t, body, "id: 1")
		assert.Contains(t, body, "id: 2")
	})

	t.Run("resumes_after_last_event_id", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())
		cs.Publish(testEvent)
		cs.Publish(testEvent)
		cs.Publish(testEvent)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		w, req := streamRequest(ctx)
		req.Header.Set("Last-Event-ID", "2")

		svc.StreamEvents(w, req)

		body := w.Body.String()
		assert.Contains(t, body, "id: 3")
		assert.NotContains(t, body, "id: 1")
		assert.NotContains(t, body, "id: 2")
	})

	t.Run("exits_on_cancelled_context", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w, req := streamRequest(ctx)

		done := make(chan struct{})
		go func() {
			svc.StreamEvents(w, req)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			t.Error("StreamEvents did not return after context cancellation")
		}
	})

	t.Run("rejects_non_flusher", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		req := httptest.NewRequest(http.MethodGet, "/events/stream", nil)
		w := &nonFlusherWriter{header: make(http.Header)}

		svc.StreamEvents(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.code)
	})
}

type nonFlusherWriter struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (w *nonFlusherWriter) Header() http.Header         { return w.header }
func (w *nonFlusherWriter) WriteHeader(code int)         { w.code = code }
func (w *nonFlusherWriter) Write(b []byte) (int, error)  { return w.body.Write(b) }
