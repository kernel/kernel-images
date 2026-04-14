# External Event Ingestion + SSE Streaming — Plan

**Scope:** Two new HTTP endpoints layered on top of the merged CDP base: `POST /events/capture_session/publish` (external event ingestion) and `GET /events/capture_session/stream` (SSE live stream), both wired into the existing resource-style `CaptureSession`.

---

## 1. Motivation

The CDP monitor produces a unified `events.Event` stream into a `CaptureSession` (ring buffer + seq assignment + optional file write). The base branch exposes the session as a resource:

```
GET    /events/capture_session     — read current session
POST   /events/capture_session     — start a session
PATCH  /events/capture_session     — update config
DELETE /events/capture_session     — stop the session
```

Gaps this plan closes:

1. External producers (kernel API callers, browser extensions, local processes) have no way to inject events into the same merged stream.
2. There is no live pull interface — consumers can read the session state but cannot subscribe to events in real time with reconnection support.

Both new endpoints share the existing `CaptureSession` — no new storage, no new transport, no new schema.

---

## 2. What's Changing

### 2.1 New endpoints

| Method | Path | Handler | operationId |
| --- | --- | --- | --- |
| POST | `/events/capture_session/publish` | `ApiService.PublishEvent` | `publishEvent` |
| GET | `/events/capture_session/stream` | `ApiService.StreamCaptureSession` (SSE) | `streamCaptureSession` |

The stream endpoint follows the same singleton pattern as the other `/events/capture_session` routes. Handlers reference `s.captureSession` directly; the endpoint returns 404 when no session is active.

Both registered in `server/cmd/api/main.go` alongside the `/events/capture_session` routes from the base branch.

### 2.2 `POST /events/capture_session/publish`

Accepts a JSON `events.Event` body and publishes it into the currently active `CaptureSession`.

**Defaults applied server-side when caller omits them:**

- `source.kind` — stamped to `kernel_api` when absent, so downstream consumers can distinguish external traffic from CDP traffic.

**Validation and status codes:**

- `400` on invalid JSON body.
- `400` when `type` is empty.
- `400` when `category` is absent or not in the known set (`events.ValidCategory`). External callers always know their category; derivation is not performed.
- `404` when no capture session is active (consistent with the resource model — publish has no implicit session).
- `200` on successful publish.

The handler does not take `monitorMu` — `CaptureSession.Publish` is serialised internally and guarantees monotonic seq delivery.

**Request** — `type` and `category` are required:

```json
POST /events/capture_session/publish
Content-Type: application/json

{
  "type": "network_request",
  "category": "network",
  "ts": 1713100000000000,
  "source": {
    "kind": "kernel_api",
    "event": "fetch",
    "metadata": { "request_id": "abc123" }
  },
  "detail_level": "standard",
  "url": "https://example.com/api/data",
  "data": { "method": "GET", "status": 200 }
}
```

Minimal valid request:

```json
{ "type": "network_request", "category": "network" }
```

**Response** `200` — pipeline stamps `seq` and `capture_session_id`, returns the full `Envelope`:

```json
{
  "capture_session_id": "sess_01j...",
  "seq": 42,
  "event": {
    "ts": 1713100000000000,
    "type": "network_request",
    "category": "network",
    "source": { "kind": "kernel_api" },
    "detail_level": "standard",
    "url": "https://example.com/api/data",
    "data": { "method": "GET", "status": 200 }
  }
}
```

**Capture-session category filter interaction.** The base branch introduced `CaptureConfig.Categories` to let callers filter what the CDP monitor records. External publishes are **not** filtered by this config — an explicit publish is treated as a caller intent and always reaches the pipeline. (Open question §6.5.)

### 2.3 `GET /events/capture_session/stream`

Server-Sent Events endpoint backed by the singleton `CaptureSession`. Follows the same pattern as `GET /events/capture_session` — no path parameter, operates on the currently active session, returns 404 when none is active.

**Frame format:**

```
id: {seq}
data: {envelope-json}
```

**Headers set:**

- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `X-Accel-Buffering: no` (disables nginx / reverse-proxy buffering)

**Reconnection.** Honours the `Last-Event-ID` request header. On reconnect the client passes the last `seq` it saw; the handler constructs `captureSession.NewReader(lastSeq)` which resumes at the first envelope with `Seq > lastSeq` — gap-tolerant, does not require `lastSeq+1`.

**No-session behaviour.** Returns `404` when no capture session is active, matching the resource-style semantics of the base branch.

**Flusher guard.** If the `ResponseWriter` does not implement `http.Flusher`, the handler returns `500` **before writing any headers** to avoid partial responses.

**Lifecycle.** The handler loops on `reader.Read(ctx)` bound to the request context; when the client disconnects, the read returns and the goroutine exits cleanly.

---

## 3. Key Design Decisions

1. **Shared pipeline, not a new queue.** External events flow through the same `CaptureSession` as CDP events, so `seq` is globally monotonic across all sources. Consumers never have to merge streams.
2. `**source.kind` is the fan-out key.\*\* `kernel_api` for publish, `cdp` for the monitor, `extension` / `local_process` reserved for future producers. Category is a required caller-supplied field; source is the precise provenance.
3. **Publish does not honour** `CaptureConfig.Categories`**.** The config is a filter on what the CDP monitor records — an explicit publish is a deliberate caller action and bypasses it.
4. **SSE over WebSocket.** SSE is one-way, proxy-friendly, and has built-in reconnection semantics (`Last-Event-ID`) that map cleanly to our `seq` cursor. No extra framing library.
5. **Direct writes, no goroutine.** `StreamEvents` writes straight to the `ResponseWriter` from the request goroutine. No `io.Pipe`, no background worker — correct for HTTP/1.1 SSE and simpler to reason about on disconnect.
6. **Seq==0 skip on fast-forward.** Synthetic `events_dropped` envelopes carry `Seq==0`; the `Last-Event-ID` seek skips them so they never advance the cursor past a real event.
7. **Envelope size cap enforced in the pipeline, not the handler.** `truncateIfNeeded` (1 MB limit) lives on the publish path; `PublishEvent` does not re-check size.
8. **404 when no session.** Both endpoints 404 if no capture session is active, consistent with the resource model. Publishes are not buffered and streams do not wait.

---

---

## 4. Testing

- Unit tests in `events_publish_test.go` and `events_stream_test.go` run against a real `ApiService` + `CaptureSession` (no mocks).
- Race: `go test ./... -race` passes for the whole server module.
- SSE tests use an `httptest.Server` + a small SSE client that parses `id:` / `data:` frames and asserts ordering and content.
- `Last-Event-ID` reconnection is exercised by: publish N events → stream receives them → disconnect → publish M more → reconnect with last `seq` → assert stream resumes at seq N+1 (or the first surviving seq if the ring dropped events).
- No-session 404 is covered for both endpoints.

---