package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// CaptureConfig holds caller-supplied capture preferences. All fields are optional;
// zero values mean "use server defaults" (all categories).
type CaptureConfig struct {
	Categories []EventCategory
}

// CaptureSession is the unified write path that fans events out to a FileWriter
// (durable, session-scoped) and a RingBuffer (in-memory SSE fan-out). It also
// tracks session identity, category filter, and timestamps so the API layer does
// not need a separate session-management type.
//
// Call Start to begin a session, Publish to forward events, Stop to end one.
// Close releases file descriptors. All methods are safe for concurrent use.
type CaptureSession struct {
	mu    sync.Mutex
	ring  *RingBuffer
	files *FileWriter
	seq   atomic.Uint64

	// session state, guarded by mu
	id         string
	categories map[EventCategory]struct{}
	createdAt  time.Time
}

func NewCaptureSession(ring *RingBuffer, files *FileWriter) *CaptureSession {
	return &CaptureSession{ring: ring, files: files}
}

// Start begins a new capture session. Subsequent Publish calls that match cfg's
// category filter will be written to the FileWriter in addition to the RingBuffer.
func (s *CaptureSession) Start(id string, cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = id
	s.createdAt = time.Now()
	cats := cfg.Categories
	if len(cats) == 0 {
		cats = AllCategories
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
	s.id = ""
}

// ID returns the active capture session ID, or "" if no session is running.
func (s *CaptureSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Config returns the current capture configuration.
func (s *CaptureSession) Config() CaptureConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := make([]EventCategory, 0, len(s.categories))
	for c := range s.categories {
		cats = append(cats, c)
	}
	return CaptureConfig{Categories: cats}
}

// UpdateConfig replaces the category filter for the running session.
func (s *CaptureSession) UpdateConfig(cfg CaptureConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats := cfg.Categories
	if len(cats) == 0 {
		cats = AllCategories
	}
	s.categories = make(map[EventCategory]struct{}, len(cats))
	for _, c := range cats {
		s.categories[c] = struct{}{}
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
	return s.seq.Load()
}

// Publish assigns a monotonically increasing sequence number, writes to the
// RingBuffer (always), and writes to the FileWriter if a session is active and
// the event's category is included in the session filter.
// Returns the Envelope as stored in the ring.
func (s *CaptureSession) Publish(ev Event) Envelope {
	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMilli()
	}
	if ev.DetailLevel == "" {
		ev.DetailLevel = DetailStandard
	}

	s.mu.Lock()
	sessionID := s.id
	_, catOK := s.categories[ev.Category]
	shouldWrite := sessionID != "" && catOK
	env := Envelope{
		CaptureSessionID: sessionID,
		Seq:              s.seq.Add(1),
		Event:            ev,
	}
	s.mu.Unlock()

	env, data := truncateIfNeeded(env)

	if shouldWrite {
		if data == nil {
			slog.Error("capture_session: marshal failed, skipping file write", "seq", env.Seq, "category", env.Event.Category)
		} else if err := s.files.Write(env, data); err != nil {
			slog.Error("capture_session: file write failed", "seq", env.Seq, "category", env.Event.Category, "err", err)
		}
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
