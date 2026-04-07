package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestEvent() events.Event {
	return events.Event{
		Type:     "console_log",
		Category: events.CategoryConsole,
		Source:   events.Source{Kind: events.KindCDP},
	}
}

func timedStreamRequest(t *testing.T, timeout time.Duration) (*httptest.ResponseRecorder, *http.Request, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events/stream", nil).WithContext(ctx)
	return w, req, cancel
}

// sseFrameRe matches a valid SSE frame: "id: <number>\ndata: <json>\n\n"
var sseFrameRe = regexp.MustCompile(`id: (\d+)\ndata: (\{.*\})\n\n`)

func TestStreamEvents(t *testing.T) {
	t.Run("delivers_buffered_events", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())
		cs.Publish(makeTestEvent())
		cs.Publish(makeTestEvent())

		w, req, cancel := timedStreamRequest(t, 2*time.Second)
		defer cancel()

		svc.StreamEvents(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
		assert.Equal(t, "no", w.Header().Get("X-Accel-Buffering"))

		frames := sseFrameRe.FindAllStringSubmatch(w.Body.String(), -1)
		require.Len(t, frames, 2)
		assert.Equal(t, "1", frames[0][1])
		assert.Equal(t, "2", frames[1][1])
	})

	t.Run("resumes_after_last_event_id", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())
		cs.Publish(makeTestEvent())
		cs.Publish(makeTestEvent())
		cs.Publish(makeTestEvent())

		w, req, cancel := timedStreamRequest(t, 2*time.Second)
		defer cancel()
		req.Header.Set("Last-Event-ID", "2")

		svc.StreamEvents(w, req)

		frames := sseFrameRe.FindAllStringSubmatch(w.Body.String(), -1)
		require.Len(t, frames, 1)
		assert.Equal(t, "3", frames[0][1])
	})

	t.Run("invalid_last_event_id_starts_from_zero", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())
		cs.Publish(makeTestEvent())

		w, req, cancel := timedStreamRequest(t, 2*time.Second)
		defer cancel()
		req.Header.Set("Last-Event-ID", "garbage")

		svc.StreamEvents(w, req)

		frames := sseFrameRe.FindAllStringSubmatch(w.Body.String(), -1)
		require.Len(t, frames, 1, "invalid Last-Event-ID should fall back to seq 0")
		assert.Equal(t, "1", frames[0][1])
	})

	t.Run("skips_dropped_events_on_ring_overflow", func(t *testing.T) {
		// Ring buffer capacity is 16 (from newPublishTestService).
		// Publishing 20 events overflows the ring; the reader should
		// skip nil-envelope results (dropped events) without hanging.
		svc, cs := newPublishTestService(t, t.TempDir())
		for range 20 {
			cs.Publish(makeTestEvent())
		}

		w, req, cancel := timedStreamRequest(t, 2*time.Second)
		defer cancel()

		svc.StreamEvents(w, req)

		frames := sseFrameRe.FindAllStringSubmatch(w.Body.String(), -1)
		// Ring capacity is 16; 20 publishes means 4 evicted, 16 surviving.
		require.Len(t, frames, 16)
	})

	t.Run("sse_frame_contains_valid_json", func(t *testing.T) {
		svc, cs := newPublishTestService(t, t.TempDir())
		cs.Publish(makeTestEvent())

		w, req, cancel := timedStreamRequest(t, 2*time.Second)
		defer cancel()

		svc.StreamEvents(w, req)

		frames := sseFrameRe.FindAllStringSubmatch(w.Body.String(), -1)
		require.Len(t, frames, 1)

		var env events.Envelope
		require.NoError(t, json.Unmarshal([]byte(frames[0][2]), &env))
		assert.Equal(t, uint64(1), env.Seq)
		assert.Equal(t, "console_log", env.Event.Type)
		assert.Equal(t, "test-capture", env.CaptureSessionID)
	})

	t.Run("exits_on_cancelled_context", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/events/stream", nil).WithContext(ctx)

		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()

		done := make(chan struct{})
		go func() {
			svc.StreamEvents(w, req)
			close(done)
		}()

		select {
		case <-done:
		case <-timer.C:
			t.Fatal("StreamEvents did not return after context cancellation")
		}
	})

	t.Run("rejects_non_flusher", func(t *testing.T) {
		svc, _ := newPublishTestService(t, t.TempDir())

		req := httptest.NewRequest(http.MethodGet, "/events/stream", nil)
		w := &nonFlusherWriter{header: make(http.Header)}

		svc.StreamEvents(w, req)

		require.Equal(t, http.StatusInternalServerError, w.code)
		assert.Contains(t, w.body.String(), "streaming not supported")
	})
}

type nonFlusherWriter struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (w *nonFlusherWriter) Header() http.Header        { return w.header }
func (w *nonFlusherWriter) WriteHeader(code int)        { w.code = code }
func (w *nonFlusherWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
