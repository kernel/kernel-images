# CDP Monitor

The monitor is the browser-facing layer of the kernel browser logging pipeline. It connects to Chrome's DevTools endpoint, tracks all page sessions via CDP's `Target.setAutoAttach`, and converts raw CDP notifications into typed `events.Event` values for downstream consumers.

## Overview

`cdpmonitor` manages a Chrome DevTools Protocol (CDP) WebSocket connection to a running Chrome browser. It subscribes to CDP events across all attached tabs, translates them into structured `events.Event` values, and publishes them via a caller-supplied `PublishFunc`. It also derives synthetic events from sequences of CDP events and takes screenshots on significant page activity.

Chrome can restart independently of the monitor. When that happens, `UpstreamProvider` pushes a new DevTools URL and the monitor reconnects automatically, emitting lifecycle events so consumers can track continuity.

## Event taxonomy

**CDP-derived** (1-to-1 with a CDP notification): `console_log`, `console_error`, `network_request`, `network_response`, `network_loading_failed`, `navigation`, `dom_content_loaded`, `page_load`, `layout_shift`

**Computed** (inferred from sequences of CDP events): `network_idle` (fires when in-flight requests drop to zero), `layout_settled` (1 s after `page_load` with no intervening layout shifts), `navigation_settled` (fires once `dom_content_loaded`, `network_idle`, and `layout_settled` have all fired for the same navigation).

**Interaction** (fired by `interaction.js` via `Runtime.bindingCalled`): `interaction_click`, `interaction_key`, `scroll_settled`

**Monitor lifecycle** (emitted by the monitor itself, not by Chrome): `screenshot`, `monitor_disconnected`, `monitor_reconnected`, `monitor_reconnect_failed`, `monitor_init_failed`

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
| Body/MIME capture sizing and text truncation helpers | `util.go` |

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
restartMu -> lifeMu -> pendReqMu -> computed.mu -> pendMu -> sessionsMu
```

`bindingRateMu` is independent of this ordering and is always acquired alone.

| Lock | Protects |
| --- | --- |
| `restartMu` | Serializes `handleUpstreamRestart` to prevent overlapping reconnects from rapid Chrome restarts |
| `lifeMu` | `conn`, `lifecycleCtx`, `cancel`, `done`, `readReady` -- all fields that change during Start / Stop / reconnect |
| `pendReqMu` | `pendingRequests` (requestId -&gt; `networkReqState`): in-flight network requests accumulating request/response metadata until `loadingFinished` |
| `computed.mu` | All `computedState` fields: counters and timers for the `network_idle`, `layout_settled`, and `navigation_settled` state machines |
| `pendMu` | `pending` (id -&gt; reply channel): in-flight CDP commands waiting for a response from Chrome |
| `sessionsMu` | `sessions` (sessionID -&gt; `targetInfo`): the set of currently attached CDP targets (tabs, iframes, workers) |
| `bindingRateMu` | `bindingLastSeen` (sessionID:eventType -&gt; time): rate-limit state for `__kernelEvent` binding calls |

Fields that need no mutex use `sync/atomic`: `nextID`, `mainSessionID`, `running`, `lastScreenshotAt`, `screenshotInFlight`.

### WebSocket concurrency

`coder/websocket` guarantees one concurrent `Read` and one concurrent `Write` are safe on the same connection. `readLoop` is the sole reader. All writes go through `send`, which calls `conn.Write` directly -- `conn.Write` is internally serialized by the library, so no external write mutex is needed.