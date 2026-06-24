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
	latestSeq  uint64        // highest envelope.Seq published
	readerWake chan struct{} // closed-and-replaced when blocked readers need a wakeup
	waiters    int
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
	var old chan struct{}
	if rb.waiters > 0 {
		old = rb.readerWake
		rb.readerWake = make(chan struct{})
	}
	rb.mu.Unlock()
	if old != nil {
		close(old)
	}
}

func (rb *ringBuffer) publishLocked(env Envelope) chan struct{} {
	rb.buf[env.Seq%rb.cap] = env
	rb.latestSeq = env.Seq
	if rb.waiters == 0 {
		return nil
	}
	old := rb.readerWake
	rb.readerWake = make(chan struct{})
	return old
}

func (rb *ringBuffer) closeWake(old chan struct{}) {
	if old != nil {
		close(old)
	}
}

// publish adds an envelope to the ring, evicting the oldest on overflow.
func (rb *ringBuffer) publish(env Envelope) {
	rb.mu.Lock()
	old := rb.publishLocked(env)
	rb.mu.Unlock()
	rb.closeWake(old)
}

func (rb *ringBuffer) publishNext(env Envelope) Envelope {
	rb.mu.Lock()
	env.Seq = rb.latestSeq + 1
	env, _ = truncateIfNeeded(env)
	old := rb.publishLocked(env)
	rb.mu.Unlock()
	rb.closeWake(old)
	return env
}

func (rb *ringBuffer) seq() uint64 {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.latestSeq
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

// TryRead returns the next available result without blocking. Returns
// (result, true) if data is available, (ReadResult{}, false) if the reader
// has caught up to the latest published seq.
func (r *Reader) TryRead() (ReadResult, bool) {
	r.rb.mu.RLock()
	defer r.rb.mu.RUnlock()

	latest := r.rb.latestSeq
	oldest := r.rb.oldestSeq()

	if latest == 0 || r.nextSeq > latest {
		return ReadResult{}, false
	}

	if r.nextSeq < oldest {
		dropped := oldest - r.nextSeq
		r.nextSeq = oldest
		return ReadResult{Dropped: dropped}, true
	}

	env := r.rb.buf[r.nextSeq%r.rb.cap]
	r.nextSeq++
	return ReadResult{Envelope: &env}, true
}

// Read blocks until the next envelope is available or ctx is cancelled.
func (r *Reader) Read(ctx context.Context) (ReadResult, error) {
	for {
		r.rb.mu.Lock()
		wake := r.rb.readerWake
		latest := r.rb.latestSeq
		oldest := r.rb.oldestSeq()

		if latest == 0 {
			// Buffer is empty (or was just reset). Reset reader position
			// so it starts from the beginning when new data arrives.
			r.nextSeq = 1
			r.rb.waiters++
			r.rb.mu.Unlock()
			err := waitForWake(ctx, wake)
			r.rb.mu.Lock()
			r.rb.waiters--
			r.rb.mu.Unlock()
			if err != nil {
				return ReadResult{}, ctx.Err()
			}
			continue
		}

		if r.nextSeq < oldest {
			dropped := oldest - r.nextSeq
			r.nextSeq = oldest
			r.rb.mu.Unlock()
			return ReadResult{Dropped: dropped}, nil
		}

		if r.nextSeq <= latest {
			env := r.rb.buf[r.nextSeq%r.rb.cap]
			r.nextSeq++
			r.rb.mu.Unlock()
			return ReadResult{Envelope: &env}, nil
		}

		r.rb.waiters++
		r.rb.mu.Unlock()
		err := waitForWake(ctx, wake)
		r.rb.mu.Lock()
		r.rb.waiters--
		r.rb.mu.Unlock()
		if err != nil {
			return ReadResult{}, ctx.Err()
		}
	}
}

func waitForWake(ctx context.Context, wake <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	}
}
