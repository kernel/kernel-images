package cdpmonitor

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
)
const (
	networkIdleDebounce  = 500 * time.Millisecond
	layoutSettledDebounce = 1 * time.Second
)

// computedState holds the mutable state for all computed meta-events.
type computedState struct {
	mu      sync.Mutex
	publish PublishFunc

	// network_idle: 500 ms debounce after all pending requests finish.
	netPending int
	netTimer   *time.Timer
	netFired   bool

	// layout_settled: 1s after page_load with no intervening layout shifts.
	layoutTimer  *time.Timer
	layoutFired  bool
	pageLoadSeen bool

	// navigation_settled: fires once dom_content_loaded, network_idle, and
	// layout_settled have all fired after the same Page.frameNavigated.
	navDOMLoaded     bool
	navNetIdle       bool
	navLayoutSettled bool
	navFired         bool
}

// newComputedState creates a fresh computedState backed by the given publish func.
func newComputedState(publish PublishFunc) *computedState {
	return &computedState{publish: publish}
}

func stopTimer(t *time.Timer) {
	if t == nil {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// resetOnNavigation resets all state machines. Called on Page.frameNavigated
func (s *computedState) resetOnNavigation() {
	s.mu.Lock()
	defer s.mu.Unlock()

	stopTimer(s.netTimer)
	s.netTimer = nil
	s.netPending = 0
	s.netFired = false

	stopTimer(s.layoutTimer)
	s.layoutTimer = nil
	s.layoutFired = false
	s.pageLoadSeen = false

	s.navDOMLoaded = false
	s.navNetIdle = false
	s.navLayoutSettled = false
	s.navFired = false
}

func (s *computedState) onRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.netPending++
	// A new request invalidates any pending network_idle timer
	stopTimer(s.netTimer)
	s.netTimer = nil
}

// onLoadingFinished is called on Network.loadingFinished or Network.loadingFailed.
func (s *computedState) onLoadingFinished() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.netPending--
	if s.netPending < 0 {
		s.netPending = 0
	}
	if s.netPending > 0 || s.netFired {
		return
	}
	// All requests done and not yet fired — start 500 ms debounce timer.
	stopTimer(s.netTimer)
	s.netTimer = time.AfterFunc(networkIdleDebounce, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.netFired || s.netPending > 0 {
			return
		}
		s.netFired = true
		s.navNetIdle = true
		s.publish(events.Event{
			Ts:          time.Now().UnixMilli(),
			Type:        "network_idle",
			Category:    events.CategoryNetwork,
			Source:      events.Source{Kind: events.KindCDP},
			DetailLevel: events.DetailStandard,
			Data:        json.RawMessage(`{}`),
		})
		s.checkNavigationSettled()
	})
}

// onPageLoad is called on Page.loadEventFired.
func (s *computedState) onPageLoad() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageLoadSeen = true
	if s.layoutFired {
		return
	}
	// Start the 1 s layout_settled timer.
	stopTimer(s.layoutTimer)
	s.layoutTimer = time.AfterFunc(layoutSettledDebounce, s.emitLayoutSettled)
}

// onLayoutShift is called when a layout_shift sentinel arrives from injected JS.
func (s *computedState) onLayoutShift() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layoutFired || !s.pageLoadSeen {
		return
	}
	// Reset the timer to 1 s from now.
	stopTimer(s.layoutTimer)
	s.layoutTimer = time.AfterFunc(layoutSettledDebounce, s.emitLayoutSettled)
}

// emitLayoutSettled is called from the layout timer's AfterFunc goroutine
func (s *computedState) emitLayoutSettled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layoutFired || !s.pageLoadSeen {
		return
	}
	s.layoutFired = true
	s.navLayoutSettled = true
	s.publish(events.Event{
		Ts:          time.Now().UnixMilli(),
		Type:        "layout_settled",
		Category:    events.CategoryPage,
		Source:      events.Source{Kind: events.KindCDP},
		DetailLevel: events.DetailStandard,
		Data:        json.RawMessage(`{}`),
	})
	s.checkNavigationSettled()
}

// onDOMContentLoaded is called on Page.domContentEventFired.
func (s *computedState) onDOMContentLoaded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.navDOMLoaded = true
	s.checkNavigationSettled()
}

// checkNavigationSettled emits navigation_settled if all three flags are set
func (s *computedState) checkNavigationSettled() {
	if s.navDOMLoaded && s.navNetIdle && s.navLayoutSettled && !s.navFired {
		s.navFired = true
		s.publish(events.Event{
			Ts:          time.Now().UnixMilli(),
			Type:        "navigation_settled",
			Category:    events.CategoryPage,
			Source:      events.Source{Kind: events.KindCDP},
			DetailLevel: events.DetailStandard,
			Data:        json.RawMessage(`{}`),
		})
	}
}
