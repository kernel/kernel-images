package events

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestController(t *testing.T, stream string) (*S2StorageController, *atomic.Int32) {
	t.Helper()
	var resolved atomic.Int32
	c := NewS2StorageController(newTestStream(t, 64), "test-basin", "test-token", func() string {
		resolved.Add(1)
		return stream
	}, S2Config{}, slog.Default())
	return c, &resolved
}

func stopController(t *testing.T, c *S2StorageController) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Stop(ctx)
}

func TestS2StorageController_OpensNothingUntilStart(t *testing.T) {
	c, resolved := newTestController(t, "test-stream")

	assert.Zero(t, resolved.Load(), "stream name must not be resolved before Start")
	assert.False(t, c.Running())
	assert.False(t, c.EverStarted())
}

func TestS2StorageController_StartIsIdempotent(t *testing.T) {
	c, resolved := newTestController(t, "test-stream")

	require.NoError(t, c.Start(context.Background(), 0))
	first := c.writer
	require.NotNil(t, first)

	require.NoError(t, c.Start(context.Background(), 0))
	assert.Same(t, first, c.writer, "second Start must not replace the writer")
	assert.Equal(t, int32(1), resolved.Load(), "second Start must not re-resolve the stream")

	require.NoError(t, stopController(t, c))
}

func TestS2StorageController_ConcurrentStartOpensOneWriter(t *testing.T) {
	c, resolved := newTestController(t, "test-stream")

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Start(context.Background(), 0)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	assert.True(t, c.Running())
	assert.True(t, c.EverStarted())
	assert.Equal(t, int32(1), resolved.Load())

	require.NoError(t, stopController(t, c))
}

func TestS2StorageController_ConcurrentStartReturnsFailure(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	c := NewS2StorageController(newTestStream(t, 64), "test-basin", "test-token", func() string {
		close(entered)
		<-release
		return "test-stream"
	}, S2Config{}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	firstDone := make(chan error, 1)
	go func() { firstDone <- c.Start(ctx, 0) }()
	<-entered

	secondDone := make(chan error, 1)
	go func() { secondDone <- c.Start(context.Background(), 0) }()
	select {
	case err := <-secondDone:
		t.Fatalf("concurrent Start returned before the in-flight start finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	require.ErrorIs(t, <-firstDone, context.Canceled)
	require.ErrorIs(t, <-secondDone, context.Canceled)
	assert.False(t, c.Running())
	assert.False(t, c.EverStarted())
}

func TestS2StorageController_StartFailureRollsBack(t *testing.T) {
	c, resolved := newTestController(t, "test-stream")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, c.Start(ctx, 0), context.Canceled)
	assert.False(t, c.Running())
	assert.False(t, c.EverStarted())
	assert.Equal(t, int32(1), resolved.Load())

	require.NoError(t, c.Start(context.Background(), 0))
	assert.True(t, c.Running())
	assert.True(t, c.EverStarted())
	assert.Equal(t, int32(2), resolved.Load())

	require.NoError(t, stopController(t, c))
}

func TestS2StorageController_FailedStartDoesNotLogEnabled(t *testing.T) {
	var logs bytes.Buffer
	c := NewS2StorageController(newTestStream(t, 64), "test-basin", "test-token", func() string {
		return "test-stream"
	}, S2Config{}, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, c.Start(ctx, 0), context.Canceled)
	assert.NotContains(t, logs.String(), "S2 storage enabled")
}

func TestS2StorageController_SuccessfulStartLogsEnabled(t *testing.T) {
	var logs bytes.Buffer
	c := NewS2StorageController(newTestStream(t, 64), "test-basin", "test-token", func() string {
		return "test-stream"
	}, S2Config{}, slog.New(slog.NewTextHandler(&logs, nil)))

	require.NoError(t, c.Start(context.Background(), 0))
	assert.Contains(t, logs.String(), "S2 storage enabled")

	require.NoError(t, stopController(t, c))
}

func TestS2StorageController_EmptyStreamDoesNotStart(t *testing.T) {
	c, resolved := newTestController(t, "")

	require.NoError(t, c.Start(context.Background(), 0))
	assert.Equal(t, int32(1), resolved.Load())
	assert.False(t, c.Running())
	assert.False(t, c.EverStarted())
}

func TestS2StorageController_MissingCredentialsDoesNotResolveStream(t *testing.T) {
	var resolved atomic.Int32
	c := NewS2StorageController(newTestStream(t, 64), "", "", func() string {
		resolved.Add(1)
		return "test-stream"
	}, S2Config{}, slog.Default())

	require.NoError(t, c.Start(context.Background(), 0))
	assert.Zero(t, resolved.Load())
	assert.False(t, c.Running())
	assert.False(t, c.EverStarted())
}

func TestS2StorageController_StopWithoutStart(t *testing.T) {
	c, _ := newTestController(t, "test-stream")

	require.NoError(t, stopController(t, c))
	assert.False(t, c.EverStarted())
}

func TestS2StorageController_StopTimeoutKeepsWriter(t *testing.T) {
	c := &S2StorageController{
		writer:      &S2StorageWriter{started: true, done: make(chan struct{})},
		cancel:      func() {},
		everStarted: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, c.Stop(ctx), context.Canceled)
	assert.True(t, c.Running())
	assert.True(t, c.EverStarted())
	require.ErrorIs(t, c.Stop(ctx), context.Canceled)
}

func TestS2StorageController_StopHonorsContextDuringStart(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStart := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseStart()
	c := NewS2StorageController(newTestStream(t, 64), "test-basin", "test-token", func() string {
		close(entered)
		<-release
		return "test-stream"
	}, S2Config{}, slog.Default())
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	startDone := make(chan error, 1)
	go func() { startDone <- c.Start(parent, 0) }()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Stop(ctx) }()
	select {
	case err := <-stopDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Stop did not honor its context while Start was in progress")
	}

	releaseStart()
	require.ErrorIs(t, <-startDone, context.Canceled)
}

func TestS2StorageController_StateAvailableDuringStop(t *testing.T) {
	stopEntered := make(chan struct{})
	var cancelOnce sync.Once
	c := &S2StorageController{
		writer: &S2StorageWriter{started: true, done: make(chan struct{})},
		cancel: func() {
			cancelOnce.Do(func() { close(stopEntered) })
		},
		everStarted: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Stop(ctx) }()
	<-stopEntered

	runningDone := make(chan bool, 1)
	go func() { runningDone <- c.Running() }()
	select {
	case running := <-runningDone:
		assert.True(t, running)
	case <-time.After(time.Second):
		t.Fatal("Running blocked while Stop was in progress")
	}

	everStartedDone := make(chan bool, 1)
	go func() { everStartedDone <- c.EverStarted() }()
	select {
	case everStarted := <-everStartedDone:
		assert.True(t, everStarted)
	case <-time.After(time.Second):
		t.Fatal("EverStarted blocked while Stop was in progress")
	}

	cancel()
	require.ErrorIs(t, <-stopDone, context.Canceled)
}

func TestS2StorageController_StopHonorsContextDuringStop(t *testing.T) {
	stopEntered := make(chan struct{})
	var cancelOnce sync.Once
	c := &S2StorageController{
		writer: &S2StorageWriter{started: true, done: make(chan struct{})},
		cancel: func() {
			cancelOnce.Do(func() { close(stopEntered) })
		},
		everStarted: true,
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstStopDone := make(chan error, 1)
	go func() { firstStopDone <- c.Stop(firstCtx) }()
	<-stopEntered

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	cancelSecond()
	secondStopDone := make(chan error, 1)
	go func() { secondStopDone <- c.Stop(secondCtx) }()
	select {
	case err := <-secondStopDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Stop did not honor its context while another Stop was in progress")
	}

	cancelFirst()
	require.ErrorIs(t, <-firstStopDone, context.Canceled)
}

func TestS2StorageController_EverStartedSurvivesStop(t *testing.T) {
	c, resolved := newTestController(t, "test-stream")

	require.NoError(t, c.Start(context.Background(), 0))
	require.NoError(t, stopController(t, c))

	assert.False(t, c.Running())
	assert.True(t, c.EverStarted())

	require.NoError(t, c.Start(context.Background(), 0))
	assert.False(t, c.Running(), "a stopped controller must not reopen the sink")
	assert.Equal(t, int32(1), resolved.Load())
}

// The ring can still hold events captured before storage was wanted, and the
// writer must not replay them. A ring of four holding seqs 7..10 makes a
// replay observable without submitting anything: a writer after seq 10 has
// nothing to read, while one reading from seq 0 finds six evicted events.
func TestS2StorageController_StartSkipsEventsAtOrBeforeAfterSeq(t *testing.T) {
	es := newTestStream(t, 4)
	for range 10 {
		es.Publish(Envelope{Event: makeEvent("captured with storage off")})
	}
	c := NewS2StorageController(es, "test-basin", "test-token", func() string { return "test-stream" }, S2Config{}, slog.Default())

	require.NoError(t, c.Start(context.Background(), 10))
	require.True(t, c.Running())
	assert.Never(t, func() bool { return es.DroppedEvents() > 0 }, 200*time.Millisecond, time.Millisecond, "the writer read events at or before afterSeq")

	require.NoError(t, stopController(t, c))
}

// The first event the writer forwards is exactly the one after afterSeq, so
// nothing at the boundary captured with storage off is persisted.
func TestS2StorageWriter_ForwardsFromAfterSeq(t *testing.T) {
	es := newTestStream(t, 64)
	for range 10 {
		es.Publish(Envelope{Event: makeEvent("ev")})
	}
	backend := &mockBackend{}
	w := NewS2StorageWriter(es, "", "", "", 7, S2Config{}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.startLocked(ctx, backend)
	w.mu.Unlock()

	require.Eventually(t, func() bool { return len(backend.envelopes()) == 3 }, time.Second, time.Millisecond)
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, w.Stop(stopCtx))

	var seqs []uint64
	for _, env := range backend.envelopes() {
		seqs = append(seqs, env.Seq)
	}
	assert.Equal(t, []uint64{8, 9, 10}, seqs)
}
