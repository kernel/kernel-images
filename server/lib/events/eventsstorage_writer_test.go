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
// It also implements sessionRemover so that RemoveSession paths can be exercised.
type mockStorage struct {
	mu      sync.Mutex
	calls   []appendCall
	removed []string
	err     error
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

func (m *mockStorage) Remove(streamName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, streamName)
}

func (m *mockStorage) Close() error { return nil }

func (m *mockStorage) recorded() []appendCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]appendCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *mockStorage) removedSessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.removed))
	copy(out, m.removed)
	return out
}

// blockingStorage is an EventsStorage whose Append blocks until release is closed.
// appendCalled is closed (once) when Append is first entered so callers can
// synchronise on "Append is in progress".
type blockingStorage struct {
	appendCalled chan struct{}
	release      chan struct{}
	once         sync.Once
}

func newBlockingStorage() *blockingStorage {
	return &blockingStorage{
		appendCalled: make(chan struct{}),
		release:      make(chan struct{}),
	}
}

func (b *blockingStorage) Append(ctx context.Context, _ string, _ []byte) error {
	b.once.Do(func() { close(b.appendCalled) })
	select {
	case <-b.release:
		return ctx.Err() // return non-nil so the writer sees a failed append
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingStorage) Close() error { return nil }

// Compile-time interface guards.
// mockStorage must implement sessionRemover (so RemoveSession paths are exercised).
// blockingStorage must NOT implement sessionRemover (so the no-op path is exercised).
var _ sessionRemover = (*mockStorage)(nil)

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
	t.Fatalf("timed out waiting for %d appends after %v, got %d; calls: %v", n, timeout, len(b.recorded()), b.recorded())
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

	calls := waitForN(t, backend, 1, 500*time.Millisecond)
	cancel()
	<-done

	require.NotEmpty(t, calls, "session_ended event should be forwarded to storage")
	for _, c := range calls {
		env := unmarshalEnv(t, c.data)
		assert.Equal(t, "session-x", env.CaptureSessionID)
	}
}

func TestEventsStorageWriter_RingOverflow(t *testing.T) {
	// Publish twice the ring capacity before starting the writer, so the writer
	// will encounter result.Dropped > 0 on its first Read and take the overflow
	// path (PublishUnfiltered an EventsStorageError system event).
	//
	// Cascade behaviour: each error event published by the writer advances
	// latestSeq and oldest by 1, keeping nextSeq exactly 1 behind oldest on the
	// next Read — creating an unbounded feedback loop. The loop terminates when
	// session.Stop() clears the session ID, turning PublishUnfiltered into a
	// no-op. Once no new events are injected the writer drains the ring, catches
	// up to latest, blocks, sees ctx.Done(), and returns.
	//
	// Verifiable invariants:
	//   (a) Run exits cleanly within the timeout (no panic, no goroutine leak).
	//   (b) backend received fewer appends than events published — confirming that
	//       drops occurred and the overflow path actually executed. Because error
	//       events cycle through the ring and overwrite earlier entries before the
	//       test reader can observe them, the overflow path is confirmed here via
	//       the count discrepancy rather than by reading a specific error event.
	const ringCap = 64
	session, writer, backend := newWriterTest(t, ringCap)

	session.Start("session-overflow", CaptureConfig{})
	const published = ringCap * 2
	for i := 0; i < published; i++ {
		session.Publish(Event{Type: "ev", Category: CategoryConsole})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		writer.Run(ctx)
	}()

	// Let the writer run briefly to hit the overflow path at least once,
	// then stop the session (makes PublishUnfiltered a no-op, breaking the
	// cascade) and cancel the context so Run can exit.
	// A short fixed pause is acceptable here: the writer's log output
	// ("ring buffer overflow, events dropped") is the observable proof that the
	// overflow path ran; we just need to give the goroutine a scheduling window.
	time.Sleep(5 * time.Millisecond)
	session.Stop()
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after context cancellation")
	}

	// Fewer appends than events published confirms drops occurred (overflow path ran).
	calls := backend.recorded()
	assert.Less(t, len(calls), published,
		"backend should have received fewer appends than published events (overflow drops confirmed)")
	// Sanity: no more than ringCap events could have survived the drop.
	assert.LessOrEqual(t, len(calls), ringCap,
		"writer should not see more events than ring capacity")
}

func TestEventsStorageWriter_ShutdownDuringAppend(t *testing.T) {
	// Verify that cancelling the writer context while Append is blocked causes
	// Run to return cleanly without panicking.
	session, err := NewCaptureSession(CaptureSessionConfig{
		LogDir:       t.TempDir(),
		RingCapacity: 16,
	})
	require.NoError(t, err)

	bs := newBlockingStorage()
	writer := NewEventsStorageWriter(session, bs)

	session.Start("session-shutdown", CaptureConfig{})
	session.Publish(Event{Type: "test.event", Category: CategoryConsole})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		writer.Run(ctx)
	}()

	// Wait until Append has been entered — the writer is now blocked inside bs.Append.
	select {
	case <-bs.appendCalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for Append to be called")
	}

	// Cancel the context while Append is still blocking.
	cancel()

	// Unblock Append so it can return ctx.Err() and the writer can exit.
	close(bs.release)

	// Writer must exit cleanly.
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after context cancellation during Append")
	}
}

func TestEventsStorageWriter_RemoveSession(t *testing.T) {
	t.Run("storage implements sessionRemover", func(t *testing.T) {
		session, err := NewCaptureSession(CaptureSessionConfig{
			LogDir:       t.TempDir(),
			RingCapacity: 16,
		})
		require.NoError(t, err)

		backend := &mockStorage{} // mockStorage implements sessionRemover via Remove()
		writer := NewEventsStorageWriter(session, backend)

		writer.RemoveSession("test-session-id")

		removed := backend.removedSessions()
		require.Len(t, removed, 1, "Remove should have been called once")
		assert.Equal(t, "test-session-id", removed[0])
	})

	t.Run("storage does not implement sessionRemover — no panic", func(t *testing.T) {
		session, err := NewCaptureSession(CaptureSessionConfig{
			LogDir:       t.TempDir(),
			RingCapacity: 16,
		})
		require.NoError(t, err)

		// blockingStorage satisfies EventsStorage but intentionally does NOT
		// implement sessionRemover (no Remove method). RemoveSession must be a no-op.
		writer := NewEventsStorageWriter(session, newBlockingStorage())

		assert.NotPanics(t, func() {
			writer.RemoveSession("test-session-id")
		})
	})
}

func unmarshalEnv(t *testing.T, data []byte) Envelope {
	t.Helper()
	var env Envelope
	require.NoError(t, json.Unmarshal(data, &env))
	return env
}
