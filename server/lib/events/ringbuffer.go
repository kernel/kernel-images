package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// RingBuffer is a fixed-capacity circular buffer with closed-channel broadcast fan-out.
// Writers never block regardless of reader count or speed.
type RingBuffer struct {
	mu      sync.RWMutex
	buf     []Envelope
	head    int    // next write position (mod cap)
	written uint64 // total ever published (monotonic)
	notify  chan struct{}
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		buf:    make([]Envelope, capacity),
		notify: make(chan struct{}),
	}
}

// Publish adds an envelope to the ring, evicting the oldest on overflow.
func (rb *RingBuffer) Publish(env Envelope) {
	rb.mu.Lock()
	rb.buf[rb.head] = env
	rb.head = (rb.head + 1) % len(rb.buf)
	rb.written++
	old := rb.notify
	rb.notify = make(chan struct{})
	rb.mu.Unlock()
	close(old)
}

func (rb *RingBuffer) oldestSeq() uint64 {
	if rb.written <= uint64(len(rb.buf)) {
		return 0
	}
	return rb.written - uint64(len(rb.buf))
}

// NewReader returns a Reader positioned at publish index 0.
func (rb *RingBuffer) NewReader() *Reader {
	return &Reader{rb: rb, nextSeq: 0}
}

// Reader tracks an independent read position in a RingBuffer.
type Reader struct {
	rb      *RingBuffer
	nextSeq uint64
}

// Read blocks until the next envelope is available or ctx is cancelled.
// When the reader has fallen behind, a synthetic drop event is returned.
func (r *Reader) Read(ctx context.Context) (Envelope, error) {
	for {
		r.rb.mu.RLock()
		notify := r.rb.notify
		oldest := r.rb.oldestSeq()
		written := r.rb.written

		if r.nextSeq < oldest {
			dropped := oldest - r.nextSeq
			r.nextSeq = oldest
			r.rb.mu.RUnlock()
			data := json.RawMessage(fmt.Sprintf(`{"dropped":%d}`, dropped))
			return Envelope{
				Event: Event{Type: "events.dropped", Category: CategorySystem, Source: Source{Kind: KindKernelAPI}, Data: data},
			}, nil
		}

		if r.nextSeq < written {
			idx := int(r.nextSeq % uint64(len(r.rb.buf)))
			env := r.rb.buf[idx]
			r.nextSeq++
			r.rb.mu.RUnlock()
			return env, nil
		}

		r.rb.mu.RUnlock()

		select {
		case <-ctx.Done():
			return Envelope{}, ctx.Err()
		case <-notify:
		}
	}
}
