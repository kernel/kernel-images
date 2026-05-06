package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockStorage is an in-memory EventsStorage for writer tests. No S2 dependency.
type mockStorage struct {
	mu    sync.Mutex
	calls []appendCall
	err   error
}

type appendCall struct {
	streamName string
	data       []byte
}

func (m *mockStorage) Append(_ context.Context, streamName string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.calls = append(m.calls, appendCall{streamName: streamName, data: cp})
	return nil
}

func (m *mockStorage) Close() error { return nil }

func (m *mockStorage) recorded() []appendCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]appendCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// waitForN polls until at least n appends are recorded or the timeout elapses.
func waitForN(t *testing.T, b *mockStorage, n int, timeout time.Duration) []appendCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if calls := b.recorded(); len(calls) >= n {
			return calls
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d appends, got %d", n, len(b.recorded()))
	return nil
}

func newWriterTest(t *testing.T, ringCap int) (*CaptureSession, *EventsStorageWriter, *mockStorage) {
	t.Helper()
	session, err := NewCaptureSession(CaptureSessionConfig{
		LogDir:       t.TempDir(),
		RingCapacity: ringCap,
	})
	require.NoError(t, err)
	backend := &mockStorage{}
	writer := NewEventsStorageWriter(session, backend)
	return session, writer, backend
}

func startWriter(writer *EventsStorageWriter) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		writer.Run(ctx)
	}()
	return cancel, done
}

func TestEventsStorageWriter_NormalAppend(t *testing.T) {
	session, writer, backend := newWriterTest(t, 64)
	cancel, done := startWriter(writer)
	defer func() { cancel(); <-done }()

	session.Start("session-abc", CaptureConfig{})
	session.Publish(Event{Type: "test.event", Category: CategoryConsole})
	session.Publish(Event{Type: "test.event2", Category: CategoryNetwork})

	calls := waitForN(t, backend, 2, 200*time.Millisecond)
	for _, c := range calls {
		assert.Equal(t, "session-abc", c.streamName)
		var env Envelope
		require.NoError(t, json.Unmarshal(c.data, &env))
		assert.Equal(t, "session-abc", env.CaptureSessionID)
	}
	assert.Equal(t, "test.event", unmarshalEnv(t, calls[0].data).Event.Type)
	assert.Equal(t, "test.event2", unmarshalEnv(t, calls[1].data).Event.Type)
}

func TestEventsStorageWriter_ContextCancel(t *testing.T) {
	_, writer, _ := newWriterTest(t, 16)
	cancel, done := startWriter(writer)

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestEventsStorageWriter_AppendError(t *testing.T) {
	session, writer, backend := newWriterTest(t, 64)
	backend.err = errors.New("s2 unavailable")
	cancel, done := startWriter(writer)
	defer func() { cancel(); <-done }()

	session.Start("session-err", CaptureConfig{})
	session.Publish(Event{Type: "test.event", Category: CategoryConsole})

	// Wait for the error event to appear in the ring (published via PublishUnfiltered).
	// We can detect it by checking the ring: stop the session and look at what was published.
	// Use a small reader to observe the ring directly.
	reader := session.NewReader(0)
	ctx, timeoutCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer timeoutCancel()

	var found bool
	for !found {
		res, err := reader.Read(ctx)
		require.NoError(t, err)
		if res.Dropped > 0 {
			continue
		}
		if res.Envelope.Event.Type == EventsStorageError {
			found = true
		}
	}
	assert.True(t, found, "expected system_durable_error event in ring")
	// The storage should have received no successful appends.
	assert.Empty(t, backend.recorded())
}

func TestEventsStorageWriter_EventsStorageErrorSkipped(t *testing.T) {
	session, writer, backend := newWriterTest(t, 64)
	cancel, done := startWriter(writer)
	defer func() { cancel(); <-done }()

	session.Start("session-skip", CaptureConfig{})
	// Inject a system_durable_error event directly. The writer must not re-append it.
	session.PublishUnfiltered(Event{
		Type:     EventsStorageError,
		Category: CategorySystem,
		Source:   Source{Kind: KindLocalProcess},
	})
	// Also publish a normal event so we have a signal the writer processed the above.
	session.Publish(Event{Type: "after.error", Category: CategoryConsole})

	calls := waitForN(t, backend, 1, 200*time.Millisecond)
	for _, c := range calls {
		env := unmarshalEnv(t, c.data)
		assert.NotEqual(t, EventsStorageError, env.Event.Type,
			"system_durable_error must not be forwarded to storage")
	}
}

func TestEventsStorageWriter_SessionEndedForwarded(t *testing.T) {
	session, writer, backend := newWriterTest(t, 64)
	cancel, done := startWriter(writer)

	session.Start("session-x", CaptureConfig{})
	session.Stop() // emits session_ended before clearing the session ID

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	calls := backend.recorded()
	require.NotEmpty(t, calls, "session_ended event should be forwarded to storage")
	for _, c := range calls {
		env := unmarshalEnv(t, c.data)
		assert.Equal(t, "session-x", env.CaptureSessionID)
	}
}

func TestEventsStorageWriter_RingOverflow(t *testing.T) {
	// Use ring capacity of 4; publish 8 events before starting the writer.
	session, writer, backend := newWriterTest(t, 4)

	session.Start("session-overflow", CaptureConfig{})
	for i := 0; i < 8; i++ {
		session.Publish(Event{Type: "ev", Category: CategoryConsole})
	}

	cancel, done := startWriter(writer)
	defer func() { cancel(); <-done }()

	// Wait for writer to drain whatever is available (at most 4 events).
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// Writer must not have crashed; it should have processed the available events.
	calls := backend.recorded()
	assert.LessOrEqual(t, len(calls), 4, "writer should not see more events than ring capacity")
}

func unmarshalEnv(t *testing.T, data []byte) Envelope {
	t.Helper()
	var env Envelope
	require.NoError(t, json.Unmarshal(data, &env))
	return env
}
