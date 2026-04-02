package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Pipeline is a single-use write path that wraps events in envelopes and fans
// them out to a FileWriter (durable) and RingBuffer (in-memory). Call Start
// once with a capture session ID, then Publish concurrently. Close flushes the
// FileWriter; there is no restart or terminal event.
type Pipeline struct {
	mu               sync.Mutex
	ring             *RingBuffer
	files            *FileWriter
	seq              atomic.Uint64
	captureSessionID atomic.Pointer[string]
}

func NewPipeline(ring *RingBuffer, files *FileWriter) *Pipeline {
	p := &Pipeline{ring: ring, files: files}
	empty := ""
	p.captureSessionID.Store(&empty)
	return p
}

// Start sets the capture session ID stamped on every subsequent envelope.
func (p *Pipeline) Start(captureSessionID string) {
	p.captureSessionID.Store(&captureSessionID)
}

// Publish wraps ev in an Envelope, truncates if needed, then writes to
// FileWriter (durable) before RingBuffer (in-memory fan-out).
func (p *Pipeline) Publish(ev Event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMilli()
	}
	if ev.DetailLevel == "" {
		ev.DetailLevel = DetailStandard
	}

	env := Envelope{
		CaptureSessionID: *p.captureSessionID.Load(),
		Seq:              p.seq.Add(1),
		Event:            ev,
	}
	env, data := truncateIfNeeded(env)

	if data == nil {
		slog.Error("pipeline: marshal failed, skipping file write", "seq", env.Seq, "category", env.Event.Category)
	} else if err := p.files.Write(env, data); err != nil {
		slog.Error("pipeline: file write failed", "seq", env.Seq, "category", env.Event.Category, "err", err)
	}
	p.ring.Publish(env)
}

// NewReader returns a Reader positioned at the start of the ring buffer.
func (p *Pipeline) NewReader(afterSeq uint64) *Reader {
	return p.ring.NewReader(afterSeq)
}

// Close flushes and releases all open file descriptors.
func (p *Pipeline) Close() error {
	return p.files.Close()
}
