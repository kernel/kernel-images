package telemetry

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// TelemetryConfig holds caller-supplied telemetry preferences. All fields are
// optional; zero values mean "use server defaults" (events.DefaultCategories).
type TelemetryConfig struct {
	// Categories limits which event categories are captured. nil or empty
	// captures events.DefaultCategories. Monitor is added automatically when
	// any CDP category is present and is not configurable here.
	Categories []oapi.TelemetryEventCategory
	// ExportOTLP forwards captured events to the configured OTLP endpoint.
	// Off by default and independent of what is captured.
	ExportOTLP bool
	// ExcludedCdpMethods leaves the named browser-control methods out of the
	// cdp_command stream. Empty reports every supported method. Telemetry only:
	// an excluded command still reaches the browser.
	ExcludedCdpMethods []oapi.BrowserCdpCommandMethod
}

// TelemetrySession manages a telemetry session against a shared EventStream.
// It category-filters Publish calls, tracks session-scoped metadata (ID,
// config, timestamps), and embeds telemetry_session_id into
// Event.Source.Metadata before forwarding to the bus.
//
// A *TelemetrySession is required to be non-nil: NewTelemetrySession panics
// on a nil EventStream and ApiService construction rejects a nil session.
// Callers should not nil-check.
type TelemetrySession struct {
	es              *events.EventStream
	mu              sync.Mutex
	id              string
	sessionStartSeq uint64
	categories      map[oapi.TelemetryEventCategory]struct{}
	exportOTLP      bool
	appliedAt       time.Time
	excludedCdp     map[string]struct{}
	// active mirrors "a session is running with these categories" for callers
	// on a hot path, who must decide whether to do any work at all before they
	// reach Publish and its mutex. nil means no session. Written under mu;
	// the pointed-to set is never mutated after it is stored.
	active atomic.Pointer[map[oapi.TelemetryEventCategory]struct{}]
	// excludedCdpActive mirrors excludedCdp for the same reason. Never nil once
	// stored, and the pointed-to set is never mutated after it is stored.
	excludedCdpActive atomic.Pointer[map[string]struct{}]
}

func NewTelemetrySession(es *events.EventStream) *TelemetrySession {
	if es == nil {
		panic("telemetry: NewTelemetrySession requires a non-nil EventStream")
	}
	return &TelemetrySession{es: es, categories: categorySet(nil)}
}

// setActiveLocked republishes the lock-free view of the session state.
// Requires s.mu to be held.
func (s *TelemetrySession) setActiveLocked() {
	if s.id == "" {
		s.active.Store(nil)
		return
	}
	cats := s.categories
	s.active.Store(&cats)
	excluded := s.excludedCdp
	s.excludedCdpActive.Store(&excluded)
}

// categorySet builds the active filter set from the configured categories. An
// empty config falls back to the default set. Monitor is included whenever any
// CDP category is present, since collector-health rides along with CDP data.
func categorySet(cats []oapi.TelemetryEventCategory) map[oapi.TelemetryEventCategory]struct{} {
	if len(cats) == 0 {
		cats = events.DefaultCategories
	}
	set := make(map[oapi.TelemetryEventCategory]struct{}, len(cats)+1)
	for _, c := range cats {
		set[c] = struct{}{}
	}
	if events.HasCDPCategory(cats) {
		set[events.Monitor] = struct{}{}
	}
	return set
}

// excludedSet builds the cdp_command exclusion set. nil when nothing is
// excluded, which is the common case and the cheapest lookup.
func excludedSet(methods []oapi.BrowserCdpCommandMethod) map[string]struct{} {
	if len(methods) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		set[string(m)] = struct{}{}
	}
	return set
}

// Start begins a new telemetry session with the given ID and config. Sequence
// numbers are process-monotonic and do not reset between sessions; a
// Last-Event-ID from any previous session is valid for resuming the SSE stream.
func (s *TelemetrySession) Start(telemetrySessionID string, cfg TelemetryConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = telemetrySessionID
	s.sessionStartSeq = s.es.Seq()
	s.appliedAt = time.Now()
	s.categories = categorySet(cfg.Categories)
	s.exportOTLP = cfg.ExportOTLP
	s.excludedCdp = excludedSet(cfg.ExcludedCdpMethods)
	s.setActiveLocked()
}

// publishLocked stamps telemetry_session_id into ev.Source.Metadata and forwards to the bus.
// Requires s.mu to be held.
func (s *TelemetrySession) publishLocked(ev events.Event) events.Envelope {
	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMicro()
	}
	if ev.Source.Metadata == nil {
		m := make(map[string]string)
		ev.Source.Metadata = &m
	}
	(*ev.Source.Metadata)["telemetry_session_id"] = s.id
	return s.es.Publish(events.Envelope{Event: ev})
}

// Publish applies the telemetry config filter and forwards ev to the
// EventStream. Returns the assigned envelope and true on success, or a zero
// envelope and false when the event was dropped (session inactive or
// category disabled). Fire-and-forget callers can ignore the returns.
func (s *TelemetrySession) Publish(ev events.Event) (events.Envelope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == "" {
		return events.Envelope{}, false
	}
	if _, ok := s.categories[ev.Category]; !ok {
		return events.Envelope{}, false
	}
	return s.publishLocked(ev), true
}

// NewReader returns a Reader from the EventStream positioned after afterSeq.
func (s *TelemetrySession) NewReader(afterSeq uint64) *events.Reader {
	return s.es.NewReader(afterSeq)
}

// ID returns the current telemetry session ID, or "" if no session is active.
func (s *TelemetrySession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// RecordDropped notes that a consumer found a gap of n envelopes in the stream.
func (s *TelemetrySession) RecordDropped(n uint64) {
	s.es.RecordDropped(n)
}

// DroppedEvents returns the cumulative gap count across consumers.
func (s *TelemetrySession) DroppedEvents() uint64 {
	return s.es.DroppedEvents()
}

// Seq returns the sequence number of the last published event.
func (s *TelemetrySession) Seq() uint64 {
	return s.es.Seq()
}

// SessionStartSeq returns the sequence number at which the current session
// started. Fresh SSE connections with no Last-Event-ID should begin here.
func (s *TelemetrySession) SessionStartSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionStartSeq
}

// Config returns the current telemetry configuration.
func (s *TelemetrySession) Config() TelemetryConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := make([]oapi.TelemetryEventCategory, 0, len(s.categories))
	for c := range s.categories {
		cats = append(cats, c)
	}
	excluded := make([]oapi.BrowserCdpCommandMethod, 0, len(s.excludedCdp))
	for m := range s.excludedCdp {
		excluded = append(excluded, oapi.BrowserCdpCommandMethod(m))
	}
	sort.Slice(excluded, func(i, j int) bool { return excluded[i] < excluded[j] })
	return TelemetryConfig{Categories: cats, ExportOTLP: s.exportOTLP, ExcludedCdpMethods: excluded}
}

// AppliedAt returns when the current configuration was applied, or the zero
// time if telemetry is not configured.
func (s *TelemetrySession) AppliedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appliedAt
}

// UpdateConfig applies a new TelemetryConfig to the running session.
func (s *TelemetrySession) UpdateConfig(cfg TelemetryConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.categories = categorySet(cfg.Categories)
	s.exportOTLP = cfg.ExportOTLP
	s.excludedCdp = excludedSet(cfg.ExcludedCdpMethods)
	s.setActiveLocked()
}

// CategoryEnabled reports whether events in category c are currently captured.
// It returns false when no session is active. Lock-free, so a caller on the
// CDP forwarding path can check it per frame; Publish re-checks under mu.
func (s *TelemetrySession) CategoryEnabled(c oapi.TelemetryEventCategory) bool {
	cats := s.active.Load()
	if cats == nil {
		return false
	}
	_, ok := (*cats)[c]
	return ok
}

// ExcludedCdpMethods returns the methods left out of the cdp_command stream.
// Lock-free for the same reason CategoryEnabled is; the returned set is
// read-only and may be nil.
func (s *TelemetrySession) ExcludedCdpMethods() map[string]struct{} {
	excluded := s.excludedCdpActive.Load()
	if excluded == nil {
		return nil
	}
	return *excluded
}

// Active reports whether a telemetry session is currently running.
func (s *TelemetrySession) Active() bool {
	return s.active.Load() != nil
}

// Stop ends the current telemetry session. The ring buffer is left intact so
// existing readers can finish draining.
func (s *TelemetrySession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = ""
	s.appliedAt = time.Time{}
	// The session is over, so export is off; keep Config() authoritative for the
	// desired export state after a clear.
	s.exportOTLP = false
	s.setActiveLocked()
}
