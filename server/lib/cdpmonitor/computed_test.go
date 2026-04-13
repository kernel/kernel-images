package cdpmonitor

import (
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
)

// newTestComputed creates a computedState with an eventCollector for testing.
func newTestComputed(t *testing.T) (*computedState, *eventCollector) {
	t.Helper()
	ec := newEventCollector()
	cs := newComputedState(ec.publishFn())
	return cs, ec
}

func TestNetworkIdle(t *testing.T) {
	t.Run("debounce_500ms", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		cs.resetOnNavigation(0)

		cs.onRequest()
		cs.onRequest()
		cs.onRequest()

		t0 := time.Now()
		cs.onLoadingFinished()
		cs.onLoadingFinished()
		cs.onLoadingFinished()

		ev := ec.waitFor(t, "network_idle", 2*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(400), "fired too early")
		assert.Equal(t, events.CategoryNetwork, ev.Category)
	})

	t.Run("timer_reset_on_new_request", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		cs.resetOnNavigation(0)

		cs.onRequest()
		cs.onLoadingFinished()
		time.Sleep(200 * time.Millisecond)

		cs.onRequest()
		t1 := time.Now()
		cs.onLoadingFinished()

		ec.waitFor(t, "network_idle", 2*time.Second)
		assert.GreaterOrEqual(t, time.Since(t1).Milliseconds(), int64(400), "should reset timer on new request")
	})
}

func TestLayoutSettled(t *testing.T) {
	t.Run("debounce_1s_after_page_load", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		cs.resetOnNavigation(0)

		t0 := time.Now()
		cs.onPageLoad()

		ev := ec.waitFor(t, "layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(900), "fired too early")
		assert.Equal(t, events.CategoryPage, ev.Category)
	})

	t.Run("layout_shift_before_page_load_ignored", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		cs.resetOnNavigation(0)

		// layout_shift before page_load should be ignored; layout_settled must
		// still fire after page_load's 1s debounce.
		cs.onLayoutShift()
		t0 := time.Now()
		cs.onPageLoad()

		ec.waitFor(t, "layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(900), "should fire 1s after page_load, not layout_shift")
	})

	t.Run("layout_shift_resets_timer", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		cs.resetOnNavigation(0)
		cs.onPageLoad()

		time.Sleep(600 * time.Millisecond)
		cs.onLayoutShift()
		t1 := time.Now()

		ec.waitFor(t, "layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t1).Milliseconds(), int64(900), "should reset after layout_shift")
	})
}

func TestNavigationSettled(t *testing.T) {
	t.Run("fires_when_all_three_flags_set", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		cs.resetOnNavigation(0)

		cs.onDOMContentLoaded()
		cs.onRequest()
		cs.onLoadingFinished()
		cs.onPageLoad()

		ev := ec.waitFor(t, "navigation_settled", 3*time.Second)
		assert.Equal(t, events.CategoryPage, ev.Category)
	})

	t.Run("interrupted_by_new_navigation", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		cs.resetOnNavigation(0)

		cs.onDOMContentLoaded()
		cs.onRequest()
		cs.onLoadingFinished()

		// Interrupt before layout_settled fires.
		cs.resetOnNavigation(0)

		ec.assertNone(t, "navigation_settled", 1500*time.Millisecond)
	})
}
