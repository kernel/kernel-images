package events

import (
	"context"
	"log/slog"
)

// EventsStorage is the durable storage backend for browser events.
type EventsStorage interface {
	Append(ctx context.Context, env Envelope) error
	Close() error
}

// EventsStorageWriter drains the ring buffer and forwards each envelope to
// the configured EventsStorage backend. It is designed to run as a single
// goroutine via Run.
type EventsStorageWriter struct {
	reader  *Reader
	storage EventsStorage
}

// NewEventsStorageWriter creates a writer that reads from es starting at seq 0.
func NewEventsStorageWriter(es *EventStream, storage EventsStorage) *EventsStorageWriter {
	return &EventsStorageWriter{
		reader:  es.NewReader(0),
		storage: storage,
	}
}

// Run reads from the ring buffer and appends each envelope to storage until
// ctx is cancelled. It returns nil on clean shutdown.
func (w *EventsStorageWriter) Run(ctx context.Context) error {
	for {
		res, err := w.reader.Read(ctx)
		if err != nil {
			// ctx cancelled — clean shutdown
			return nil
		}
		if res.Dropped > 0 {
			slog.Warn("events storage writer: dropped events", "count", res.Dropped)
			continue
		}
		if err := w.storage.Append(ctx, *res.Envelope); err != nil {
			slog.Error("events storage writer: append failed", "seq", res.Envelope.Seq, "err", err)
		}
	}
}

// Close drains in-flight writes and releases backend resources.
func (w *EventsStorageWriter) Close() error {
	return w.storage.Close()
}
