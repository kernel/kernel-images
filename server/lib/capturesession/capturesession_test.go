package capturesession

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEventStream(t *testing.T, capacity int) *events.EventStream {
	t.Helper()
	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: capacity})
	require.NoError(t, err)
	return es
}

func newTestCaptureSession(t *testing.T) *CaptureSession {
	t.Helper()
	p := NewCaptureSession(newTestEventStream(t, 100))
	p.Start("test-session", CaptureConfig{})
	return p
}

func readEnvelope(t *testing.T, r *events.Reader, ctx context.Context) events.Envelope {
	t.Helper()
	res, err := r.Read(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Envelope, "expected envelope, got drop")
	return *res.Envelope
}

func cdpEvent(typ string, cat events.EventCategory) events.Event {
	return events.Event{Type: typ, Category: cat, Source: events.Source{Kind: events.KindCDP}}
}

func sessionIDFromMetadata(t *testing.T, src events.Source) string {
	t.Helper()
	id, ok := src.Metadata["capture_session_id"]
	require.True(t, ok, "capture_session_id not found in source.metadata")
	return id
}

func TestCaptureSession(t *testing.T) {
	t.Run("concurrent_publish_seq_order", func(t *testing.T) {
		const goroutines = 8
		const eventsEach = 50
		const total = goroutines * eventsEach

		p := NewCaptureSession(newTestEventStream(t, total))
		p.Start("test-concurrent", CaptureConfig{})
		reader := p.NewReader(0)

		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < eventsEach; j++ {
					p.Publish(cdpEvent("console.log", events.CategoryConsole))
				}
			}()
		}
		wg.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		for want := uint64(1); want <= total; want++ {
			env := readEnvelope(t, reader, ctx)
			assert.Equal(t, want, env.Seq, "events must arrive in seq order")
		}
	})

	t.Run("seq_continues_across_sessions", func(t *testing.T) {
		p := NewCaptureSession(newTestEventStream(t, 100))
		p.Start("session-1", CaptureConfig{})
		p.Publish(cdpEvent("ev.one", events.CategorySystem))
		p.Publish(cdpEvent("ev.two", events.CategorySystem))

		p.Start("session-2", CaptureConfig{})
		p.Publish(cdpEvent("ev.three", events.CategorySystem))

		assert.Equal(t, uint64(2), p.SessionStartSeq(), "session-2 starts after seq 2")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		reader := p.NewReader(2)
		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, uint64(3), env.Seq)
		assert.Equal(t, "session-2", sessionIDFromMetadata(t, env.Event.Source))
		assert.Equal(t, "ev.three", env.Event.Type)
	})

	t.Run("publish_increments_seq", func(t *testing.T) {
		p := newTestCaptureSession(t)
		reader := p.NewReader(0)

		for i := 0; i < 3; i++ {
			p.Publish(events.Event{Type: "page.navigation", Category: events.CategoryPage, Source: events.Source{Kind: events.KindCDP}, Ts: 1})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		for want := uint64(1); want <= 3; want++ {
			env := readEnvelope(t, reader, ctx)
			assert.Equal(t, want, env.Seq, "expected seq %d got %d", want, env.Seq)
		}
	})

	t.Run("publish_sets_ts", func(t *testing.T) {
		p := newTestCaptureSession(t)
		reader := p.NewReader(0)

		before := time.Now().UnixMicro()
		p.Publish(events.Event{Type: "page.navigation", Category: events.CategoryPage, Source: events.Source{Kind: events.KindCDP}})
		after := time.Now().UnixMicro()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.GreaterOrEqual(t, env.Event.Ts, before)
		assert.LessOrEqual(t, env.Event.Ts, after)
	})

	t.Run("publish_writes_ring", func(t *testing.T) {
		p := newTestCaptureSession(t)

		reader := p.NewReader(0)
		p.Publish(events.Event{Type: "page.navigation", Category: events.CategoryPage, Source: events.Source{Kind: events.KindCDP}, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "page.navigation", env.Event.Type)
		assert.Equal(t, events.CategoryPage, env.Event.Category)
	})

	t.Run("start_sets_capture_session_id_in_source_metadata", func(t *testing.T) {
		p := newTestCaptureSession(t)
		p.Start("test-uuid", CaptureConfig{})

		reader := p.NewReader(0)
		p.Publish(events.Event{Type: "page.navigation", Category: events.CategoryPage, Source: events.Source{Kind: events.KindCDP}, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "test-uuid", sessionIDFromMetadata(t, env.Event.Source))
	})

	t.Run("data_unchanged_when_session_id_in_metadata", func(t *testing.T) {
		p := newTestCaptureSession(t)
		p.Start("merge-session", CaptureConfig{})

		reader := p.NewReader(0)
		p.Publish(events.Event{
			Type:     "page.navigation",
			Category: events.CategoryPage,
			Source:   events.Source{Kind: events.KindCDP},
			Ts:       1,
			Data:     json.RawMessage(`{"url":"https://example.com"}`),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "merge-session", sessionIDFromMetadata(t, env.Event.Source))
		assert.JSONEq(t, `{"url":"https://example.com"}`, string(env.Event.Data))
	})

	t.Run("truncation_applied", func(t *testing.T) {
		p := newTestCaptureSession(t)
		reader := p.NewReader(0)

		largeData := strings.Repeat("x", 1_100_000)
		rawData, err := json.Marshal(map[string]string{"payload": largeData})
		require.NoError(t, err)

		p.Publish(events.Event{
			Type:     "page.navigation",
			Category: events.CategoryPage,
			Source:   events.Source{Kind: events.KindCDP},
			Ts:       1,
			Data:     json.RawMessage(rawData),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.True(t, env.Event.Truncated)
		assert.True(t, json.Valid(env.Event.Data))
	})
}

