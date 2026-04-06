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

	// navSeq is incremented on every resetOnNavigation. AfterFunc callbacks
	// capture their navSeq at creation and bail if it has changed, preventing
	// stale timers from publishing events for a previous navigation.
	navSeq int

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

// resetOnNavigation resets all state machines. Called on Page.frameNavigated.
// Increments navSeq so any AfterFunc callbacks already running will discard their results.
func (s *computedState) resetOnNavigation() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.navSeq++

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
	// All requests done and not yet fired: start 500ms debounce timer.
	stopTimer(s.netTimer)
	navSeq := s.navSeq
	s.netTimer = time.AfterFunc(networkIdleDebounce, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.navSeq != navSeq || s.netFired || s.netPending > 0 {
			return
		}
		s.netFired = true
		s.navNetIdle = true
		s.publish(events.Event{
			Ts:          time.Now().UnixMilli(),
			Type:        EventNetworkIdle,
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
	// Start the 1s layout_settled timer.
	stopTimer(s.layoutTimer)
	navSeq := s.navSeq
	s.layoutTimer = time.AfterFunc(layoutSettledDebounce, func() { s.emitLayoutSettled(navSeq) })
}

// onLayoutShift is called when a layout_shift sentinel arrives from injected JS.
func (s *computedState) onLayoutShift() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layoutFired || !s.pageLoadSeen {
		return
	}
	// Reset the timer to 1s from now.
	stopTimer(s.layoutTimer)
	navSeq := s.navSeq
	s.layoutTimer = time.AfterFunc(layoutSettledDebounce, func() { s.emitLayoutSettled(navSeq) })
}

// emitLayoutSettled is called from the layout timer's AfterFunc goroutine.
func (s *computedState) emitLayoutSettled(navSeq int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.navSeq != navSeq || s.layoutFired || !s.pageLoadSeen {
		return
	}
	s.layoutFired = true
	s.navLayoutSettled = true
	s.publish(events.Event{
		Ts:          time.Now().UnixMilli(),
		Type:        EventLayoutSettled,
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
			Type:        EventNavigationSettled,
			Category:    events.CategoryPage,
			Source:      events.Source{Kind: events.KindCDP},
			DetailLevel: events.DetailStandard,
			Data:        json.RawMessage(`{}`),
		})
	}
}
