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
	mu        sync.Mutex
	appended  []Envelope
	err       error
	errCount  int
}

func (m *mockBackend) Append(_ context.Context, env Envelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		m.errCount++
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

func (m *mockBackend) errors() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errCount
}

func (m *mockBackend) clearErr() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = nil
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

func TestStorageWriter_NormalAppend(t *testing.T) {
	es := newTestStream(t, 64)
	backend := &mockBackend{}
	w := NewStorageWriter(es, backend)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx) //nolint:errcheck
	}()

	env1 := es.Publish(Envelope{Event: makeEvent("test.one")})
	env2 := es.Publish(Envelope{Event: makeEvent("test.two")})

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

func TestStorageWriter_DroppedEvents(t *testing.T) {
	es := newTestStream(t, 4)
	backend := &mockBackend{}
	w := NewStorageWriter(es, backend)

	// Publish 8 events before the writer starts — fills and wraps the ring.
	for i := range 8 {
		es.Publish(Envelope{Event: makeEvent("drop.test." + string(rune('a'+i)))})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx) //nolint:errcheck
	}()

	require.Eventually(t, func() bool {
		return len(backend.envelopes()) > 0
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-done

	// With ring capacity 4 and 8 publishes, the writer must have skipped at
	// least 4 events via a drop gap — so fewer than 8 envelopes landed.
	got := backend.envelopes()
	assert.Less(t, len(got), 8, "expected fewer than 8 envelopes due to ring overflow")
	for _, env := range got {
		assert.NotEmpty(t, env.Event.Type)
	}
}

func TestStorageWriter_AppendError(t *testing.T) {
	es := newTestStream(t, 64)
	backend := &mockBackend{err: errors.New("storage unavailable")}
	w := NewStorageWriter(es, backend)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx) //nolint:errcheck
	}()

	// Publish an event that will fail. Wait until the writer has attempted it
	// (errCount > 0), then clear the error and publish a second event. The
	// writer must continue past the error and deliver the second event.
	es.Publish(Envelope{Event: makeEvent("will.fail")})
	require.Eventually(t, func() bool {
		return backend.errors() > 0
	}, time.Second, 5*time.Millisecond)

	backend.clearErr()
	es.Publish(Envelope{Event: makeEvent("will.succeed")})
	require.Eventually(t, func() bool {
		return len(backend.envelopes()) == 1
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-done

	got := backend.envelopes()
	require.Len(t, got, 1)
	assert.Equal(t, "will.succeed", got[0].Event.Type)
}

func TestStorageWriter_ContextCancelled(t *testing.T) {
	es := newTestStream(t, 64)
	backend := &mockBackend{}
	w := NewStorageWriter(es, backend)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := w.Run(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
