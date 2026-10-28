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
	c := newIdempotentCalls[int](4, 4)
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
	c := newIdempotentCalls[int](4, 4)
	call := func(context.Context) (int, error) { return 1, nil }
	_, err := c.Do(context.Background(), "k", idempotencyFingerprint("a"), call, keepAll)
	require.NoError(t, err)
	_, err = c.Do(context.Background(), "k", idempotencyFingerprint("b"), call, keepAll)
	assert.ErrorIs(t, err, errIdempotencyKeyReused)
}

func TestIdempotentCallsReleaseKeyWhenNotKept(t *testing.T) {
	c := newIdempotentCalls[int](4, 4)
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
	c := newIdempotentCalls[int](4, 4)
	fp := idempotencyFingerprint("request")
	_, err := c.Do(context.Background(), "k", fp, func(context.Context) (int, error) { panic("boom") }, keepAll)
	require.ErrorContains(t, err, "panic: boom")
	resp, err := c.Do(context.Background(), "k", fp, func(context.Context) (int, error) { return 1, nil }, keepAll)
	require.NoError(t, err)
	assert.Equal(t, 1, resp, "a panicked call releases its key")
}

// A caller that disconnects does not stop the call; a retry gets its result.
func TestIdempotentCallsSurviveCallerCancellation(t *testing.T) {
	c := newIdempotentCalls[int](4, 4)
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
	c := newIdempotentCalls[int](2, 4)
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

// A call no caller waits on is cancelled after the grace period, unless a
// retry attaches first.
func TestIdempotentCallsCancelAbandonedCall(t *testing.T) {
	c := newIdempotentCalls[int](4, 4)
	c.abandonGrace = 100 * time.Millisecond
	fp := idempotencyFingerprint("request")
	started := make(chan struct{}, 1)
	stopped := make(chan error, 1)
	hung := func(ctx context.Context) (int, error) {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- context.Cause(ctx)
		return 0, context.Cause(ctx)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Do(ctx, "k", fp, hung, keepAll)
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	// A retry within the grace period keeps the call alive while it waits.
	retryCtx, cancelRetry := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelRetry()
	_, err := c.Do(retryCtx, "k", fp, hung, keepAll)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case <-stopped:
		t.Fatal("the call was cancelled while a retry waited on it")
	default:
	}

	// With no caller left, the call is cancelled after the grace period.
	select {
	case cause := <-stopped:
		assert.ErrorIs(t, cause, errIdempotentCallAbandoned)
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned call was not cancelled")
	}
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.calls["k"]
		return !ok
	}, time.Second, 5*time.Millisecond, "a cancelled call releases its key")
}

// Pending calls outlive their callers, so new keys are refused past the cap.
// A retry of a pending key still waits for it.
func TestIdempotentCallsCapPending(t *testing.T) {
	c := newIdempotentCalls[int](4, 1)
	fp := idempotencyFingerprint("request")
	release := make(chan struct{})
	blocked := func(context.Context) (int, error) { <-release; return 1, nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Do(ctx, "running", fp, blocked, keepAll)
	require.ErrorIs(t, err, context.Canceled)

	_, err = c.Do(context.Background(), "new", fp, blocked, keepAll)
	require.ErrorIs(t, err, errIdempotentCallsBusy)

	retry := make(chan int, 1)
	go func() {
		resp, err := c.Do(context.Background(), "running", fp, blocked, keepAll)
		assert.NoError(t, err)
		retry <- resp
	}()
	close(release)
	assert.Equal(t, 1, <-retry)

	resp, err := c.Do(context.Background(), "new", fp, func(context.Context) (int, error) { return 2, nil }, keepAll)
	require.NoError(t, err)
	assert.Equal(t, 2, resp, "finishing a call frees its pending slot")
}

func TestValidateIdempotencyKey(t *testing.T) {
	assert.NoError(t, validateIdempotencyKey("stainless-node-retry-0f3c2a1e"))
	assert.Error(t, validateIdempotencyKey(""))
	assert.Error(t, validateIdempotencyKey(strings.Repeat("k", maxIdempotencyKeyLength+1)))
	assert.Error(t, validateIdempotencyKey("bad\nkey"))
	assert.Error(t, validateIdempotencyKey("ключ"))
}
