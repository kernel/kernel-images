# External Event Ingestion + SSE Streaming — Plan

**Scope:** Two new HTTP endpoints layered on top of the merged CDP base: `POST /events/publish` (external event ingestion) and `GET /events/stream` (SSE live stream), both wired into the existing resource-style `CaptureSession`.

---

## 1. Motivation

The CDP monitor produces a unified `events.Event` stream into a `CaptureSession` (ring buffer + seq assignment + optional file write). The base branch exposes the session as a resource:

```
GET    /events/capture_session     — read current session
POST   /events/capture_session     — start a session (409 if one is active)
PATCH  /events/capture_session     — update config
DELETE /events/capture_session     — stop the session
```

Gaps this plan closes:

1. External producers (kernel API callers, browser extensions, local processes) have no way to inject events into the same merged stream.
2. There is no live pull interface — consumers can read the session state but cannot subscribe to events in real time with reconnection support.

---

## 2. What's Changing

### 2.1 New endpoints

| Method | Path | Handler |
| --- | --- | --- |
| POST | `/events/publish` | `ApiService.PublishEvent` |
| GET | `/events/stream` | `ApiService.StreamEvents` (SSE) |

### 2.2 `POST /events/publish`

Accepts a JSON `events.Event` body and publishes it into the currently active `CaptureSession`.

**Publish requires an active capture session.** If no session is active the handler returns `400` — the caller must `POST /events/start` first. The session is a precondition for publishing, not an implicitly-created resource.

**Validation and status codes:**

- `400` when no capture session is active.
- `400` on invalid JSON body.
- `400` when `type` is empty.
- `200` on successful publish.

The handler unconditionally sets `source.kind = KindKernelAPI` **after** decoding, overwriting whatever the caller supplied. This is not a default — it is an enforcement. A caller that sends `"source": {"kind": "cdp"}` must not be able to forge CDP provenance; `source.kind` is documented as the fan-out key (§3.2) and its value for this endpoint is not negotiable.

The handler does not take `monitorMu` — `CaptureSession.Publish` is serialised internally and guarantees monotonic seq delivery. This creates a narrow TOCTOU: a concurrent `StopCapture` can clear the session ID between the `Active()` check and the `Publish()` call, causing the event to be written to the ring buffer with an empty `captureSessionID` (after the `session_ended` marker). This is accepted; grabbing `monitorMu` on every publish would serialize all external events behind CDP monitor start/stop.

### 2.3 `GET /events/stream`

Server-Sent Events endpoint backed by a `CaptureSession` reader. Supports multiple concurrent clients — each connection creates an independent `Reader` on the ring buffer.

**Frame format:**

```
id: {seq}
data: {envelope-json}
```

**Headers set:**

- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `X-Accel-Buffering: no` (disables nginx / reverse-proxy buffering)

After writing headers, the handler calls `flusher.Flush()` once before entering the read loop so the client's `EventSource.onopen` fires immediately rather than waiting for the first event.

**Read position.** A client chooses where to start reading:

- `afterSeq=0` (default) — read from the **oldest event still in the ring buffer**. This is the behaviour when no `Last-Event-ID` header is sent.
- `afterSeq=N` (via `Last-Event-ID: N` header) — resume after seq N. The reader returns the first envelope with `Seq > N`. Gap-tolerant: if events between the client's last seq and the oldest surviving seq were evicted, the reader reports a `Dropped` count and fast-forwards.

`Last-Event-ID: 0` and a missing `Last-Event-ID` header are treated identically — both start from the oldest buffered event. This is correct because synthetic `events_dropped` envelopes carry `Seq==0`; a reconnecting client that received only a dropped frame and sends `Last-Event-ID: 0` must not be fast-forwarded past real events.

**Multi-client fan-out.** The ring buffer's closed-channel broadcast means N concurrent readers are woken on every publish with no per-reader allocation on the write path. Each reader tracks its own `nextSeq` independently — a slow client does not block others or the publisher.

**No-session behaviour.** Returns `400` when no capture session is active, consistent with `/events/publish`.

**Client disconnect.** A consumer leaves the stream by closing the connection. The request context is cancelled, `reader.Read(ctx)` returns `ctx.Err()`, and the handler goroutine exits. No server-side cleanup is required — the `Reader` struct is GC'd once the goroutine returns.

**Flusher guard.** If the `ResponseWriter` does not implement `http.Flusher`, the handler returns `500` **before writing any headers** to avoid partial responses.

**Keepalive.** The handler emits a comment frame (`:\n\n`) when no event has been sent for 15 seconds. Without this, reverse proxies that enforce a read timeout (nginx default: `proxy_read_timeout 60s`, some load balancers shorter) tear down idle connections. `X-Accel-Buffering: no` suppresses proxy buffering but does not reset the read timeout. Implementation: wrap each `reader.Read` call in a `context.WithTimeout(ctx, 15*time.Second)`; a `DeadlineExceeded` result (when the parent `ctx` is still live) means no event arrived — write the comment frame, flush, and loop. The simple `for { reader.Read }` structure is preserved with no goroutine or channel needed.

**Lifecycle.** The handler loops on `reader.Read` with a 15s per-call deadline. It exits on three conditions: (1) the client disconnects — the parent `ctx` is cancelled and `reader.Read` returns `ctx.Err()`; (2) a `session_ended` envelope is received — the handler writes the final SSE frame and returns; (3) a write to the `ResponseWriter` fails — the handler returns. No server-side cleanup is required in any case.

### 2.4 `POST /events/stop` — session clearing

`StopCapture` must stop the entire capture session, not just the CDP monitor. Currently it calls `cdpMonitor.Stop()` but leaves the capture session state intact, so `Active()` remains true and subsequent publishes or stream connections succeed against a session that is conceptually stopped.

**Required change:** `StopCapture` calls `captureSession.Stop()` after `cdpMonitor.Stop()`. `CaptureSession.Stop()` does two things in order:

1. Publishes a synthetic `session_ended` envelope (type `"session_ended"`, source kind `"kernel_api"`) into the ring buffer — this is a real envelope with a real monotonic seq so every open `StreamEvents` connection receives it.
2. Clears the session ID so `Active()` returns false.

**Cross-session reader termination.** Without this boundary, a `Reader` blocked across a Stop→Start cycle wakes up on the first `Publish()` of the new session and silently delivers new-session envelopes to a connection that was opened on the old session (the `CaptureSessionID` in the envelope changes but no signal is sent). The `session_ended` envelope is the signal. `StreamEvents` exits its read loop after sending the `session_ended` frame, closing the connection cleanly. The client may then reconnect and open a new stream against the new session.

**Seq counter.** `Stop()` does **not** reset the seq counter — seq remains monotonic across start/stop cycles within the same process lifetime. This avoids any possibility of a reconnecting SSE client receiving a lower seq than its last `Last-Event-ID`.

### 2.5 OpenAPI spec entries

Both endpoints must be added to `server/openapi.yaml` under `paths:` alongside the existing `/events/capture_session` block.

`POST /events/publish` — standard JSON request/response:

```yaml
  /events/publish:
    post:
      summary: Publish an external event into the active capture session
      operationId: PublishEvent
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Event'
      responses:
        '200':
          description: Event accepted and published
        '400':
          description: No active capture session, invalid JSON, or missing `type`
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
```

`GET /events/stream` — SSE long-lived response. OpenAPI 3.x has no native SSE type; document the response as `text/event-stream` with a string schema and describe the frame format in `description:`:

```yaml
  /events/stream:
    get:
      summary: Stream capture-session events as Server-Sent Events
      operationId: StreamEvents
      parameters:
        - in: header
          name: Last-Event-ID
          schema:
            type: string
          required: false
          description: Resume after this sequence number. Omit to start from the oldest buffered event.
      responses:
        '200':
          description: Live stream of capture-session events. A synthetic `events_dropped` event is sent when events were evicted from the buffer before the client could read them.
          content:
            text/event-stream:
              schema:
                type: string
        '400':
          description: No active capture session
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '500':
          description: ResponseWriter does not support flushing
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
```

---

## 3. Key Design Decisions

1. **Shared pipeline, not a new queue.** External events flow through the same `CaptureSession` as CDP events, so `seq` is globally monotonic across all sources. Consumers never have to merge streams.

2. `source.kind` **is the fan-out key.** `kernel_api` for publish, `cdp` for the monitor, `extension` / `local_process` reserved for future producers.

3. **SSE over WebSocket.** SSE is one-way, proxy-friendly, and has built-in reconnection semantics (`Last-Event-ID`) that map cleanly to our `seq` cursor. No extra framing library.

4. **Direct writes, no goroutine.** `StreamEvents` writes straight to the `ResponseWriter` from the request goroutine. No `io.Pipe`, no background worker — correct for HTTP/1.1 SSE and simpler to reason about on disconnect.

5. **Seq==0 skip on fast-forward.** Synthetic `events_dropped` envelopes carry `Seq==0`; the `Last-Event-ID` seek skips them so they never advance the cursor past a real event.

6. **400 when no session.** Both endpoints return `400` if no capture session is active. The session is a precondition — the caller must start one first. `400` (No active capture session). Note: `GET /events/capture_session` (base branch) returns `404` when no session exists — clients must handle both codes depending on which endpoint they call.

7. **Multi-client streaming.** Each `GET /events/stream` creates an independent ring buffer `Reader`. Readers are woken via closed-channel broadcast — O(1) on the publish side regardless of subscriber count. A slow reader falls behind and gets a `Dropped` notification; it never blocks the publisher or other readers.

8. **CaptureSession active-state tracking.** `Start()` sets the session ID; `Stop()` clears it — `StopCapture` calls both `cdpMonitor.Stop()` and `captureSession.Stop()`. `Active() bool` is a required new method implemented under `s.mu.Lock()` (consistent with all other methods on the struct): `s.mu.Lock(); defer s.mu.Unlock(); return s.captureSessionID != ""`. Do **not** use `atomic.Pointer` — a double-Load pattern (`Load() != nil && *Load() != ""`) introduces a TOCTOU nil dereference if a concurrent `Stop()` zeroes the pointer between the two loads, and the existing mutex already provides the necessary synchronisation at no additional cost. `Active()` does not exist yet and is a prerequisite for both `PublishEvent` and `StreamEvents`. Seq is **not** reset on stop — it stays monotonic across start/stop cycles so reconnecting SSE clients never see a lower seq than their last `Last-Event-ID`.

9. `session_ended` **envelope for cross-session reader safety.** A `Reader` held open across Stop→Start wakes silently on the new session's first `Publish` and starts delivering new-session envelopes. Context cancellation on Stop would require storing per-connection cancel funcs in `ApiService`. Instead, `Stop()` publishes a synthetic `session_ended` envelope (real seq, `source.kind = "kernel_api"`) before clearing the session ID. `StreamEvents` exits its loop after sending this frame. This keeps the disconnect path entirely in the event stream — no shared mutable state in the handler, no cancel-func bookkeeping.