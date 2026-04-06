package cdpmonitor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
)

func TestNetworkIdle(t *testing.T) {
	t.Run("debounce_500ms", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		simulateRequest(m, "r1")
		simulateRequest(m, "r2")
		simulateRequest(m, "r3")

		t0 := time.Now()
		simulateFinished(m, "r1")
		simulateFinished(m, "r2")
		simulateFinished(m, "r3")

		ev := ec.waitFor(t, "network_idle", 2*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(400), "fired too early")
		assert.Equal(t, events.CategoryNetwork, ev.Category)
	})

	t.Run("timer_reset_on_new_request", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		simulateRequest(m, "a1")
		simulateFinished(m, "a1")
		time.Sleep(200 * time.Millisecond)

		simulateRequest(m, "a2")
		t1 := time.Now()
		simulateFinished(m, "a2")

		ec.waitFor(t, "network_idle", 2*time.Second)
		assert.GreaterOrEqual(t, time.Since(t1).Milliseconds(), int64(400), "should reset timer on new request")
	})
}

func TestLayoutSettled(t *testing.T) {
	t.Run("debounce_1s_after_page_load", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		t0 := time.Now()
		m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

		ev := ec.waitFor(t, "layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(900), "fired too early")
		assert.Equal(t, events.CategoryPage, ev.Category)
	})

	t.Run("layout_shift_resets_timer", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")
		m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

		time.Sleep(600 * time.Millisecond)
		shiftParams, _ := json.Marshal(map[string]any{
			"event": map[string]any{"type": "layout-shift"},
		})
		m.handleTimelineEvent(shiftParams, "s1")
		t1 := time.Now()

		ec.waitFor(t, "layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t1).Milliseconds(), int64(900), "should reset after layout_shift")
	})
}

func TestNavigationSettled(t *testing.T) {
	t.Run("fires_when_all_three_flags_set", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		m.handleDOMContentLoaded(json.RawMessage(`{}`), "s1")

		simulateRequest(m, "r1")
		simulateFinished(m, "r1")

		m.handleLoadEventFired(json.RawMessage(`{}`), "s1")

		ev := ec.waitFor(t, "navigation_settled", 3*time.Second)
		assert.Equal(t, events.CategoryPage, ev.Category)
	})

	t.Run("interrupted_by_new_navigation", func(t *testing.T) {
		m, ec := newComputedMonitor(t)
		navigateMonitor(m, "https://example.com")

		m.handleDOMContentLoaded(json.RawMessage(`{}`), "s1")

		simulateRequest(m, "r2")
		simulateFinished(m, "r2")

		// Interrupt before layout_settled fires.
		navigateMonitor(m, "https://example.com/page2")

		ec.assertNone(t, "navigation_settled", 1500*time.Millisecond)
	})
}
