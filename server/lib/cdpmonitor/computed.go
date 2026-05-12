package cdpmonitor

import (
	"encoding/json"
	"maps"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
)

const (
	// networkIdleDebounce matches Playwright's networkidle heuristic: fire after
	// 500 ms with no in-flight network requests.
	networkIdleDebounce = 500 * time.Millisecond
	// layoutSettledDebounce gives the page 1 s after the last layout shift (or
	// page_load if no shifts occur) before declaring the layout stable. 1 s is
	// chosen to cover typical late-loading web fonts and deferred image reflows.
	layoutSettledDebounce = 1 * time.Second
)

// computedState holds the mutable state for all computed meta-events.
type computedState struct {
	mu      sync.Mutex
	publish PublishFunc

	// dead is set by stop(). Timer callbacks check it under mu and bail,
	// preventing orphaned events after a session is detached or cleared.
	dead bool

	// navSeq is incremented on every resetOnNavigation. AfterFunc callbacks
	// capture their navSeq at creation and bail if it has changed, preventing
	// stale timers from publishing events for a previous navigation.
	navSeq int

	// navCtx is the navigation identity stamped at the last Page.frameNavigated.
	// navData and navMeta are its precomputed JSON payload and Source.Metadata.
	// Maps are replaced (not mutated) on each reset, so in-flight events holding
	// a pointer to old navMeta are safe.
	navCtx  navContext
	navData json.RawMessage
	navMeta map[string]string

	// network_idle: 500 ms debounce after all pending requests finish.
	netPending int
	netTimer   *time.Timer
	netFired   bool

	// layout_settled: 1s after page_load with no intervening layout shifts.
	layoutTimer  *time.Timer
	layoutFired  bool
	pageLoadSeen bool

	// navigation_settled: fires once dom_content_loaded and layout_settled have
	// both fired after the same Page.frameNavigated. Decoupled from network_idle
	navDOMLoaded     bool
	navLayoutSettled bool
	navFired         bool
}

// newComputedState creates a fresh computedState backed by the given publish func.
// navData is initialized to {} and navMeta to an empty map so events emitted
// before the first frameNavigated carry consistent empty payloads rather than null.
func newComputedState(publish PublishFunc) *computedState {
	return &computedState{
		publish: publish,
		navData: json.RawMessage(`{}`),
		navMeta: make(map[string]string),
	}
}

// navSnapshot returns the precomputed nav payload and metadata under mu.
func (s *computedState) navSnapshot() (json.RawMessage, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.navData, s.navMeta
}

// navDataWith merges extra fields into the current nav payload.
// Nav context fields (session_id, target_id, etc.) always take precedence over
// caller-supplied extra so a page-controlled payload cannot forge nav identity.
func (s *computedState) navDataWith(extra map[string]any) json.RawMessage {
	result := make(map[string]any)
	maps.Copy(result, extra)
	if s != nil {
		d, _ := s.navSnapshot()
		base := make(map[string]any)
		_ = json.Unmarshal(d, &base)
		maps.Copy(result, base)
	}
	out, _ := json.Marshal(result)
	return out
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

// stop marks the state machine dead and cancels pending timers. Called when the
// owning session detaches or the monitor reconnects. Any AfterFunc goroutine
// already running will check dead under mu and discard its result.
func (s *computedState) stop() {
	s.mu.Lock()
	s.dead = true
	stopTimer(s.netTimer)
	stopTimer(s.layoutTimer)
	s.mu.Unlock()
}

// resetOnNavigation resets all state machines. Called on Page.frameNavigated.
// Increments navSeq so any AfterFunc callbacks already running will discard their results.
// inflight seeds netPending; callers pass 0 because each session only tracks its
// own requests and starts fresh on navigation.
func (s *computedState) resetOnNavigation(inflight int, ctx navContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.navSeq++
	s.navCtx = ctx
	navData, err := json.Marshal(map[string]any{
		"session_id":  ctx.sessionID,
		"target_id":   ctx.targetID,
		"target_type": ctx.targetType,
		"frame_id":    ctx.frameID,
		"loader_id":   ctx.loaderID,
		"url":         ctx.url,
		"nav_seq":     s.navSeq,
	})
	if err != nil {
		return err
	}
	s.navData = navData
	s.navMeta = map[string]string{
		MetadataKeyCDPSessionID: ctx.sessionID,
		MetadataKeyTargetID:     ctx.targetID,
		MetadataKeyTargetType:   ctx.targetType,
	}

	stopTimer(s.netTimer)
	s.netTimer = nil
	s.netPending = inflight
	s.netFired = false
	if inflight == 0 {
		s.startNetIdleTimer()
	}

	stopTimer(s.layoutTimer)
	s.layoutTimer = nil
	s.layoutFired = false
	s.pageLoadSeen = false

	s.navDOMLoaded = false
	s.navLayoutSettled = false
	s.navFired = false
	return nil
}

func (s *computedState) onRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	s.netPending++
	// A new request invalidates any pending network_idle timer
	stopTimer(s.netTimer)
	s.netTimer = nil
}

// onLoadingFinished is called on Network.loadingFinished or Network.loadingFailed.
func (s *computedState) onLoadingFinished() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}

	s.netPending--
	if s.netPending < 0 {
		// Clamping to zero: received a loadingFinished with no matching onRequest.
		// This can happen if we attached mid-flight and missed the requestWillBeSent event.
		s.netPending = 0
	}
	if s.netPending > 0 || s.netFired {
		return
	}
	// All requests done and not yet fired: start 500ms debounce timer.
	s.startNetIdleTimer()
}

// startNetIdleTimer arms the network_idle debounce timer. Must be called with s.mu held.
func (s *computedState) startNetIdleTimer() {
	if s.dead {
		return
	}
	stopTimer(s.netTimer)
	navSeq := s.navSeq
	navData := s.navData
	navMeta := s.navMeta
	s.netTimer = time.AfterFunc(networkIdleDebounce, func() {
		s.mu.Lock()
		if s.dead || s.navSeq != navSeq || s.netFired || s.netPending > 0 {
			s.mu.Unlock()
			return
		}
		s.netFired = true
		s.mu.Unlock()
		s.publish(events.Event{
			Ts:       time.Now().UnixMicro(),
			Type:     EventNetworkIdle,
			Category: events.CategoryNetwork,
			Source: events.Source{
				Kind:     events.KindCDP,
				Metadata: navMeta,
			},
			Data: navData,
		})
	})
}

// onPageLoad is called on Page.loadEventFired.
func (s *computedState) onPageLoad() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
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
	if s.dead || s.layoutFired || !s.pageLoadSeen {
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
	if s.dead || s.navSeq != navSeq || s.layoutFired || !s.pageLoadSeen {
		s.mu.Unlock()
		return
	}
	s.layoutFired = true
	s.navLayoutSettled = true
	navData := s.navData
	navMeta := s.navMeta
	evs := []events.Event{{
		Ts:       time.Now().UnixMicro(),
		Type:     EventLayoutSettled,
		Category: events.CategoryPage,
		Source: events.Source{
			Kind:     events.KindCDP,
			Metadata: navMeta,
		},
		Data: navData,
	}}
	evs = append(evs, s.pendingNavigationSettled()...)
	s.mu.Unlock()
	for _, ev := range evs {
		s.publish(ev)
	}
}

// onDOMContentLoaded is called on Page.domContentEventFired.
func (s *computedState) onDOMContentLoaded() {
	s.mu.Lock()
	s.navDOMLoaded = true
	evs := s.pendingNavigationSettled()
	s.mu.Unlock()
	for _, ev := range evs {
		s.publish(ev)
	}
}

// pendingNavigationSettled returns a navigation_settled event if both
// conditions are met. Must be called with s.mu held.
func (s *computedState) pendingNavigationSettled() []events.Event {
	if s.dead {
		return nil
	}
	if s.navDOMLoaded && s.navLayoutSettled && !s.navFired {
		s.navFired = true
		return []events.Event{{
			Ts:       time.Now().UnixMicro(),
			Type:     EventNavigationSettled,
			Category: events.CategoryPage,
			Source: events.Source{
				Kind:     events.KindCDP,
				Metadata: s.navMeta,
			},
			Data: s.navData,
		}}
	}
	return nil
}
