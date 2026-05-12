package events

import (
	"fmt"
	"sync"
)

// EventStream is the process-lifetime event bus. It owns the ring buffer and
// sequence counter, which outlive individual capture sessions.
type EventStream struct {
	mu   sync.Mutex
	seq  uint64
	ring *ringBuffer
}

type EventStreamConfig struct {
	// RingCapacity is the number of envelopes the ring buffer holds.
	RingCapacity int
}

func NewEventStream(cfg EventStreamConfig) (*EventStream, error) {
	rb, err := newRingBuffer(cfg.RingCapacity)
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
