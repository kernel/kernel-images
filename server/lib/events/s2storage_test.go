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

// newTestController builds a controller whose streamFn returns stream and
// records how many times it was called, so a test can assert the name is
// resolved once, at Start.
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

// TestS2StorageController_OpensNothingUntilStart is the guarantee the whole
// export-only mode rests on: a controller that is never started resolves no
// stream, so no append session is opened and nothing is persisted.
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

// TestS2StorageController_EverStartedSurvivesStop covers the state a caller
// reads to decide whether anything could have been persisted: Running goes back
// to false at shutdown, EverStarted does not. Restarting is not allowed either,
// since the append session binds a stream for its lifetime.
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
