package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// RingBuffer is a fixed-capacity circular buffer with closed-channel broadcast fan-out.
// Writers never block regardless of reader count or speed.
// Readers track their position by seq value (not ring index) and receive an
// events_dropped synthetic BrowserEvent when they fall behind the oldest retained event.
type RingBuffer struct {
	mu      sync.RWMutex
	buf     []BrowserEvent
	head    int    // next write position (mod cap)
	written uint64 // total ever published (monotonic)
	notify  chan struct{}
}

// NewRingBuffer creates a new RingBuffer with the given capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		buf:    make([]BrowserEvent, capacity),
		notify: make(chan struct{}),
	}
}

// Publish adds an event to the ring buffer, evicting the oldest entry on overflow.
// Closes the current notify channel (waking all waiting readers) and replaces it
// with a new one — outside the lock to avoid blocking under contention.
func (rb *RingBuffer) Publish(ev BrowserEvent) {
	rb.mu.Lock()
	rb.buf[rb.head] = ev
	rb.head = (rb.head + 1) % len(rb.buf)
	rb.written++
	old := rb.notify
	rb.notify = make(chan struct{})
	rb.mu.Unlock()
	close(old) // outside lock to avoid blocking under contention
}

// oldestSeq returns the seq of the oldest event still in the ring.
// Must be called under at least a read lock.
func (rb *RingBuffer) oldestSeq() uint64 {
	if rb.written <= uint64(len(rb.buf)) {
		return 0
	}
	return rb.written - uint64(len(rb.buf))
}

// NewReader returns a Reader positioned at publish index 0 (the very beginning of the ring).
// If the ring has already published events, the reader will receive an
// events_dropped BrowserEvent on the first Read call if it has fallen behind
// the oldest retained event.
func (rb *RingBuffer) NewReader() *Reader {
	return &Reader{rb: rb, nextSeq: 0}
}

// Reader tracks an independent read position in a RingBuffer.
// A Reader must not be used concurrently from multiple goroutines.
//
// nextSeq is a monotonic count of publishes consumed by this reader — it is
// an index into the ring, not the BrowserEvent.Seq field.
type Reader struct {
	rb      *RingBuffer
	nextSeq uint64 // publish index, not BrowserEvent.Seq
}

// Read blocks until the next event is available or ctx is cancelled.
// Returns (event, nil) for a normal event.
// Returns (events_dropped BrowserEvent, nil) if the reader has fallen behind
// the ring's oldest retained event — the dropped count is in Data as valid JSON.
func (r *Reader) Read(ctx context.Context) (BrowserEvent, error) {
	for {
		r.rb.mu.RLock()
		notify := r.rb.notify
		oldest := r.rb.oldestSeq()
		written := r.rb.written

		// Reader fell behind — synthesize events_dropped before advancing.
		if r.nextSeq < oldest {
			dropped := oldest - r.nextSeq
			r.nextSeq = oldest
			r.rb.mu.RUnlock()
			data := json.RawMessage(fmt.Sprintf(`{"dropped":%d}`, dropped))
			return BrowserEvent{Type: "events.dropped", Category: CategorySystem, SourceKind: SourceKernelAPI, Data: data}, nil
		}

		// Event is available — read it.
		if r.nextSeq < written {
			idx := int(r.nextSeq % uint64(len(r.rb.buf)))
			ev := r.rb.buf[idx]
			r.nextSeq++
			r.rb.mu.RUnlock()
			return ev, nil
		}

		// No event yet — wait for notification.
		r.rb.mu.RUnlock()

		select {
		case <-ctx.Done():
			return BrowserEvent{}, ctx.Err()
		case <-notify:
			// new event available; loop to read it
		}
	}
}
