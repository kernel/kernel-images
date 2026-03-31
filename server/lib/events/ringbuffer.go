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
	mu        sync.RWMutex
	buf       []Envelope
	cap       uint64
	latestSeq uint64         // highest envelope.Seq published
	readerWake chan struct{} // closed-and-replaced on each Publish to wake blocked readers
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		buf:        make([]Envelope, capacity),
		cap:        uint64(capacity),
		readerWake: make(chan struct{}),
	}
}

// Publish adds an envelope to the ring, evicting the oldest on overflow.
func (rb *RingBuffer) Publish(env Envelope) {
	rb.mu.Lock()
	rb.buf[env.Seq%rb.cap] = env
	rb.latestSeq = env.Seq
	old := rb.readerWake
	rb.readerWake = make(chan struct{})
	rb.mu.Unlock()
	close(old)
}

func (rb *RingBuffer) oldestSeq() uint64 {
	if rb.latestSeq <= rb.cap {
		return 1
	}
	return rb.latestSeq - rb.cap + 1
}

// NewReader returns a Reader. afterSeq == 0 starts from the oldest available
// envelope; afterSeq > 0 resumes after that seq.
func (rb *RingBuffer) NewReader(afterSeq uint64) *Reader {
	nextSeq := afterSeq + 1
	if afterSeq == 0 {
		nextSeq = 1
	}
	return &Reader{rb: rb, nextSeq: nextSeq}
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
		wake := r.rb.readerWake
		latest := r.rb.latestSeq
		oldest := r.rb.oldestSeq()

		if latest == 0 {
			r.rb.mu.RUnlock()
			select {
			case <-ctx.Done():
				return Envelope{}, ctx.Err()
			case <-wake:
				continue
			}
		}

		if r.nextSeq < oldest {
			dropped := oldest - r.nextSeq
			r.nextSeq = oldest
			r.rb.mu.RUnlock()
			data := json.RawMessage(fmt.Sprintf(`{"dropped":%d}`, dropped))
			return Envelope{
				Event: Event{Type: "events.dropped", Category: CategorySystem, Source: Source{Kind: KindKernelAPI}, Data: data},
			}, nil
		}

		if r.nextSeq <= latest {
			env := r.rb.buf[r.nextSeq%r.rb.cap]
			r.nextSeq++
			r.rb.mu.RUnlock()
			return env, nil
		}

		r.rb.mu.RUnlock()

		select {
		case <-ctx.Done():
			return Envelope{}, ctx.Err()
		case <-wake:
		}
	}
}
