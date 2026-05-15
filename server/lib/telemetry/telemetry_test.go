package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEventStream(t *testing.T, capacity int) *events.EventStream {
	t.Helper()
	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: capacity})
	require.NoError(t, err)
	return es
}

func newTestTelemetrySession(t *testing.T) *TelemetrySession {
	t.Helper()
	ts := NewTelemetrySession(newTestEventStream(t, 100))
	ts.Start("test-session", TelemetryConfig{})
	return ts
}

func readEnvelope(t *testing.T, r *events.Reader, ctx context.Context) events.Envelope {
	t.Helper()
	res, err := r.Read(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Envelope, "expected envelope, got drop")
	return *res.Envelope
}

func cdpEvent(typ string, cat oapi.TelemetryEventCategory) events.Event {
	return events.Event{Type: typ, Category: cat, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}}
}

func telemetrySessionIDFromMetadata(t *testing.T, src oapi.BrowserEventSource) string {
	t.Helper()
	require.NotNil(t, src.Metadata, "source.metadata is nil")
	id, ok := (*src.Metadata)["telemetry_session_id"]
	require.True(t, ok, "telemetry_session_id not found in source.metadata")
	return id
}

func TestTelemetrySession(t *testing.T) {
	t.Run("concurrent_publish_seq_order", func(t *testing.T) {
		const goroutines = 8
		const eventsEach = 50
		const total = goroutines * eventsEach

		ts := NewTelemetrySession(newTestEventStream(t, total))
		ts.Start("test-concurrent", TelemetryConfig{})
		reader := ts.NewReader(0)

		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < eventsEach; j++ {
					ts.Publish(cdpEvent("console.log", events.Console))
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
		ts := NewTelemetrySession(newTestEventStream(t, 100))
		ts.Start("session-1", TelemetryConfig{})
		ts.Publish(cdpEvent("ev.one", events.System))
		ts.Publish(cdpEvent("ev.two", events.System))

		ts.Start("session-2", TelemetryConfig{})
		ts.Publish(cdpEvent("ev.three", events.System))

		assert.Equal(t, uint64(2), ts.SessionStartSeq(), "session-2 starts after seq 2")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		reader := ts.NewReader(2)
		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, uint64(3), env.Seq)
		assert.Equal(t, "session-2", telemetrySessionIDFromMetadata(t, env.Event.Source))
		assert.Equal(t, "ev.three", env.Event.Type)
	})

	t.Run("publish_increments_seq", func(t *testing.T) {
		ts := newTestTelemetrySession(t)
		reader := ts.NewReader(0)

		for i := 0; i < 3; i++ {
			ts.Publish(events.Event{Type: "page.navigation", Category: events.Page, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}, Ts: 1})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		for want := uint64(1); want <= 3; want++ {
			env := readEnvelope(t, reader, ctx)
			assert.Equal(t, want, env.Seq, "expected seq %d got %d", want, env.Seq)
		}
	})

	t.Run("publish_sets_ts", func(t *testing.T) {
		ts := newTestTelemetrySession(t)
		reader := ts.NewReader(0)

		before := time.Now().UnixMicro()
		ts.Publish(events.Event{Type: "page.navigation", Category: events.Page, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}})
		after := time.Now().UnixMicro()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.GreaterOrEqual(t, env.Event.Ts, before)
		assert.LessOrEqual(t, env.Event.Ts, after)
	})

	t.Run("publish_writes_ring", func(t *testing.T) {
		ts := newTestTelemetrySession(t)

		reader := ts.NewReader(0)
		ts.Publish(events.Event{Type: "page.navigation", Category: events.Page, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "page.navigation", env.Event.Type)
		assert.Equal(t, events.Page, env.Event.Category)
	})

	t.Run("start_sets_telemetry_session_id_in_source_metadata", func(t *testing.T) {
		ts := newTestTelemetrySession(t)
		ts.Start("test-uuid", TelemetryConfig{})

		reader := ts.NewReader(0)
		ts.Publish(events.Event{Type: "page.navigation", Category: events.Page, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "test-uuid", telemetrySessionIDFromMetadata(t, env.Event.Source))
	})

	t.Run("data_unchanged_when_telemetry_session_id_in_metadata", func(t *testing.T) {
		ts := newTestTelemetrySession(t)
		ts.Start("merge-session", TelemetryConfig{})

		reader := ts.NewReader(0)
		ts.Publish(events.Event{
			Type:     "page.navigation",
			Category: events.Page,
			Source:   oapi.BrowserEventSource{Kind: oapi.Cdp},
			Ts:       1,
			Data:     json.RawMessage(`{"url":"https://example.com"}`),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, "merge-session", telemetrySessionIDFromMetadata(t, env.Event.Source))
		assert.JSONEq(t, `{"url":"https://example.com"}`, string(env.Event.Data))
	})

	t.Run("system_events_always_captured_regardless_of_config", func(t *testing.T) {
		ts := NewTelemetrySession(newTestEventStream(t, 10))
		// Start with only console category — system should still pass through.
		ts.Start("sys-test", TelemetryConfig{Categories: []oapi.TelemetryEventCategory{events.Console}})
		reader := ts.NewReader(0)

		ts.Publish(events.Event{Type: "monitor.disconnected", Category: events.System, Source: oapi.BrowserEventSource{Kind: oapi.KernelApi}, Ts: 1})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		env := readEnvelope(t, reader, ctx)
		assert.Equal(t, events.System, env.Event.Category)
	})

	t.Run("truncation_applied", func(t *testing.T) {
		ts := newTestTelemetrySession(t)
		reader := ts.NewReader(0)

		largeData := strings.Repeat("x", 1_100_000)
		rawData, err := json.Marshal(map[string]string{"payload": largeData})
		require.NoError(t, err)

		ts.Publish(events.Event{
			Type:     "page.navigation",
			Category: events.Page,
			Source:   oapi.BrowserEventSource{Kind: oapi.Cdp},
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
