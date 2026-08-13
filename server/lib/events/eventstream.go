package events

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// EventStream is the process-lifetime event bus. It owns the ring buffer and
// sequence counter, which outlive individual capture sessions.
type EventStream struct {
	mu   sync.Mutex
	seq  uint64
	ring *ringBuffer
	// dropped counts envelopes a consumer missed because it fell behind the
	// ring, summed across consumers and sessions. Loss is per-consumer, so this
	// is a pressure signal rather than a count of distinct lost events.
	dropped atomic.Uint64
}

type EventStreamConfig struct {
	// RingCapacity is the number of envelopes the ring buffer holds.
	RingCapacity int
	// RingMaxBytes bounds the memory those envelopes may occupy. Zero uses
	// DefaultRingMaxBytes. Capacity alone does not bound memory: a slot holds
	// anything from a small control event to a base64 screenshot.
	RingMaxBytes uint64
}

// DefaultRingMaxBytes bounds the ring when a caller does not choose. Sized to
// hold a full ring of control events comfortably while keeping a run of
// screenshot-sized ones from costing capacity times the largest envelope.
const DefaultRingMaxBytes = 64 << 20

func NewEventStream(cfg EventStreamConfig) (*EventStream, error) {
	maxBytes := cfg.RingMaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultRingMaxBytes
	}
	rb, err := newRingBuffer(cfg.RingCapacity, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("event stream: %w", err)
	}
	return &EventStream{ring: rb}, nil
}

// Publish assigns a monotonically increasing seq to env, truncates oversized
// payloads, and pushes it to the ring buffer.
func (es *EventStream) Publish(env Envelope) Envelope {
	es.mu.Lock()
	es.seq++
	env.Seq = es.seq
	es.mu.Unlock()

	env, _ = truncateIfNeeded(env)
	es.ring.publish(env)
	return env
}

// RecordDropped notes that a consumer found a gap of n envelopes. Consumers
// report it rather than the ring detecting it, because only a consumer knows
// what it had already read.
func (es *EventStream) RecordDropped(n uint64) {
	es.dropped.Add(n)
}

// DroppedEvents returns the cumulative gap count across consumers, so a reader
// can tell a quiet stream from one it is falling behind.
func (es *EventStream) DroppedEvents() uint64 {
	return es.dropped.Load()
}

// NewReader returns a Reader positioned after afterSeq. Pass 0 to start from
// the oldest buffered event.
func (es *EventStream) NewReader(afterSeq uint64) *Reader {
	return es.ring.newReader(afterSeq)
}

// Seq returns the sequence number of the last published event.
func (es *EventStream) Seq() uint64 {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.seq
}
