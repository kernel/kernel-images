package scaletozero

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDebouncedControllerSingleAcquireRelease(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))

	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 1, mock.enableCalls)
}

func TestDebouncedControllerMultipleAcquiresDebounced(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Acquire(t.Context()))

	assert.Equal(t, 1, mock.disableCalls)
}

func TestDebouncedControllerReleaseOnlyOnLastHolder(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))
	assert.Equal(t, 0, mock.enableCalls)

	require.NoError(t, c.Release(t.Context()))
	assert.Equal(t, 1, mock.enableCalls)
}

func TestDebouncedControllerAcquireFailureRollsBack(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{disableErr: assert.AnError}
	c := NewDebouncedController(mock)

	err := c.Acquire(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, mock.disableCalls)

	// Clear error; next Acquire should write again
	mock.disableErr = nil
	require.NoError(t, c.Acquire(t.Context()))
	assert.Equal(t, 2, mock.disableCalls)

	// Release should write once
	require.NoError(t, c.Release(t.Context()))
	assert.Equal(t, 1, mock.enableCalls)
}

func TestDebouncedControllerReleaseFailureRetry(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	require.NoError(t, c.Acquire(t.Context()))
	mock.enableErr = assert.AnError

	err := c.Release(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, mock.enableCalls)

	// Clear error; retry should succeed
	mock.enableErr = nil
	require.NoError(t, c.Release(t.Context()))
	assert.Equal(t, 2, mock.enableCalls)
}

func TestDebouncedControllerReleaseWithoutAcquireNoWrite(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)
	require.NoError(t, c.Release(t.Context()))
	assert.Equal(t, 0, mock.enableCalls)
}

func TestDebouncedControllerInterleavedSequence(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)
	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))
	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))
	assert.Equal(t, 2, mock.disableCalls)
	assert.Equal(t, 2, mock.enableCalls)
}

type mockToggler struct {
	mu           sync.Mutex
	disableCalls int
	enableCalls  int
	disableErr   error
	enableErr    error
}

func (m *mockToggler) Disable(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disableCalls++
	return m.disableErr
}

func (m *mockToggler) Enable(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enableCalls++
	return m.enableErr
}

func TestUnikraftCloudTogglerNoFileNoError(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "scale_to_zero_disable")
	c := &unikraftCloudToggler{path: p}

	require.NoError(t, c.Disable(t.Context()))
	require.NoError(t, c.Enable(t.Context()))

	_, err := os.Stat(p)
	assert.True(t, os.IsNotExist(err), "should not create the file on no-op")
}

func TestUnikraftCloudTogglerWritesPlusAndMinus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "scale_to_zero_disable")
	require.NoError(t, os.WriteFile(p, []byte{}, 0o600))
	c := &unikraftCloudToggler{path: p}

	require.NoError(t, c.Disable(t.Context()))
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, []byte("+"), b)

	require.NoError(t, c.Enable(t.Context()))
	b, err = os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, []byte("-"), b)
}

func TestUnikraftCloudTogglerTruncatesExistingContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "scale_to_zero_disable")
	require.NoError(t, os.WriteFile(p, []byte("abc123"), 0o600))
	c := &unikraftCloudToggler{path: p}

	require.NoError(t, c.Disable(t.Context()))
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, []byte("+"), b)
}

func TestDebouncedControllerCooldownDelaysRelease(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedControllerWithCooldown(mock, 50*time.Millisecond)

	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))

	// Underlying Enable should not have been called yet — still in cooldown
	mock.mu.Lock()
	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 0, mock.enableCalls)
	mock.mu.Unlock()

	// Wait for cooldown to fire
	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	assert.Equal(t, 1, mock.enableCalls)
	mock.mu.Unlock()
}

func TestDebouncedControllerCooldownCancelledByNewAcquire(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedControllerWithCooldown(mock, 50*time.Millisecond)

	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))

	// New request arrives before cooldown fires
	require.NoError(t, c.Acquire(t.Context()))

	// Wait past what would have been the cooldown
	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	// Underlying Enable should NOT have been called — the new Acquire cancelled the timer
	assert.Equal(t, 0, mock.enableCalls)
	// Only one actual underlying Disable (second Acquire saw already-off state)
	assert.Equal(t, 1, mock.disableCalls)
	mock.mu.Unlock()

	// Release the second request
	require.NoError(t, c.Release(t.Context()))
	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	assert.Equal(t, 1, mock.enableCalls)
	mock.mu.Unlock()
}

func TestDebouncedControllerCooldownCollapsesRapidSequential(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedControllerWithCooldown(mock, 50*time.Millisecond)

	// Simulate 10 rapid sequential requests
	for i := 0; i < 10; i++ {
		require.NoError(t, c.Acquire(t.Context()))
		require.NoError(t, c.Release(t.Context()))
	}

	// Only 1 underlying Disable; underlying Enable not yet called (still in cooldown)
	mock.mu.Lock()
	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 0, mock.enableCalls)
	mock.mu.Unlock()

	// Wait for final cooldown
	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 1, mock.enableCalls)
	mock.mu.Unlock()
}

func TestDebouncedControllerCooldownZeroBehavesLikeOriginal(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedControllerWithCooldown(mock, 0)

	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))

	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 1, mock.enableCalls)
}

func TestDebouncedControllerDisableHoldsAcrossRelease(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	// Idempotent Disable first.
	require.NoError(t, c.Disable(t.Context()))
	assert.Equal(t, 1, mock.disableCalls)

	// Simulate a middleware-wrapped request: Acquire then Release.
	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))

	// Idempotent disable still held, so no underlying Enable should have fired.
	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 0, mock.enableCalls)

	// Release the idempotent disable: Enable fires.
	require.NoError(t, c.Enable(t.Context()))
	assert.Equal(t, 1, mock.enableCalls)
}

func TestDebouncedControllerDisableIdempotent(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	require.NoError(t, c.Disable(t.Context()))
	require.NoError(t, c.Disable(t.Context()))
	require.NoError(t, c.Disable(t.Context()))
	assert.Equal(t, 1, mock.disableCalls)

	require.NoError(t, c.Enable(t.Context()))
	require.NoError(t, c.Enable(t.Context()))
	assert.Equal(t, 1, mock.enableCalls)
}

func TestDebouncedControllerEnableWithoutDisableNoWrite(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	require.NoError(t, c.Enable(t.Context()))
	assert.Equal(t, 0, mock.disableCalls)
	assert.Equal(t, 0, mock.enableCalls)
}

func TestDebouncedControllerEnableDefersToActiveHolds(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedController(mock)

	require.NoError(t, c.Disable(t.Context()))
	require.NoError(t, c.Acquire(t.Context())) // simulate inflight request

	// Releasing the idempotent disable while a hold is active must not re-enable.
	require.NoError(t, c.Enable(t.Context()))
	assert.Equal(t, 0, mock.enableCalls)

	// Hold released -> underlying Enable fires.
	require.NoError(t, c.Release(t.Context()))
	assert.Equal(t, 1, mock.enableCalls)
}

func TestDebouncedControllerDisableCancelsCooldownTimer(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedControllerWithCooldown(mock, 50*time.Millisecond)

	// Drive a request through, putting us into the cooldown window.
	require.NoError(t, c.Acquire(t.Context()))
	require.NoError(t, c.Release(t.Context()))

	// Idempotent Disable during the cooldown: should cancel the pending re-enable.
	require.NoError(t, c.Disable(t.Context()))

	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	assert.Equal(t, 1, mock.disableCalls)
	assert.Equal(t, 0, mock.enableCalls)
	mock.mu.Unlock()

	require.NoError(t, c.Enable(t.Context()))
	time.Sleep(100 * time.Millisecond)
	mock.mu.Lock()
	assert.Equal(t, 1, mock.enableCalls)
	mock.mu.Unlock()
}

func TestDebouncedControllerEnableHonorsCooldown(t *testing.T) {
	t.Parallel()
	mock := &mockToggler{}
	c := NewDebouncedControllerWithCooldown(mock, 50*time.Millisecond)

	require.NoError(t, c.Disable(t.Context()))
	require.NoError(t, c.Enable(t.Context()))

	// Cooldown should defer the underlying Enable.
	mock.mu.Lock()
	assert.Equal(t, 0, mock.enableCalls)
	mock.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	assert.Equal(t, 1, mock.enableCalls)
	mock.mu.Unlock()
}
