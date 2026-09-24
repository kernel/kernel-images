package pagerecovery

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testURL = "https://store.example/item"

func fixedClock(start time.Time) (*time.Time, func() time.Time) {
	now := start
	return &now, func() time.Time { return now }
}

func TestReserveStopsAtMaxAttempts(t *testing.T) {
	clock, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 2, Budget: time.Minute}, now)

	for attempt := 1; attempt <= 2; attempt++ {
		wait, ok := l.reserve("s1", testURL, 0, false)
		require.True(t, ok, "attempt %d", attempt)
		assert.Positive(t, wait)
		*clock = clock.Add(wait)
	}
	_, ok := l.reserve("s1", testURL, 0, false)
	assert.False(t, ok, "the third refusal is the site's answer, not a retry")
}

func TestReserveStopsWhenBudgetIsSpent(t *testing.T) {
	clock, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 10, Budget: time.Second}, now)

	_, ok := l.reserve("s1", testURL, 0, false)
	require.True(t, ok)
	*clock = clock.Add(2 * time.Second)

	_, ok = l.reserve("s1", testURL, 0, false)
	assert.False(t, ok)
}

func TestRetryAfterIsHonouredWithinTheBudget(t *testing.T) {
	_, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 3, Budget: 8 * time.Second}, now)

	wait, ok := l.reserve("s1", testURL, 2*time.Second, true)
	require.True(t, ok)
	assert.Equal(t, 2*time.Second, wait, "a site that named a delay gets that delay")

	// Longer than the budget has left: waiting it out would read as a hung
	// navigation, so the refusal goes through instead.
	_, ok = l.reserve("s1", testURL, time.Hour, true)
	assert.False(t, ok)
}

func TestBudgetIsPerSessionAndPerURL(t *testing.T) {
	_, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 1, Budget: time.Minute}, now)

	_, ok := l.reserve("s1", testURL, 0, false)
	require.True(t, ok)
	_, ok = l.reserve("s1", testURL, 0, false)
	require.False(t, ok)

	_, ok = l.reserve("s2", testURL, 0, false)
	assert.True(t, ok, "another tab has its own budget")
	_, ok = l.reserve("s1", testURL+"?page=2", 0, false)
	assert.True(t, ok, "another URL has its own budget")
}

func TestFragmentsShareOneBudget(t *testing.T) {
	_, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 1, Budget: time.Minute}, now)

	_, ok := l.reserve("s1", testURL+"#reviews", 0, false)
	require.True(t, ok)
	_, ok = l.reserve("s1", testURL, 0, false)
	assert.False(t, ok, "the fragment does not name a different document")
}

func TestSettleReportsAttemptsAndReleasesTheBudget(t *testing.T) {
	_, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 1, Budget: time.Minute}, now)

	assert.Zero(t, l.settle("s1", testURL), "a page that was never refused reports no attempts")

	_, ok := l.reserve("s1", testURL, 0, false)
	require.True(t, ok)
	assert.Equal(t, 1, l.settle("s1", testURL))

	_, ok = l.reserve("s1", testURL, 0, false)
	assert.True(t, ok, "the next navigation is judged on its own")
}

func TestForgetDropsOnlyOneSession(t *testing.T) {
	_, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 1, Budget: time.Minute}, now)

	_, _ = l.reserve("s1", testURL, 0, false)
	_, _ = l.reserve("s2", testURL, 0, false)
	l.forget("s1")

	_, ok := l.reserve("s1", testURL, 0, false)
	assert.True(t, ok)
	_, ok = l.reserve("s2", testURL, 0, false)
	assert.False(t, ok)
}

func TestExpiredEntriesAreReleased(t *testing.T) {
	clock, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 1, Budget: time.Minute}, now)

	_, ok := l.reserve("s1", testURL, 0, false)
	require.True(t, ok)
	*clock = clock.Add(attemptTTL + time.Minute)

	_, ok = l.reserve("s1", testURL, 0, false)
	assert.True(t, ok, "a page revisited much later starts fresh")
}

func TestLedgerStaysBounded(t *testing.T) {
	_, now := fixedClock(time.Now())
	l := newLedger(Config{MaxAttempts: 4, Budget: time.Minute}, now)

	for i := 0; i < maxTrackedAttempts*2; i++ {
		_, _ = l.reserve("s1", fmt.Sprintf("%s/%d", testURL, i), 0, false)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	assert.LessOrEqual(t, len(l.entries), maxTrackedAttempts)
}

func TestBackoffGrowsAndJitters(t *testing.T) {
	distinct := make(map[time.Duration]struct{})
	for i := 0; i < 64; i++ {
		distinct[backoff(0)] = struct{}{}
	}
	assert.Greater(t, len(distinct), 1, "lockstep retries are how a short block becomes a long one")

	for attempt := 0; attempt < 4; attempt++ {
		lower := (defaultBaseBackoff << attempt) / 2
		upper := defaultBaseBackoff << attempt
		got := backoff(attempt)
		assert.GreaterOrEqual(t, got, lower)
		assert.LessOrEqual(t, got, upper)
	}
}
