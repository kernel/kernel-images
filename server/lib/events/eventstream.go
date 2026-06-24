package events

import (
	"fmt"
)

// EventStream is the process-lifetime event bus. Its ring buffer and sequence
// counter outlive individual capture sessions.
type EventStream struct {
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
	return es.ring.publishNext(env)
}

// NewReader returns a Reader positioned after afterSeq. Pass 0 to start from
// the oldest buffered event.
func (es *EventStream) NewReader(afterSeq uint64) *Reader {
	return es.ring.newReader(afterSeq)
}

// Seq returns the sequence number of the last published event.
func (es *EventStream) Seq() uint64 {
	return es.ring.seq()
}
