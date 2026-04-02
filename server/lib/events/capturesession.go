package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// CaptureSession is a single-use write path that wraps events in envelopes and
// fans them out to a FileWriter (durable) and RingBuffer (in-memory). Call Start
// once with a capture session ID, then Publish concurrently. Close flushes the
// FileWriter; there is no restart or terminal event.
type CaptureSession struct {
	mu               sync.Mutex
	ring             *RingBuffer
	files            *FileWriter
	seq              atomic.Uint64
	captureSessionID atomic.Pointer[string]
}

func NewCaptureSession(ring *RingBuffer, files *FileWriter) *CaptureSession {
	s := &CaptureSession{ring: ring, files: files}
	empty := ""
	s.captureSessionID.Store(&empty)
	return s
}

// Start sets the capture session ID stamped on every subsequent envelope.
func (s *CaptureSession) Start(captureSessionID string) {
	s.captureSessionID.Store(&captureSessionID)
}

// Publish wraps ev in an Envelope, truncates if needed, then writes to
// FileWriter (durable) before RingBuffer (in-memory fan-out).
func (s *CaptureSession) Publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMilli()
	}
	if ev.DetailLevel == "" {
		ev.DetailLevel = DetailStandard
	}

	env := Envelope{
		CaptureSessionID: *s.captureSessionID.Load(),
		Seq:              s.seq.Add(1),
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
