# Browser Events Durable Storage Plan

---

## Overview

Browser Events captured by the image server (CDP, live view, computer control, captcha) are already written to per-category JSONL files and streamed over SSE. This plan adds a third sink: a cloud append-only log store.

---

## System Context

```
[CDP Monitor]  ──┐
[Computer API] ──┤─► CaptureSession.Publish ──► RingBuffer (fan-out)
[Extension]    ──┤                                    │
[Live View]    ──┘                    ┌───────────────┼──────────────┐
                                      │               │              │
                                  FileWriter     SSE handler   EventsStorageWriter ◄─ (new)
                                   (local)      (real-time)    (durable)
```

All three sinks consume from the same ring buffer. The ring buffer is non-blocking: writers never wait for any sink. Each sink holds an independent `Reader` cursor.

---

## Key Components

### `EventsStorageWriter` (`eventsstorage.go`)

The single goroutine that moves events from the ring to the configured backend. Single-use: `Run(ctx)` blocks until ctx is cancelled, then returns. `Close()` drains in-flight writes and tears down the backend.

### `EventsStorage` (interface in `eventsstorage.go`)

```go
type EventsStorage interface {
    // Append writes data to the named stream. The S2 backend relies on
    // the basin's create-stream-on-append feature.
    Append(ctx context.Context, streamName string, data []byte) error
    // Close flushes pending writes and releases resources.
    Close() error
}
```

The interface boundary between `EventsStorageWriter` and any specific backend. The mock implementation used in tests lives exclusively in `eventsstorage_writer_test.go`.

### S2 Storage (`s2storage.go`)

The production `EventsStorage` backed by S2. Lazily creates one S2 producer per capture session ID. The producer map is mutex-protected; `Append` is called serially from `EventsStorageWriter.Run`, but ack goroutines run concurrently.

**Producer lifecycle:** Producers are evicted when their capture session ends via `Remove(streamName string)`, called from the `POST /events/stop` handler. This prevents unbounded accumulation of producers across session cycles on long-running servers.

### `s2Producer`

Bundles one `s2.Producer` with a `sync.WaitGroup` that tracks in-flight ack goroutines. `Close()` calls `wg.Wait()` before closing the producer, ensuring no ack is orphaned.

---

## File Structure

```
server/lib/events/
  eventsstorage.go             # EventsStorage interface + EventsStorageWriter
  eventsstorage_writer_test.go # Tests via mockBackend — no S2 dependency
  s2storage.go                 # S2 implementation of EventsStorage
```

---

## Architectural Decisions

### 1. Stream name = capture session ID

Each capture session maps to a dedicated stream named by the session UUID. Streams are created automatically on first write (S2 does this via create-stream-on-append basin feature). This means:

- Replaying a session = reading one stream from seq 0
- Concurrent sessions write to separate streams with no coordination

### 2. Lazy producer creation with session-end eviction (S2 backend)

Producers are created on first `Append` for a given stream name and cached until the session ends. The `s2storage` exposes a `Remove(streamName)` method that drains and closes the producer for that stream. `POST /events/stop` calls `Remove` after the session is torn down. Preventing the producer map from growing unbounded on long-running servers that cycle through many capture sessions.

### 3. Batching: 100ms linger / 50 records (S2 backend)

The S2 SDK batcher coalesces records before flushing to the network. Configuration:

```
Linger:     100ms
MaxRecords: 50
```

These are independent of the ring buffer read loop — the writer appends one record per ring Read, and the batcher decides when to flush.

### 4. Feedback loop prevention

`EventsStorageWriter.Run` skips envelopes whose `Event.Type == EventsStorageError`. Without this, a error would re-enter the ring and be read by EventsStorageWriter causing churn. The constant `EventsStorageError` is defined in `event.go` and used in both the writer and the error-emit path to prevent typo-driven breakage.

### 5. 1MB record size limit and truncation

The S2 1MB per-record limit is enforced at `CaptureSession.Publish`, not at the EventsStorageWriter. `truncateIfNeeded` nulls `event.data` and sets `event.truncated=true` when the marshalled envelope exceeds `maxRecordBytes`. This ensures truncation applies equally to file logs and durable records.

### 6. Shutdown sequencing

Shutdown must be strictly ordered to avoid writing to a closed `FileWriter`:

```
1. ctx cancelled (SIGINT/SIGTERM)
2. EventsStorageWriter.Run returns (reader unblocks from cancelled ctx)
3. storageDone channel closes
4. storageWriter.Close() — drains in-flight S2 writes
5. apiService.Shutdown() — closes CaptureSession (and its FileWriter)
```

---

## Testing

`EventsStorageWriter` is tested exclusively through `mockBackend` (defined in `eventsstorage_writer_test.go`). Test cases cover:

| Scenario | What is verified |
| --- | --- |
| Normal append | Records routed to correct stream, deserialise back to `Envelope` |
| Ring buffer overflow (dropped) | Writer logs warning and skips; no crash |
| `Append` error | `publishFn` receives exactly one `system_durable_error` event |
| Context cancelled | `Run` returns `nil` (clean shutdown) |
| `EventsStorageError` skipped | Error events not re-submitted, preventing feedback loops |
| Marshal failure (oversized) | Writer skips and continues; next event is processed normally |

---