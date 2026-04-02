package events

import (
	"log/slog"
	"sync"
	"time"
)

// CaptureSession is a single-use write path that wraps events in envelopes and
// fans them out to a FileWriter (durable) and RingBuffer (in-memory). Publish
// concurrently; Close flushes the FileWriter.
type CaptureSession struct {
	mu               sync.Mutex
	ring             *RingBuffer
	files            *FileWriter
	seq              uint64
	captureSessionID string
}

func NewCaptureSession(captureSessionID string, ring *RingBuffer, files *FileWriter) *CaptureSession {
	return &CaptureSession{ring: ring, files: files, captureSessionID: captureSessionID}
}

// Publish wraps ev in an Envelope, truncates if needed, then writes to
// FileWriter (durable) before RingBuffer (in-memory fan-out).
func (s *CaptureSession) Publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMicro()
	}
	if ev.DetailLevel == "" {
		ev.DetailLevel = DetailStandard
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
