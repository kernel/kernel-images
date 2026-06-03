# CDP Monitor

The monitor is the browser-facing layer of the kernel browser logging pipeline. It connects to Chrome's DevTools endpoint, tracks all page sessions via CDP's `Target.setAutoAttach`, and converts raw CDP notifications into typed `events.Event` values for downstream consumers.

## Overview

`cdpmonitor` manages a Chrome DevTools Protocol (CDP) WebSocket connection to a running Chrome browser. It subscribes to CDP events across all attached tabs, translates them into structured `events.Event` values, and publishes them via a caller-supplied `PublishFunc`. It also derives synthetic events from sequences of CDP events and takes screenshots on significant page activity.

Chrome can restart independently of the monitor. When that happens, `UpstreamProvider` pushes a new DevTools URL and the monitor reconnects automatically, emitting lifecycle events so consumers can track continuity.

## Event taxonomy

**CDP-derived** (1-to-1 with a CDP notification): `console_log`, `console_error`, `network_request`, `network_response`, `network_loading_failed`, `page_tab_opened`, `page_navigation`, `page_dom_content_loaded`, `page_load`, `page_layout_shift`, `page_lcp`

**Computed** (inferred from sequences of CDP events): `network_idle` (fires when in-flight requests drop to zero), `page_layout_settled` (1 s after `page_load` with no intervening layout shifts), `page_navigation_settled` (fires once `page_dom_content_loaded` and `page_layout_settled` have both fired for the same navigation; intentionally independent of `network_idle` so that a single hung request cannot stall the event).

**Interaction** (fired by `interaction.js` via `Runtime.bindingCalled`): `interaction_click`, `interaction_key`, `interaction_scroll_settled`

**Monitor lifecycle** (emitted by the monitor itself, not by Chrome): `monitor_screenshot`, `monitor_disconnected`, `monitor_reconnected`, `monitor_reconnect_failed`, `monitor_init_failed`

## Responsibilities

| Concern | Where |
| --- | --- |
| WebSocket lifecycle (connect, read, reconnect) | `monitor.go` |
| CDP domain setup per session | `domains.go` |
| Event translation (CDP params to `events.Event`) | `handlers.go` |
| Synthetic event state machines | `computed.go` |
| Screenshot capture via ffmpeg | `screenshot.go` |
| CDP protocol types | `cdp_proto.go`, `types.go` |
| Interaction tracking injected into the page | `interaction.js` |
| Body/MIME capture sizing, text truncation, and typed payload helpers | `util.go` |

## Internals

### Reconnect model

`subscribeToUpstream` listens to `UpstreamProvider.Subscribe()` for new DevTools URLs. On each URL change (indicating Chrome restarted), `handleUpstreamRestart` tears down the existing connection, dials the new URL with capped-exponential backoff (250 ms → 500 ms → 1 s → 2 s, up to 10 attempts), then restarts `readLoop` and re-initializes all CDP sessions. `restartMu` serializes concurrent restart signals so rapid Chrome restarts do not produce overlapping reconnects.

### Goroutines

| Goroutine | Lifetime | Tracked by |
| --- | --- | --- |
| `readLoop` | one per WebSocket connection | `done` channel |
| `subscribeToUpstream` | same as `lifecycleCtx` | `asyncWg` |
| `sweepPendingRequests` | same as `lifecycleCtx` | `asyncWg` |
| `initSession` | short-lived, one per connect or reconnect | `asyncWg` |
| `attachExistingTargets` wrapper | short-lived, one per existing target on reconnect | `asyncWg` |
| `enableDomains` + `injectScript` | short-lived, one per target attach | `asyncWg` |
| `fetchResponseBody` | one per completed network request | `asyncWg` |
| `captureScreenshot` | one per screenshot trigger | `asyncWg` |

`Stop()` cancels `lifecycleCtx`, waits for `readLoop` via `done`, then waits for all other goroutines via `asyncWg` before closing the connection.

### Lock ordering

Locks must be acquired left to right. Never hold a lock on the left while acquiring one further right.

```
restartMu -> lifeMu -> pendReqMu -> computed.mu -> pendMu
restartMu -> lifeMu -> sessionsMu
```

`computed.mu` and `sessionsMu` are never held simultaneously; `cs.stop()` and `cs.resetOnNavigation()` are called only after the relevant `sessionsMu` critical section is complete.

`bindingRateMu` is independent of this ordering and is always acquired alone.

| Lock | Protects |
| --- | --- |
| `restartMu` | Serializes `handleUpstreamRestart` to prevent overlapping reconnects from rapid Chrome restarts |
| `lifeMu` | `conn`, `lifecycleCtx`, `cancel`, `done`, `readReady`: all fields that change during Start / Stop / reconnect |
| `pendReqMu` | `pendingRequests` (requestId -&gt; `networkReqState`): in-flight network requests accumulating request/response metadata until `loadingFinished` |
| `computed.mu` | All `computedState` fields: counters and timers for the `network_idle`, `page_layout_settled`, and `page_navigation_settled` state machines |
| `pendMu` | `pending` (id -&gt; reply channel): in-flight CDP commands waiting for a response from Chrome |
| `sessionsMu` | `sessions` (sessionID -&gt; `targetInfo`): the set of currently attached CDP targets (tabs, iframes, workers) |
| `bindingRateMu` | `bindingLastSeen` (sessionID:eventType -&gt; time): rate-limit state for `__kernelEvent` binding calls |

Fields that need no mutex use `sync/atomic`: `nextID`, `mainSessionID`, `running`, `lastScreenshotAt`, `screenshotInFlight`.

### WebSocket concurrency

`coder/websocket` guarantees one concurrent `Read` and one concurrent `Write` are safe on the same connection. `readLoop` is the sole reader. All writes go through `send`, which calls `conn.Write` directly; `conn.Write` is internally serialized by the library, so no external write mutex is needed.

## Event data model

### Envelope and top-level fields

Every event arrives as an `Envelope`:

```json
{
  "seq": 42,
  "event": {
    "ts": 1746123456789000,
    "type": "network_request",
    "category": "network",
    "source": {
      "kind": "cdp",
      "event": "Network.requestWillBeSent",
      "metadata": {
        "telemetry_session_id": "cs_abc123",
        "cdp_session_id": "...",
        "target_id": "...",
        "target_type": "page"
      }
    },
    "data": { ... },
    "truncated": false
  }
}
```

| Field | Type | Description |
| --- | --- | --- |
| `seq` | uint64 | Process-monotonic sequence number; does not reset across telemetry config changes. |
| `event.ts` | int64 | Wall-clock time the monitor emitted the event, as **Unix microseconds** (µs since epoch). |
| `event.type` | string | See [Event taxonomy](#event-taxonomy). |
| `event.category` | string | Emitted by this monitor: `console`, `network`, `page`, `interaction`, `screenshot`, `monitor` (collector health). |
| `event.truncated` | bool | `true` if `data` was nulled to fit the 1 MB pipeline limit. |
| `event.source.metadata.telemetry_session_id` | string | Pipeline-assigned ID for the telemetry session, stamped by the telemetry layer. |

### Source object

```json
"source": {
  "kind": "cdp",
  "event": "Network.requestWillBeSent",
  "metadata": {
    "cdp_session_id": "...",
    "target_id": "...",
    "target_type": "page"
  }
}
```

| Field | Description |
| --- | --- |
| `event` | The raw CDP method that triggered the event (e.g. `Network.requestWillBeSent`). Empty for computed events. |
| `metadata.cdp_session_id` | The CDP WebSocket session multiplexer ID for this target. Changes if Chrome restarts. |
| `metadata.target_id` | Stable identifier for the browser target (tab/window). Survives navigations within the same tab. |
| `metadata.target_type` | Target type as reported by Chrome: `page`, `iframe`, `worker`, etc. |

### CDP identity primer

Five IDs appear across events. Understanding how they nest prevents confusion:

```
target_id          <- one per tab/window; stable across navigations
└── cdp_session_id <- WebSocket multiplexer channel to that target; resets on Chrome restart
    └── frame_id   <- one per frame (top-level or iframe); changes on navigation
        └── loader_id  <- one per document load; links a navigation to its network requests
            └── request_id <- one per request (stable across redirects in a chain)
```

| ID | Where it appears | What it identifies |
| --- | --- | --- |
| `target_id` | `source.metadata`, most `data` objects | The browser tab. Use this to group all events from one tab session. |
| `cdp_session_id` | `source.metadata` | The WebSocket sub-channel. Not stable across reconnects. |
| `frame_id` | `page_navigation`, `network_request`, `network_response`, `network_loading_failed` | The frame the request or navigation belongs to. Top-level frame has no `parent_frame_id`. |
| `source_frame_id` | `page_layout_shift`, `page_lcp` | The frame where the layout shift or LCP element occurred. Distinct from the nav context `frame_id`, which is always the top-level navigated frame. |
| `loader_id` | `page_navigation`, `network_request`, `network_response` | The document load that owns a request. Join `network_request.loader_id` to `page_navigation.loader_id` to correlate requests with the navigation that triggered them. |
| `request_id` | `network_request`, `network_response`, `network_loading_failed` | A single request chain (including redirects). Links request to its eventual response or failure. |

### Navigation context fields

Most event `data` objects include a nav context block stamped at the last `page_navigation`. These fields reflect the top-level frame most recently navigated in the session:

| Field | Description |
| --- | --- |
| `session_id` | Same as `source.metadata.cdp_session_id`. Repeated for data-only consumers. |
| `frame_id` | Frame ID of the navigated top-level frame. |
| `loader_id` | Loader ID of the current document. |
| `url` | URL of the current page at the time of the last navigation. |
| `nav_seq` | Monotonically increasing counter, incremented on each `page_navigation`. Use it to detect that the page has navigated between two events in the same session. For `network_request`/`network_response`/`network_loading_failed`, the `nav_seq` is captured at request-send time and carried forward to the response so a request/response pair always shares an epoch. |

### Events that do not compose `BrowserEventContext`

Of the 22 event types, two intentionally omit the standard nav context block:

- `page_tab_opened`: fires before a CDP session is attached to the new target, so `session_id`, `frame_id`, `loader_id`, and `nav_seq` are absent. Only `target_id`, `target_type`, and the tab's initial `url`/`title`/`opener_id` are populated.
- `page_navigation`: resets the navigation epoch, so it carries the new context fields inline but omits `nav_seq` (the value reported by subsequent events for this epoch is `nav_seq + 1`).

Consumers that destructure `BrowserEventContext` generically should treat these two events as special cases.

### Per-event data fields

The canonical schema for each event's `data` payload is defined in `openapi.yaml` under the corresponding `Browser*EventData` schema (e.g. `BrowserNetworkRequestEventData`). All 22 event shapes are collected into the `KnownBrowserTelemetryEvent` discriminated union, which maps each `type` string to its concrete schema; use that as the entry point when looking up a specific event's fields. The table below summarises the key fields; refer to the schema for the authoritative field list and types.

Unless otherwise noted, events also include the nav context fields described above. Network events are the exception: they carry their own `loader_id` and `frame_id` directly and do not include nav context.

#### Console events

| Event | Unique fields |
| --- | --- |
| `console_log` | `level` (CDP type string), `text` (first arg), `args` (all args as strings), `stack_trace` |
| `console_error` | Same as `console_log` when `source.event` is `Runtime.consoleAPICalled`. When `source.event` is `Runtime.exceptionThrown`: `text`, `line`, `column`, `source_url` (script file URL, not page URL), `stack_trace`. |

#### Network events

| Event | Fields |
| --- | --- |
| `network_request` | `request_id`, `loader_id`, `frame_id`, `document_url`, `method`, `url`, `headers`, `initiator_type`. Optional: `post_data`, `resource_type`, `is_redirect` + `redirect_url`. |
| `network_response` | `request_id`, `loader_id`, `frame_id`, `method`, `url`, `status`, `headers`. Optional: `status_text`, `mime_type`, `resource_type`, `body` (truncated text body for textual MIME types). |
| `network_loading_failed` | `request_id`, `error_text`, `canceled`. Optional (absent when the request record was not found): `url`, `loader_id`, `frame_id`, `resource_type`. |

#### Page events

| Event | Unique fields |
| --- | --- |
| `page_tab_opened` | `target_id`, `target_type`, `url`, `opener_id`, `title`. Emitted before the first navigation; no nav context. |
| `page_navigation` | `session_id`, `target_id`, `target_type`, `url`, `frame_id`, `parent_frame_id` (absent for top-level frames), `loader_id`. This event establishes the nav context stamped on all subsequent events for the session. |
| `page_dom_content_loaded` | Nav context + `cdp_timestamp` (CDP monotonic seconds; not a wall-clock timestamp, use `event.ts` for ordering). |
| `page_load` | Nav context + `cdp_timestamp` (CDP monotonic seconds). |
| `page_layout_shift` | Nav context + `source_frame_id`, `time`, `duration`. Optional `layout_shift_details`: `value`, `had_recent_input`. |
| `page_lcp` | Nav context + `source_frame_id`, `time`. Optional `lcp_details`: `render_time`, `load_time`, `size`, `element_id`, `url`, `node_id`. |

#### Computed events

`network_idle`, `page_layout_settled`, and `page_navigation_settled` carry nav context fields only.

#### Interaction events

All interaction events include nav context plus the fields below.

| Event | Unique fields |
| --- | --- |
| `interaction_click` | `x`, `y` (viewport coords), `selector` (CSS selector of clicked element), `tag`, `text` (element text; empty for sensitive inputs). |
| `interaction_key` | `key` (key name), `selector`, `tag`. Not emitted for sensitive input fields. |
| `interaction_scroll_settled` | `from_x`, `from_y`, `to_x`, `to_y` (scroll positions in px), `target_selector`. |

#### Monitor lifecycle events

Lifecycle events use `source.kind = "local_process"` and carry no nav context, except `monitor_screenshot` which includes nav context alongside the image payload.

| Event | Fields |
| --- | --- |
| `monitor_screenshot` | Nav context + `png` (base64-encoded PNG). |
| `monitor_disconnected` | `reason: "chrome_restarted"`. |
| `monitor_reconnected` | `reconnect_duration_ms`. |
| `monitor_reconnect_failed` | `reason: "reconnect_exhausted"`. |
| `monitor_init_failed` | `step` (name of the init step that failed, e.g. `"Target.setAutoAttach"`). |
