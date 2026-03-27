package events

import (
	"sync"
	"sync/atomic"
	"time"
)

// Pipeline glues a RingBuffer and a FileWriter into a single write path.
// A single call to Publish stamps the event with a monotonic sequence number,
// applies truncation, durably appends it to the per-category log file, and
// then makes it available to ring buffer readers.
type Pipeline struct {
	mu               sync.Mutex
	ring             *RingBuffer
	files            *FileWriter
	seq              atomic.Uint64
	captureSessionID atomic.Pointer[string]
}

// NewPipeline returns a Pipeline backed by the supplied ring and file writer.
func NewPipeline(ring *RingBuffer, files *FileWriter) *Pipeline {
	p := &Pipeline{ring: ring, files: files}
	empty := ""
	p.captureSessionID.Store(&empty)
	return p
}

// Start sets the capture session ID that will be stamped on every subsequent
// published event. It may be called at any time; the change is immediately
// visible to concurrent Publish calls.
func (p *Pipeline) Start(captureSessionID string) {
	p.captureSessionID.Store(&captureSessionID)
}

// Publish stamps, truncates, files, and broadcasts a single event.
//
// Ordering:
//  1. Stamp CaptureSessionID, Seq, Ts (Ts only if caller left it zero)
//  2. Apply truncateIfNeeded — must happen before both sinks
//  3. Write to FileWriter (durable before in-memory)
//  4. Publish to RingBuffer (in-memory fan-out)
//
// The mutex serialises concurrent callers so that seq assignment and sink
// delivery are atomic — readers always see events in seq order.
// Errors from FileWriter.Write are silently dropped; the ring buffer always
// receives the event even if the file write fails.
func (p *Pipeline) Publish(ev BrowserEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ev.CaptureSessionID = *p.captureSessionID.Load()
	ev.Seq = p.seq.Add(1) // starts at 1
	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixMilli()
	}
	if ev.DetailLevel == "" {
		ev.DetailLevel = DetailDefault
	}
	ev = truncateIfNeeded(ev)

	_ = p.files.Write(ev)
	p.ring.Publish(ev)
}

// NewReader returns a Reader positioned at the start of the ring buffer.
func (p *Pipeline) NewReader() *Reader {
	return p.ring.NewReader()
}

// Close closes the underlying FileWriter, flushing and releasing all open
// file descriptors.
func (p *Pipeline) Close() error {
	return p.files.Close()
}
