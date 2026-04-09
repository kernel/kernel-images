package events

import (
	"context"
	"fmt"
	"sync"
)

// ringBuffer is a fixed-capacity circular buffer with closed-channel broadcast fan-out.
// Writers never block regardless of reader count or speed.
type ringBuffer struct {
	mu         sync.RWMutex
	buf        []Envelope
	cap        uint64
	latestSeq  uint64         // highest envelope.Seq published
	readerWake chan struct{}   // closed-and-replaced on each Publish to wake blocked readers
}

func newRingBuffer(capacity int) (*ringBuffer, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("events: ring buffer capacity must be > 0, got %d", capacity)
	}
	return &ringBuffer{
		buf:        make([]Envelope, capacity),
		cap:        uint64(capacity),
		readerWake: make(chan struct{}),
	}, nil
}

// reset clears the buffer and wakes any blocked readers so they re-evaluate
// against the new (empty) state. Readers will reposition to seq 1 on the next
// Read call and block until fresh publishes arrive.
func (rb *ringBuffer) reset() {
	rb.mu.Lock()
	for i := range rb.buf {
		rb.buf[i] = Envelope{}
	}
	rb.latestSeq = 0
	old := rb.readerWake
	rb.readerWake = make(chan struct{})
	rb.mu.Unlock()
	close(old)
}

// publish adds an envelope to the ring, evicting the oldest on overflow.
func (rb *ringBuffer) publish(env Envelope) {
	rb.mu.Lock()
	rb.buf[env.Seq%rb.cap] = env
	rb.latestSeq = env.Seq
	old := rb.readerWake
	rb.readerWake = make(chan struct{})
	rb.mu.Unlock()
	close(old)
}

func (rb *ringBuffer) oldestSeq() uint64 {
	if rb.latestSeq <= rb.cap {
		return 1
	}
	return rb.latestSeq - rb.cap + 1
}

// newReader returns a Reader. afterSeq == 0 starts from the oldest available
// envelope; afterSeq > 0 resumes after that seq.
func (rb *ringBuffer) newReader(afterSeq uint64) *Reader {
	return &Reader{rb: rb, nextSeq: afterSeq + 1}
}

// ReadResult is returned by Reader.Read. Exactly one of Envelope or Dropped is
// set: Envelope is non-nil for a normal read, Dropped is non-zero when the
// reader fell behind and events were lost.
type ReadResult struct {
	Envelope *Envelope
	Dropped  uint64
}

// Reader tracks an independent read position in a ringBuffer.
type Reader struct {
	rb      *ringBuffer
	nextSeq uint64
}

// Read blocks until the next envelope is available or ctx is cancelled.
func (r *Reader) Read(ctx context.Context) (ReadResult, error) {
	for {
		r.rb.mu.RLock()
		wake := r.rb.readerWake
		latest := r.rb.latestSeq
		oldest := r.rb.oldestSeq()

		if latest == 0 {
			// Buffer is empty (or was just reset). Reset reader position
			// so it starts from the beginning when new data arrives.
			r.nextSeq = 1
			r.rb.mu.RUnlock()
			select {
			case <-ctx.Done():
				return ReadResult{}, ctx.Err()
			case <-wake:
				continue
			}
		}

		if r.nextSeq < oldest {
			dropped := oldest - r.nextSeq
			r.nextSeq = oldest
			r.rb.mu.RUnlock()
			return ReadResult{Dropped: dropped}, nil
		}

		if r.nextSeq <= latest {
			env := r.rb.buf[r.nextSeq%r.rb.cap]
			r.nextSeq++
			r.rb.mu.RUnlock()
			return ReadResult{Envelope: &env}, nil
		}

		r.rb.mu.RUnlock()

		select {
		case <-ctx.Done():
			return ReadResult{}, ctx.Err()
		case <-wake:
		}
	}
}
