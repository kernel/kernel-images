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
	readerWake chan struct{} // closed-and-replaced on each Publish to wake blocked readers

	// A slot holds anything from a 200-byte cdp_command to a base64 screenshot,
	// so a capacity in envelopes bounds the count and not the memory. maxBytes
	// bounds the memory: enough slots for a burst of small events, without a
	// run of large ones costing capacity times the largest envelope.
	maxBytes uint64
	bytes    uint64
	// floorSeq is the oldest seq still held after byte eviction. Readers below
	// it get a gap, exactly as they do for eviction by count.
	floorSeq uint64
}

func newRingBuffer(capacity int, maxBytes uint64) (*ringBuffer, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("events: ring buffer capacity must be > 0, got %d", capacity)
	}
	if maxBytes == 0 {
		return nil, fmt.Errorf("events: ring buffer byte budget must be > 0")
	}
	return &ringBuffer{
		buf:        make([]Envelope, capacity),
		cap:        uint64(capacity),
		maxBytes:   maxBytes,
		floorSeq:   1,
		readerWake: make(chan struct{}),
	}, nil
}

// envelopeBytes approximates what an envelope costs to hold. The payload
// dominates; the rest is a fixed handful of scalars and short strings.
func envelopeBytes(env Envelope) uint64 {
	if env.Seq == 0 {
		return 0
	}
	return uint64(len(env.Event.Data)) + envelopeOverheadBytes
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
	rb.bytes = 0
	rb.floorSeq = 1
	old := rb.readerWake
	rb.readerWake = make(chan struct{})
	rb.mu.Unlock()
	close(old)
}

// publish adds an envelope to the ring, evicting the oldest on overflow of
// either bound: the slot count, or the byte budget.
func (rb *ringBuffer) publish(env Envelope) {
	rb.mu.Lock()
	slot := env.Seq % rb.cap
	// The slot may already hold an envelope this publish is evicting by count.
	rb.bytes -= envelopeBytes(rb.buf[slot])
	rb.buf[slot] = env
	rb.bytes += envelopeBytes(env)
	rb.latestSeq = env.Seq
	rb.evictForBytesLocked()
	old := rb.readerWake
	rb.readerWake = make(chan struct{})
	rb.mu.Unlock()
	close(old)
}

// evictForBytesLocked drops the oldest envelopes until the ring is inside its
// byte budget, always keeping the newest so a publish is never a no-op.
// Requires rb.mu.
func (rb *ringBuffer) evictForBytesLocked() {
	for rb.bytes > rb.maxBytes && rb.floorSeq < rb.latestSeq {
		slot := rb.floorSeq % rb.cap
		// A slot whose seq has moved on was already evicted by count, and its
		// bytes left the total when it was overwritten.
		if rb.buf[slot].Seq == rb.floorSeq {
			rb.bytes -= envelopeBytes(rb.buf[slot])
			rb.buf[slot] = Envelope{}
		}
		rb.floorSeq++
	}
}

func (rb *ringBuffer) oldestSeq() uint64 {
	oldest := uint64(1)
	if rb.latestSeq > rb.cap {
		oldest = rb.latestSeq - rb.cap + 1
	}
	if rb.floorSeq > oldest {
		oldest = rb.floorSeq
	}
	return oldest
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

// envelopeOverheadBytes approximates the non-payload cost of a held envelope:
// the seq, timestamps, type and category strings, and the source metadata.
const envelopeOverheadBytes = 256
