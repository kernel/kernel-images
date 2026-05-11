package events

import (
	"context"
	"log/slog"
	"sync"
)

// Storage is the durable storage backend for browser events.
// Append is called serially from StorageWriter.Run and need not be thread-safe.
type Storage interface {
	Append(ctx context.Context, env Envelope) error
	Close() error
}

// StorageWriter drains the ring buffer and forwards each envelope to the
// configured Storage backend. Single-use: call Run once; it blocks until ctx
// is cancelled. Call Close after Run returns to flush in-flight writes.
// Starts from the oldest available event in the ring, not the current tail.
type StorageWriter struct {
	reader  *Reader
	storage Storage
	log     *slog.Logger
	once    sync.Once
}

// NewStorageWriter creates a writer that reads from es starting at seq 0.
func NewStorageWriter(es *EventStream, storage Storage, log *slog.Logger) *StorageWriter {
	return &StorageWriter{
		reader:  es.NewReader(0),
		storage: storage,
		log:     log,
	}
}

// Run reads from the ring buffer and appends each envelope to storage until
// ctx is cancelled. Returns ctx.Err() on clean shutdown. Must be called at
// most once; panics on a second call.
func (w *StorageWriter) Run(ctx context.Context) error {
	firstCall := false
	w.once.Do(func() { firstCall = true })
	if !firstCall {
		panic("events: StorageWriter.Run called more than once")
	}

	for {
		res, err := w.reader.Read(ctx)
		if err != nil {
			return err
		}
		if res.Dropped > 0 {
			w.log.Warn("storage writer: dropped events", "count", res.Dropped, "from_seq", res.DroppedFrom, "to_seq", res.DroppedTo)
			continue
		}
		if err := w.storage.Append(ctx, *res.Envelope); err != nil {
			w.log.Error("storage writer: append failed", "seq", res.Envelope.Seq, "err", err)
		}
	}
}

// Close drains in-flight writes and releases backend resources.
func (w *StorageWriter) Close() error {
	return w.storage.Close()
}
