package events

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// CaptureConfig holds caller-supplied capture preferences. All fields are
// optional; zero values mean "use server defaults" (all categories, standard
// detail level).
type CaptureConfig struct {
	// Categories limits which event categories are captured. nil or empty
	// means all categories.
	Categories []EventCategory

	// DetailLevel overrides the default detail level stamped on events that
	// don't set their own. Empty means DetailStandard.
	DetailLevel DetailLevel
}

// CaptureSession wraps events in envelopes and fans them out to a FileWriter
// (durable) and RingBuffer (in-memory). Call Start to begin or restart a session,
// then Publish concurrently. Close flushes the FileWriter.
//
// Reusable: call Start with a new ID to begin a new session; call Stop to end
// the current session without closing the underlying writers. Close tears down
// file descriptors and should only be called during server shutdown.
type CaptureSession struct {
	mu               sync.Mutex
	ring             *RingBuffer
	files            *FileWriter
	seq              uint64
	captureSessionID string
	categories       map[EventCategory]struct{}
	detailLevel      DetailLevel // defaults to DetailStandard
	createdAt        time.Time
}

// CaptureSessionConfig holds the parameters for creating a CaptureSession.
type CaptureSessionConfig struct {
	// LogDir is the directory where per-category JSONL log files are written.
	LogDir string

	// RingCapacity is the number of envelopes the in-memory ring buffer holds.
	RingCapacity int
}

func NewCaptureSession(cfg CaptureSessionConfig) (*CaptureSession, error) {
	fw, err := NewFileWriter(cfg.LogDir)
	if err != nil {
		return nil, fmt.Errorf("capture session: %w", err)
	}
	all := AllCategories()
	cats := make(map[EventCategory]struct{}, len(all))
	for _, c := range all {
		cats[c] = struct{}{}
	}
	return &CaptureSession{
		ring:       NewRingBuffer(cfg.RingCapacity),
		files:      fw,
		categories: cats,
	}, nil
}

// Start sets the capture session ID and applies the given config. It resets
// the sequence counter so each session starts at seq 1.
// The FileWriter is intentionally not rotated: events from different sessions
// are interleaved in the same per-category JSONL files and distinguished by
// their envelope's capture_session_id.
func (s *CaptureSession) Start(captureSessionID string, cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureSessionID = captureSessionID
	s.seq = 0
	s.createdAt = time.Now()
	s.ring.Reset()
	s.detailLevel = cfg.DetailLevel
	cats := cfg.Categories
	if len(cats) == 0 {
		cats = AllCategories()
	}
	s.categories = make(map[EventCategory]struct{}, len(cats))
	for _, c := range cats {
		s.categories[c] = struct{}{}
	}
}

// Stop ends the current session. The ring buffer is left intact so existing
// readers can finish draining.
func (s *CaptureSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureSessionID = ""
}

// ID returns the active capture session ID, or "" if no session is running.
func (s *CaptureSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureSessionID
}

// Config returns the current capture configuration.
func (s *CaptureSession) Config() CaptureConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := make([]EventCategory, 0, len(s.categories))
	for c := range s.categories {
		cats = append(cats, c)
	}
	return CaptureConfig{Categories: cats, DetailLevel: s.detailLevel}
}

// UpdateConfig replaces the category filter and detail level for the running session.
func (s *CaptureSession) UpdateConfig(cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detailLevel = cfg.DetailLevel
	if len(cfg.Categories) > 0 {
		s.categories = make(map[EventCategory]struct{}, len(cfg.Categories))
		for _, c := range cfg.Categories {
			s.categories[c] = struct{}{}
		}
	} else {
		s.categories = nil
	}
}

// CreatedAt returns when the current session was started.
func (s *CaptureSession) CreatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdAt
}

// Seq returns the sequence number of the last published event.
func (s *CaptureSession) Seq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Publish assigns a monotonically increasing sequence number, writes to the
// RingBuffer (always), and writes to the FileWriter when a session is active
// and the event's category is in the filter.
// Returns the Envelope as stored in the ring.
func (s *CaptureSession) Publish(ev Event) Envelope {
	s.mu.Lock()

	// No active session — drop silently. This can happen when events
	// arrive between Stop() and producers noticing, or before Start().
	if s.captureSessionID == "" {
		s.mu.Unlock()
		return Envelope{}
	}

	// Drop events whose category is outside the configured set.
	if _, ok := s.categories[ev.Category]; !ok {
		s.mu.Unlock()
		return Envelope{}
	}

	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMicro()
	}
	if ev.DetailLevel == "" {
		if s.detailLevel != "" {
			ev.DetailLevel = s.detailLevel
		} else {
			ev.DetailLevel = DetailStandard
		}
	}

	sessionID := s.captureSessionID
	s.seq++
	env := Envelope{
		CaptureSessionID: sessionID,
		Seq:              s.seq,
		Event:            ev,
	}
	s.mu.Unlock()

	env, data := truncateIfNeeded(env)

	if data == nil {
		slog.Error("capture_session: marshal failed, skipping file write", "seq", env.Seq, "category", env.Event.Category)
	} else if err := s.files.Write(env, data); err != nil {
		slog.Error("capture_session: file write failed", "seq", env.Seq, "category", env.Event.Category, "err", err)
	}
	s.ring.Publish(env)
	return env
}

// NewReader returns a Reader positioned after afterSeq. Pass 0 to start from
// the oldest buffered event.
func (s *CaptureSession) NewReader(afterSeq uint64) *Reader {
	return s.ring.NewReader(afterSeq)
}

// Close flushes and releases all open file descriptors.
func (s *CaptureSession) Close() error {
	return s.files.Close()
}
