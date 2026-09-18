package events

import (
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

	require.NoError(t, c.Start(context.Background()))
	first := c.writer
	require.NotNil(t, first)

	require.NoError(t, c.Start(context.Background()))
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
			errs <- c.Start(context.Background())
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

func TestS2StorageController_StartFailureRollsBack(t *testing.T) {
	c, resolved := newTestController(t, "test-stream")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, c.Start(ctx), context.Canceled)
	assert.False(t, c.Running())
	assert.False(t, c.EverStarted())
	assert.Equal(t, int32(1), resolved.Load())

	require.NoError(t, c.Start(context.Background()))
	assert.True(t, c.Running())
	assert.True(t, c.EverStarted())
	assert.Equal(t, int32(2), resolved.Load())

	require.NoError(t, stopController(t, c))
}

func TestS2StorageController_EmptyStreamDoesNotStart(t *testing.T) {
	c, resolved := newTestController(t, "")

	require.NoError(t, c.Start(context.Background()))
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

	require.NoError(t, c.Start(context.Background()))
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
	go func() { startDone <- c.Start(parent) }()
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

	require.NoError(t, c.Start(context.Background()))
	require.NoError(t, stopController(t, c))

	assert.False(t, c.Running())
	assert.True(t, c.EverStarted())

	require.NoError(t, c.Start(context.Background()))
	assert.False(t, c.Running(), "a stopped controller must not reopen the sink")
	assert.Equal(t, int32(1), resolved.Load())
}
