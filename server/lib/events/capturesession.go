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

// CaptureSession is a single-use write path that wraps events in envelopes and
// fans them out to a FileWriter (durable) and RingBuffer (in-memory). Call Start
// once with a capture session ID, then Publish concurrently. Close flushes the
// FileWriter; there is no restart or terminal event.
type CaptureSession struct {
	mu               sync.Mutex
	ring             *RingBuffer
	files            *FileWriter
	seq              uint64
	captureSessionID string
	categories       map[EventCategory]struct{}
	detailLevel      DetailLevel                // defaults to DetailStandard
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

// Publish wraps ev in an Envelope, truncates if needed, then writes to
// FileWriter (durable) before RingBuffer (in-memory fan-out).
func (s *CaptureSession) Publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Drop events whose category is outside the configured set.
	if _, ok := s.categories[ev.Category]; !ok {
		return
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

	s.seq++
	env := Envelope{
		CaptureSessionID: s.captureSessionID,
		Seq:              s.seq,
		Event:            ev,
	}
	env, data := truncateIfNeeded(env)

	if data == nil {
		slog.Error("capture_session: marshal failed, skipping file write", "seq", env.Seq, "category", env.Event.Category)
	} else if err := s.files.Write(env, data); err != nil {
		slog.Error("capture_session: file write failed", "seq", env.Seq, "category", env.Event.Category, "err", err)
	}
	s.ring.Publish(env)
}

// NewReader returns a Reader positioned at the start of the ring buffer.
func (s *CaptureSession) NewReader(afterSeq uint64) *Reader {
	return s.ring.NewReader(afterSeq)
}

// Close flushes and releases all open file descriptors.
func (s *CaptureSession) Close() error {
	return s.files.Close()
}
