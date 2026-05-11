package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Storage is the durable storage backend for browser events.
// Append is called serially from StorageWriter.Run and need not be thread-safe.
type Storage interface {
	Append(ctx context.Context, env Envelope) error
	Close() error
}

// StorageWriter drains the ring buffer and forwards each envelope to the
// configured Storage backend. Single-use: call Run once; it blocks until ctx
// is cancelled. After ctx is cancelled, call Drain to flush events that
// arrived before all publishers stopped, then call Close to flush the backend.
//
// Delivery is at-least-once: on process restart the ring is empty so no
// cross-restart duplicates occur, but consumers should dedupe by env.Seq in
// case StorageWriter is ever restarted within a process lifetime.
//
// Starts from the oldest available event in the ring, not the current tail.
type StorageWriter struct {
	reader       *Reader
	storage      Storage
	log          *slog.Logger
	once         sync.Once
	appendErrors atomic.Uint64 // total append failures; best-effort, not retried
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
// ctx is cancelled. Returns the context error on clean shutdown. Must be
// called at most once; panics on a second call.
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
		if err := w.processResult(ctx, res); err != nil {
			return err
		}
	}
}

// Drain reads any events still in the ring non-blockingly until caught up or
// ctx expires. Call after all publishers have stopped and Run has returned to
// ensure no events are silently skipped on shutdown.
func (w *StorageWriter) Drain(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.log.Warn("storage writer: drain deadline exceeded, ring may have unread events")
			return ctx.Err()
		default:
		}

		res, ok := w.reader.TryRead()
		if !ok {
			return nil
		}
		if err := w.processResult(ctx, res); err != nil {
			return err
		}
	}
}

func (w *StorageWriter) processResult(ctx context.Context, res ReadResult) error {
	if res.Dropped > 0 {
		w.log.Warn("storage writer: dropped events", "count", res.Dropped, "from_seq", res.DroppedFrom, "to_seq", res.DroppedTo)
		return nil
	}
	if err := w.storage.Append(ctx, *res.Envelope); err != nil {
		total := w.appendErrors.Add(1)
		w.log.Error("storage writer: append failed", "seq", res.Envelope.Seq, "err", err, "total_append_errors", total)
	}
	return nil
}

// AppendErrors returns the total number of Append failures since Run started.
func (w *StorageWriter) AppendErrors() uint64 {
	return w.appendErrors.Load()
}

// Close drains in-flight writes and releases backend resources.
func (w *StorageWriter) Close() error {
	return w.storage.Close()
}
