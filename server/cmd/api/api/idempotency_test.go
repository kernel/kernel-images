package api

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keepAll(int) bool { return true }

func TestIdempotentCallsRunOncePerKey(t *testing.T) {
	c := newIdempotentCalls[int](4)
	fp := idempotencyFingerprint("request")
	var runs atomic.Int32
	call := func(context.Context) (int, error) { return int(runs.Add(1)), nil }

	first, err := c.Do(context.Background(), "k", fp, call, keepAll)
	require.NoError(t, err)
	again, err := c.Do(context.Background(), "k", fp, call, keepAll)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.EqualValues(t, 1, runs.Load())

	other, err := c.Do(context.Background(), "other", fp, call, keepAll)
	require.NoError(t, err)
	assert.Equal(t, 2, other)
}

func TestIdempotentCallsRejectDifferentRequest(t *testing.T) {
	c := newIdempotentCalls[int](4)
	call := func(context.Context) (int, error) { return 1, nil }
	_, err := c.Do(context.Background(), "k", idempotencyFingerprint("a"), call, keepAll)
	require.NoError(t, err)
	_, err = c.Do(context.Background(), "k", idempotencyFingerprint("b"), call, keepAll)
	assert.ErrorIs(t, err, errIdempotencyKeyReused)
}

func TestIdempotentCallsReleaseKeyWhenNotKept(t *testing.T) {
	c := newIdempotentCalls[int](4)
	fp := idempotencyFingerprint("request")
	var runs atomic.Int32
	call := func(context.Context) (int, error) { return int(runs.Add(1)), nil }
	keepNone := func(int) bool { return false }

	_, err := c.Do(context.Background(), "k", fp, call, keepNone)
	require.NoError(t, err)
	_, err = c.Do(context.Background(), "k", fp, call, keepAll)
	require.NoError(t, err)
	assert.EqualValues(t, 2, runs.Load())

	failing := func(context.Context) (int, error) { runs.Add(1); return 0, errors.New("boom") }
	_, err = c.Do(context.Background(), "failing", fp, failing, keepAll)
	require.Error(t, err)
	_, err = c.Do(context.Background(), "failing", fp, failing, keepAll)
	require.Error(t, err)
	assert.EqualValues(t, 4, runs.Load())
}

func TestIdempotentCallsRecoverPanics(t *testing.T) {
	c := newIdempotentCalls[int](4)
	fp := idempotencyFingerprint("request")
	_, err := c.Do(context.Background(), "k", fp, func(context.Context) (int, error) { panic("boom") }, keepAll)
	require.ErrorContains(t, err, "panic: boom")
	resp, err := c.Do(context.Background(), "k", fp, func(context.Context) (int, error) { return 1, nil }, keepAll)
	require.NoError(t, err)
	assert.Equal(t, 1, resp, "a panicked call releases its key")
}

// A caller that disconnects does not stop the call; a retry gets its result.
func TestIdempotentCallsSurviveCallerCancellation(t *testing.T) {
	c := newIdempotentCalls[int](4)
	fp := idempotencyFingerprint("request")
	release := make(chan struct{})
	var runs atomic.Int32
	call := func(ctx context.Context) (int, error) {
		runs.Add(1)
		<-release
		return 7, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Do(ctx, "k", fp, call, keepAll)
		done <- err
	}()
	require.Eventually(t, func() bool { return runs.Load() == 1 }, time.Second, 5*time.Millisecond)
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)

	retry := make(chan int, 1)
	go func() {
		resp, err := c.Do(context.Background(), "k", fp, call, keepAll)
		assert.NoError(t, err)
		retry <- resp
	}()
	close(release)
	assert.Equal(t, 7, <-retry)
	assert.EqualValues(t, 1, runs.Load())
}

func TestIdempotentCallsForgetOldResults(t *testing.T) {
	c := newIdempotentCalls[int](2)
	now := time.Now()
	c.now = func() time.Time { return now }
	fp := idempotencyFingerprint("request")
	var runs atomic.Int32
	call := func(context.Context) (int, error) { return int(runs.Add(1)), nil }
	do := func(key string) int {
		t.Helper()
		resp, err := c.Do(context.Background(), key, fp, call, keepAll)
		require.NoError(t, err)
		return resp
	}

	do("a")
	do("b")
	do("c")
	assert.Equal(t, 4, do("a"), "the oldest result is dropped past the cap")
	assert.Equal(t, 3, do("c"))

	now = now.Add(idempotencyRetention + time.Second)
	assert.Equal(t, 5, do("c"), "results expire after the retention period")
}

func TestValidateIdempotencyKey(t *testing.T) {
	assert.NoError(t, validateIdempotencyKey("stainless-node-retry-0f3c2a1e"))
	assert.Error(t, validateIdempotencyKey(""))
	assert.Error(t, validateIdempotencyKey(strings.Repeat("k", maxIdempotencyKeyLength+1)))
	assert.Error(t, validateIdempotencyKey("bad\nkey"))
	assert.Error(t, validateIdempotencyKey("ключ"))
}
