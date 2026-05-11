package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockBackend struct {
	mu       sync.Mutex
	appended []Envelope
	err      error
}

func (m *mockBackend) Append(_ context.Context, env Envelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.appended = append(m.appended, env)
	return nil
}

func (m *mockBackend) Close() error { return nil }

func (m *mockBackend) envelopes() []Envelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Envelope, len(m.appended))
	copy(out, m.appended)
	return out
}

func newTestStream(t *testing.T, capacity int) *EventStream {
	t.Helper()
	es, err := NewEventStream(EventStreamConfig{RingCapacity: capacity})
	require.NoError(t, err)
	return es
}

func makeEvent(typ string) Event {
	return Event{Type: typ, Category: CategorySystem}
}

func TestEventsStorageWriter_NormalAppend(t *testing.T) {
	es := newTestStream(t, 64)
	backend := &mockBackend{}
	w := NewEventsStorageWriter(es, backend)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx) //nolint:errcheck
	}()

	env1 := es.Publish(Envelope{Event: makeEvent("test.one")})
	env2 := es.Publish(Envelope{Event: makeEvent("test.two")})

	// wait for both to land
	require.Eventually(t, func() bool {
		return len(backend.envelopes()) == 2
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-done

	got := backend.envelopes()
	assert.Equal(t, env1.Seq, got[0].Seq)
	assert.Equal(t, "test.one", got[0].Event.Type)
	assert.Equal(t, env2.Seq, got[1].Seq)
	assert.Equal(t, "test.two", got[1].Event.Type)
}

func TestEventsStorageWriter_DroppedEvents(t *testing.T) {
	// Use a tiny ring so overflow is easy to trigger
	es := newTestStream(t, 4)
	backend := &mockBackend{}
	w := NewEventsStorageWriter(es, backend)

	// Publish 8 events without a reader running — fills and wraps the ring
	for i := range 8 {
		es.Publish(Envelope{Event: makeEvent("drop.test." + string(rune('a'+i)))})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx) //nolint:errcheck
	}()

	// Let the writer drain what's available
	require.Eventually(t, func() bool {
		return len(backend.envelopes()) > 0
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-done
	// Writer should not have crashed; any envelopes it did receive are valid
	for _, env := range backend.envelopes() {
		assert.NotEmpty(t, env.Event.Type)
	}
}

func TestEventsStorageWriter_AppendError(t *testing.T) {
	es := newTestStream(t, 64)
	backend := &mockBackend{err: errors.New("storage unavailable")}
	w := NewEventsStorageWriter(es, backend)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx) //nolint:errcheck
	}()

	es.Publish(Envelope{Event: makeEvent("will.fail")})

	// Give the writer a moment to process then cancel — it must not crash
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
}

func TestEventsStorageWriter_ContextCancelled(t *testing.T) {
	es := newTestStream(t, 64)
	backend := &mockBackend{}
	w := NewEventsStorageWriter(es, backend)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := w.Run(ctx)
	assert.NoError(t, err)
}
