package events

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// CaptureConfig holds caller-supplied capture preferences. All fields are
// optional; zero values mean "use server defaults" (all categories).
type CaptureConfig struct {
	// Categories limits which event categories are captured
	// nil represents all categories.
	Categories []EventCategory
}

// CaptureSession wraps events in envelopes and fans them out to a fileWriter
// Reusable: call Start with a new ID to begin a new session; call Stop to end
// the current session without closing the underlying writers. Close tears down
// file descriptors and should only be called during server shutdown.
type CaptureSession struct {
	mu               sync.Mutex
	ring             *ringBuffer
	files            *fileWriter
	seq              uint64
	captureSessionID string
	categories       map[EventCategory]struct{}
	createdAt        time.Time
}

// CaptureSessionConfig holds the parameters for creating a CaptureSession.
type CaptureSessionConfig struct {
	LogDir string
	// RingCapacity is the number of envelopes the in-memory ring buffer holds.
	RingCapacity int
}

func NewCaptureSession(cfg CaptureSessionConfig) (*CaptureSession, error) {
	rb, err := newRingBuffer(cfg.RingCapacity)
	if err != nil {
		return nil, fmt.Errorf("capture session: %w", err)
	}
	fw, err := newFileWriter(cfg.LogDir)
	if err != nil {
		return nil, fmt.Errorf("capture session: %w", err)
	}
	cats := make(map[EventCategory]struct{}, len(allCategories))
	for _, c := range allCategories {
		cats[c] = struct{}{}
	}
	return &CaptureSession{
		ring:       rb,
		files:      fw,
		categories: cats,
	}, nil
}

// Start sets the capture session ID and applies the given config. It resets
// the sequence counter so each session starts at seq 1.
// The fileWriter is intentionally not rotated: events from different sessions
// are interleaved in the same per-category JSONL files and distinguished by
// their envelope's capture_session_id.
func (s *CaptureSession) Start(captureSessionID string, cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureSessionID = captureSessionID
	s.seq = 0
	s.createdAt = time.Now()
	s.ring.reset()
	cats := cfg.Categories
	if len(cats) == 0 {
		cats = allCategories
	}
	s.categories = make(map[EventCategory]struct{}, len(cats))
	for _, c := range cats {
		s.categories[c] = struct{}{}
	}
}

// Publish wraps ev in an Envelope, truncates if needed, then writes to
// fileWriter (durable) before RingBuffer (in-memory fan-out).
func (s *CaptureSession) Publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// No active session, drop silently. This can happen when events
	// arrive between Stop() and producers noticing, or before Start().
	if s.captureSessionID == "" {
		return
	}

	// Drop events whose category is outside the configured set.
	if _, ok := s.categories[ev.Category]; !ok {
		return
	}

	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMicro()
	}

	s.seq++
	env := Envelope{
		CaptureSessionID: s.captureSessionID,
		Seq:              s.seq,
		Event:            ev,
	}
	env, data := truncateIfNeeded(env)

	if data == nil {
		slog.Error("capture_session: marshal failed, skipping file write", "seq", env.Seq, "category", env.Event.Category)
	} else {
		filename := string(env.Event.Category) + ".log"
		if err := s.files.Write(filename, data); err != nil {
			slog.Error("capture_session: file write failed", "seq", env.Seq, "category", env.Event.Category, "err", err)
		}
	}
	s.ring.publish(env)
}

// NewReader returns a Reader positioned at the start of the ring buffer.
func (s *CaptureSession) NewReader(afterSeq uint64) *Reader {
	return s.ring.newReader(afterSeq)
}

// ID returns the current capture session ID, or "" if no session is active.
func (s *CaptureSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureSessionID
}

// Seq returns the current sequence number (last published).
func (s *CaptureSession) Seq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Config returns the current capture configuration.
func (s *CaptureSession) Config() CaptureConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := make([]EventCategory, 0, len(s.categories))
	for c := range s.categories {
		cats = append(cats, c)
	}
	return CaptureConfig{
		Categories: cats,
	}
}

// CreatedAt returns when the current session was started.
func (s *CaptureSession) CreatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdAt
}

// UpdateConfig applies a new CaptureConfig to the running session without
// resetting the sequence counter or ring buffer.
func (s *CaptureSession) UpdateConfig(cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := cfg.Categories
	if len(cats) == 0 {
		cats = allCategories
	}
	s.categories = make(map[EventCategory]struct{}, len(cats))
	for _, c := range cats {
		s.categories[c] = struct{}{}
	}
}

// Stop ends the current session by clearing the session ID. The ring buffer
// is intentionally left intact so existing readers can finish draining.
// A new session can be started by calling Start again.
func (s *CaptureSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureSessionID = ""
}

// Close flushes and releases all open file descriptors.
func (s *CaptureSession) Close() error {
	return s.files.Close()
}
