package telemetry

import (
	"sync"
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
	appliedAt       time.Time
}

func NewTelemetrySession(es *events.EventStream) *TelemetrySession {
	if es == nil {
		panic("telemetry: NewTelemetrySession requires a non-nil EventStream")
	}
	return &TelemetrySession{es: es, categories: categorySet(nil)}
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
	return TelemetryConfig{Categories: cats}
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
}

// CategoryEnabled reports whether events in category c are currently captured.
// It returns false when no session is active.
func (s *TelemetrySession) CategoryEnabled(c oapi.TelemetryEventCategory) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == "" {
		return false
	}
	_, ok := s.categories[c]
	return ok
}

// Active reports whether a telemetry session is currently running.
func (s *TelemetrySession) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id != ""
}

// Stop ends the current telemetry session. The ring buffer is left intact so
// existing readers can finish draining.
func (s *TelemetrySession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = ""
	s.appliedAt = time.Time{}
}
