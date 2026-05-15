package cdpmonitor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

		cs.onRequest()
		cs.onRequest()
		cs.onRequest()

		t0 := time.Now()
		cs.onLoadingFinished()
		cs.onLoadingFinished()
		cs.onLoadingFinished()

		ev := ec.waitFor(t, "network_idle", 2*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(400), "fired too early")
		assert.Equal(t, events.Network, ev.Category)
	})

	t.Run("timer_reset_on_new_request", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

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
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

		t0 := time.Now()
		cs.onPageLoad()

		ev := ec.waitFor(t, "page_layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(900), "fired too early")
		assert.Equal(t, events.Page, ev.Category)
	})

	t.Run("layout_shift_before_page_load_ignored", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

		// layout_shift before page_load should be ignored; layout_settled must
		// still fire after page_load's 1s debounce.
		cs.onLayoutShift()
		t0 := time.Now()
		cs.onPageLoad()

		ec.waitFor(t, "page_layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t0).Milliseconds(), int64(900), "should fire 1s after page_load, not layout_shift")
	})

	t.Run("layout_shift_resets_timer", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))
		cs.onPageLoad()

		time.Sleep(600 * time.Millisecond)
		cs.onLayoutShift()
		t1 := time.Now()

		ec.waitFor(t, "page_layout_settled", 3*time.Second)
		assert.GreaterOrEqual(t, time.Since(t1).Milliseconds(), int64(900), "should reset after layout_shift")
	})
}

func TestNavigationSettled(t *testing.T) {
	t.Run("fires_after_dom_content_loaded_and_layout_settled", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

		cs.onDOMContentLoaded()
		cs.onPageLoad()

		ev := ec.waitFor(t, "page_navigation_settled", 3*time.Second)
		assert.Equal(t, events.Page, ev.Category)
	})

	t.Run("not_blocked_by_pending_network_request", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

		cs.onRequest() // never finishes
		cs.onDOMContentLoaded()
		cs.onPageLoad()

		ec.waitFor(t, "page_navigation_settled", 3*time.Second)
		ec.assertNone(t, "network_idle", 100*time.Millisecond)
	})

	t.Run("interrupted_by_new_navigation", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

		cs.onDOMContentLoaded()
		// page_load not yet fired so layout_settled is still pending.

		require.NoError(t, cs.resetOnNavigation(0, navContext{}))

		ec.assertNone(t, "page_navigation_settled", 1500*time.Millisecond)
	})
}

func TestNavDataMetadata(t *testing.T) {
	ctx := navContext{
		sessionID:  "s1",
		targetID:   "t1",
		targetType: "page",
		frameID:    "f1",
		loaderID:   "l1",
		url:        "https://example.com",
	}

	t.Run("layout_settled_carries_navData_and_navMeta", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, ctx))
		cs.onPageLoad()

		ev := ec.waitFor(t, "page_layout_settled", 3*time.Second)
		assert.Equal(t, events.Page, ev.Category)
		assert.Equal(t, "s1", (*ev.Source.Metadata)[MetadataKeyCDPSessionID])
		assert.Equal(t, "t1", (*ev.Source.Metadata)[MetadataKeyTargetID])
		assert.Equal(t, "page", (*ev.Source.Metadata)[MetadataKeyTargetType])
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "s1", data["session_id"])
		assert.Equal(t, "l1", data["loader_id"])
		assert.Equal(t, "https://example.com", data["url"])
	})

	t.Run("navigation_settled_carries_navData_and_navMeta", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, ctx))

		cs.onDOMContentLoaded()
		cs.onPageLoad()

		ev := ec.waitFor(t, "page_navigation_settled", 3*time.Second)
		assert.Equal(t, events.Page, ev.Category)
		assert.Equal(t, "s1", (*ev.Source.Metadata)[MetadataKeyCDPSessionID])
		assert.Equal(t, "t1", (*ev.Source.Metadata)[MetadataKeyTargetID])
		var data map[string]any
		require.NoError(t, json.Unmarshal(ev.Data, &data))
		assert.Equal(t, "s1", data["session_id"])
		assert.Equal(t, "l1", data["loader_id"])
	})
}

func TestStopSuppressesTimers(t *testing.T) {
	t.Run("stop_suppresses_network_idle", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))
		cs.onRequest()
		cs.onLoadingFinished() // arms 500ms network_idle timer
		cs.stop()
		ec.assertNone(t, "network_idle", 1200*time.Millisecond)
	})

	t.Run("stop_suppresses_layout_settled", func(t *testing.T) {
		cs, ec := newTestComputed(t)
		require.NoError(t, cs.resetOnNavigation(0, navContext{}))
		cs.onPageLoad() // arms 1s layout_settled timer
		cs.stop()
		ec.assertNone(t, "page_layout_settled", 1500*time.Millisecond)
	})
}
