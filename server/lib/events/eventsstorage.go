package events

import (
	"context"
	"encoding/json"
	"log/slog"
)

// EventsStorage is the durable backend interface for the storage writer.
type EventsStorage interface {
	Append(ctx context.Context, streamName string, data []byte) error
	Close() error
}

// sessionRemover is implemented by backends that support per-session cleanup.
type sessionRemover interface {
	Remove(streamName string)
}

// EventsStorageWriter reads envelopes from the ring buffer and writes them to
// an EventsStorage backend. A single goroutine drives the write loop; call
// Run to start it and Close to drain in-flight writes after Run returns.
type EventsStorageWriter struct {
	session *CaptureSession
	backend EventsStorage
}

// NewEventsStorageWriter constructs a writer backed by the given storage.
func NewEventsStorageWriter(session *CaptureSession, backend EventsStorage) *EventsStorageWriter {
	return &EventsStorageWriter{session: session, backend: backend}
}

// Run reads from the ring buffer until ctx is cancelled. It returns nil on
// clean shutdown (ctx.Err()) and must not be called concurrently.
func (w *EventsStorageWriter) Run(ctx context.Context) {
	reader := w.session.NewReader(0)
	for {
		result, err := reader.Read(ctx)
		if err != nil {
			return
		}
		if result.Dropped > 0 {
			slog.Warn("events_storage_writer: ring buffer overflow, events dropped",
				"count", result.Dropped)
			continue
		}
		env := result.Envelope
		if env.Event.Type == EventsStorageError {
			// Skip re-queued error events to prevent feedback loops.
			continue
		}
		if env.CaptureSessionID == "" {
			continue
		}
		data, err := json.Marshal(env)
		if err != nil {
			slog.Error("events_storage_writer: marshal failed, skipping",
				"seq", env.Seq, "err", err)
			continue
		}
		if err := w.backend.Append(ctx, env.CaptureSessionID, data); err != nil {
			slog.Error("events_storage_writer: append failed",
				"seq", env.Seq, "stream", env.CaptureSessionID, "err", err)
			errData, _ := json.Marshal(map[string]string{"error": err.Error()})
			w.session.PublishUnfiltered(Event{
				Type:     EventsStorageError,
				Category: CategorySystem,
				Source:   Source{Kind: KindLocalProcess},
				Data:     errData,
			})
		}
	}
}

// Close drains in-flight writes and tears down the backend. Call after Run
// returns to ensure all pending records are flushed.
func (w *EventsStorageWriter) Close() error {
	return w.backend.Close()
}

// RemoveSession evicts the backend's producer for the given session ID,
// allowing it to drain and release resources. No-op if the backend does not
// implement sessionRemover.
func (w *EventsStorageWriter) RemoveSession(id string) {
	if r, ok := w.backend.(sessionRemover); ok {
		r.Remove(id)
	}
}
