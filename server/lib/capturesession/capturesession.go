package capturesession

import (
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
)

// CaptureConfig holds caller-supplied capture preferences. All fields are
// optional; zero values mean "use server defaults" (all categories).
type CaptureConfig struct {
	// Categories limits which event categories are captured.
	// nil or empty includes all categories.
	Categories []events.EventCategory
}

// CaptureSession manages a capture session against a shared EventStream.
// It is responsible for (a) category-filtering Publish calls, (b) tracking
// session-scoped metadata (ID, config, timestamps), and (c) embedding
// capture_session_id into Event.Data before forwarding to the bus.
type CaptureSession struct {
	es               *events.EventStream
	mu               sync.Mutex
	captureSessionID string
	sessionStartSeq  uint64
	categories       map[events.EventCategory]struct{}
	createdAt        time.Time
}

func NewCaptureSession(es *events.EventStream) *CaptureSession {
	cats := make(map[events.EventCategory]struct{}, len(events.AllCategories))
	for _, c := range events.AllCategories {
		cats[c] = struct{}{}
	}
	return &CaptureSession{es: es, categories: cats}
}

// Start begins a new capture session with the given ID and config. Sequence
// numbers are process-monotonic and do not reset between sessions; a
// Last-Event-ID from any previous session is valid for resuming the SSE stream.
func (s *CaptureSession) Start(captureSessionID string, cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureSessionID = captureSessionID
	s.sessionStartSeq = s.es.Seq()
	s.createdAt = time.Now()
	cats := cfg.Categories
	if len(cats) == 0 {
		cats = events.AllCategories
	}
	s.categories = make(map[events.EventCategory]struct{}, len(cats))
	for _, c := range cats {
		s.categories[c] = struct{}{}
	}
}

// publishLocked stamps capture_session_id into ev.Source.Metadata and forwards to the bus.
// Requires s.mu to be held.
func (s *CaptureSession) publishLocked(ev events.Event) events.Envelope {
	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMicro()
	}
	if ev.Source.Metadata == nil {
		ev.Source.Metadata = make(map[string]string)
	}
	ev.Source.Metadata["capture_session_id"] = s.captureSessionID
	return s.es.Publish(events.Envelope{Event: ev})
}

// Publish applies the category filter then forwards ev to the EventStream.
func (s *CaptureSession) Publish(ev events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.captureSessionID == "" {
		return
	}
	if _, ok := s.categories[ev.Category]; !ok {
		return
	}
	s.publishLocked(ev)
}

// NewReader returns a Reader from the EventStream positioned after afterSeq.
func (s *CaptureSession) NewReader(afterSeq uint64) *events.Reader {
	return s.es.NewReader(afterSeq)
}

// ID returns the current capture session ID, or "" if no session is active.
func (s *CaptureSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureSessionID
}

// Seq returns the sequence number of the last published event.
func (s *CaptureSession) Seq() uint64 {
	return s.es.Seq()
}

// SessionStartSeq returns the sequence number at which the current session
// started. Fresh SSE connections with no Last-Event-ID should begin here.
func (s *CaptureSession) SessionStartSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionStartSeq
}

// Config returns the current capture configuration.
func (s *CaptureSession) Config() CaptureConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := make([]events.EventCategory, 0, len(s.categories))
	for c := range s.categories {
		cats = append(cats, c)
	}
	return CaptureConfig{Categories: cats}
}

// CreatedAt returns when the current session was started.
func (s *CaptureSession) CreatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdAt
}

// UpdateConfig applies a new CaptureConfig to the running session.
func (s *CaptureSession) UpdateConfig(cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := cfg.Categories
	if len(cats) == 0 {
		cats = events.AllCategories
	}
	s.categories = make(map[events.EventCategory]struct{}, len(cats))
	for _, c := range cats {
		s.categories[c] = struct{}{}
	}
}

// Active reports whether a capture session is currently running.
func (s *CaptureSession) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureSessionID != ""
}

// Stop ends the current session. The ring buffer is left intact so existing
// readers can finish draining.
func (s *CaptureSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureSessionID = ""
}
