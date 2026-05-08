package events

import (
	"fmt"
	"log/slog"
	"sync"
)

// EventStream is the process-lifetime event bus. It owns the ring buffer, file
// writer, and sequence counter.
type EventStream struct {
	mu    sync.Mutex
	seq   uint64
	ring  *ringBuffer
	files *fileWriter
}

// EventStreamConfig holds the parameters for creating an EventStream.
type EventStreamConfig struct {
	LogDir string
	// RingCapacity is the number of envelopes the ring buffer holds.
	RingCapacity int
}

func NewEventStream(cfg EventStreamConfig) (*EventStream, error) {
	rb, err := newRingBuffer(cfg.RingCapacity)
	if err != nil {
		return nil, fmt.Errorf("event stream: %w", err)
	}
	fw, err := newFileWriter(cfg.LogDir)
	if err != nil {
		return nil, fmt.Errorf("event stream: %w", err)
	}
	return &EventStream{ring: rb, files: fw}, nil
}

// publish assigns a monotonically increasing seq to env, writes it to the
// per-category JSONL file, and pushes it to the ring buffer. Called by
// CaptureSession under its own lock; env must already have CaptureSessionID set.
func (es *EventStream) publish(env Envelope) Envelope {
	es.mu.Lock()
	es.seq++
	env.Seq = es.seq
	es.mu.Unlock()

	env, data := truncateIfNeeded(env)
	if data == nil {
		slog.Error("event_stream: marshal failed, skipping file write", "seq", env.Seq, "category", env.Event.Category)
	} else {
		filename := string(env.Event.Category) + ".log"
		if err := es.files.Write(filename, data); err != nil {
			slog.Error("event_stream: file write failed", "seq", env.Seq, "category", env.Event.Category, "err", err)
		}
	}
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

// Close flushes and releases all open file descriptors.
func (es *EventStream) Close() error {
	return es.files.Close()
}
