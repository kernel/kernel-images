package events

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storageControllers builds each storage controller over the same stream
// resolver, so both sinks are held to one lifecycle contract.
func storageControllers(t *testing.T) map[string]func(es *EventStream, streamFn func() string) *S2StorageController {
	relay := newFakeRelay(t)
	return map[string]func(*EventStream, func() string) *S2StorageController{
		"s2": func(es *EventStream, streamFn func() string) *S2StorageController {
			return NewS2StorageController(es, "test-basin", "test-token", streamFn, S2Config{}, slog.Default())
		},
		"relay": func(es *EventStream, streamFn func() string) *S2StorageController {
			return NewRelayStorageController(es, testRelayConfig(relay.srv.URL), streamFn, slog.Default())
		},
	}
}

func TestStorageControllers_Lifecycle(t *testing.T) {
	for name, build := range storageControllers(t) {
		t.Run(name, func(t *testing.T) {
			var resolved atomic.Int32
			c := build(newTestStream(t, 64), func() string {
				resolved.Add(1)
				return "test-stream"
			})
			assert.Zero(t, resolved.Load(), "nothing resolves before Start")
			assert.False(t, c.Running())

			require.NoError(t, c.Start(context.Background(), 0))
			first := c.writer
			require.NoError(t, c.Start(context.Background(), 0))
			assert.Same(t, first, c.writer, "a second Start keeps the writer")
			assert.Equal(t, int32(1), resolved.Load())
			assert.True(t, c.Running())

			require.NoError(t, stopController(t, c))
			assert.False(t, c.Running())
			assert.True(t, c.EverStarted(), "EverStarted survives Stop")
		})
	}
}

// A fork resolves no stream until its identity arrives, and storage must stay
// closed until then.
func TestStorageControllers_ForkGate(t *testing.T) {
	for name, build := range storageControllers(t) {
		t.Run(name, func(t *testing.T) {
			var stream atomic.Pointer[string]
			empty := ""
			stream.Store(&empty)
			c := build(newTestStream(t, 64), func() string { return *stream.Load() })

			require.NoError(t, c.Start(context.Background(), 0))
			assert.False(t, c.EverStarted())

			applied := "fork-stream"
			stream.Store(&applied)
			require.NoError(t, c.Start(context.Background(), 0))
			assert.True(t, c.EverStarted())
			require.NoError(t, stopController(t, c))
		})
	}
}

func TestStorageControllers_CancelledStartRollsBack(t *testing.T) {
	for name, build := range storageControllers(t) {
		t.Run(name, func(t *testing.T) {
			c := build(newTestStream(t, 64), func() string { return "test-stream" })
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			require.ErrorIs(t, c.Start(ctx, 0), context.Canceled)
			assert.False(t, c.EverStarted())
		})
	}
}

func TestRelayStorageController_ForwardsEventsAfterStart(t *testing.T) {
	relay := newFakeRelay(t)
	es := newTestStream(t, 64)
	es.Publish(Envelope{Event: makeEvent("captured before storage")})
	c := NewRelayStorageController(es, testRelayConfig(relay.srv.URL), func() string { return "s" }, slog.Default())
	require.NoError(t, c.Start(context.Background(), es.Seq()))
	es.Publish(Envelope{Event: makeEvent("stored")})
	require.Eventually(t, func() bool { return len(relay.records()) == 1 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, stopController(t, c))
	assert.Len(t, relay.records(), 1)
}
