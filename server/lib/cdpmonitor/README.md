# CDP Monitor

The API starts the monitor independently of customer telemetry. It owns one persistent connection to Chrome's DevTools endpoint and uses `browsersurface` to track page, iframe, worker, and background-page sessions. Metrics-only mode enables Network and target discovery; customer telemetry optionally adds typed events, computed timers, body retrieval, screenshots, and interaction capture.

## Overview

`cdpmonitor` manages a Chrome DevTools Protocol (CDP) WebSocket connection to a running Chrome browser. It subscribes to CDP events across all attached tabs, translates them into structured `events.Event` values, and publishes them via a caller-supplied `PublishFunc`. It also derives synthetic events from sequences of CDP events and takes screenshots on significant page activity.

Chrome can restart independently of the monitor. The monitor retries startup failures, upstream notifications, socket loss (including at the same URL), and required discovery/domain initialization failures. Customer events still publish exclusively through `TelemetrySession.Publish`; platform counters do not export event payloads.

## Always-on network metrics

The separate in-memory collector serves these **label-free** metrics on the existing
`GET /metrics` endpoint, including zeros before capture starts:

| Metric | Meaning |
| --- | --- |
| `kernel_chromium_connection_resets_total` | Observed terminal `Network.loadingFailed` outcomes with exactly `net::ERR_CONNECTION_RESET`. |
| `kernel_chromium_network_requests_completed_total` | Observed terminal `Network.loadingFinished` or `Network.loadingFailed` outcomes. Includes cancellations, refusals, HTTP 500 responses, and unknown-start outcomes. |
| `kernel_chromium_network_monitor_up` | Discovery initialized, socket open, and Network plus dedicated-worker discovery initialized for every known attached target. Reattached targets also finish any retained interaction cleanup before becoming ready. Zero during setup, failures, reconnect, and shutdown. Not browser responsiveness or proof of complete request coverage. |

These count **CDP request-chain observations, not socket resets**. Internal browser
retries are not separately counted. Redirects reuse a request ID and contribute
one final outcome, not one per hop. HTTP status does not classify transport errors.
Missing `requestWillBeSent` does not exclude a valid terminal event from either
counter; metrics-only mode does not retain request-start records or bodies.

Deduplication is first-terminal-wins within the most recent **8,192 distinct
(session ID, request ID)** terminal keys in one connection generation. A fixed FIFO
and map bound the history. Retained IDs are limited to 256 bytes each; empty,
oversized, malformed, sessionless, and untracked-session events are ignored.
Identities are cleared only after old connection work drains, so a reused session
and request ID on a fresh connection counts again. Totals survive reconnections,
Chrome restarts, and telemetry toggles; a new API process starts at zero.

There is no cross-session deduplication: equal IDs on different targets may be
unrelated, while workers/service workers can expose multiple observations of one
logical fetch. A duplicate arriving after FIFO eviction can count again. Requests
before attachment/domain readiness, during disconnection, or after target detach
can be missed. These metrics are **not lossless** and are not unique HTTP-request
or socket counts. The reset fraction uses the same observed-terminal denominator.

The existing short-lived `ChromeCollector` and its UMA behavior are unchanged.
Network metrics remain available when Chrome/UMA collection fails; scrapes do not
reset or increment the counters. No URLs, domains, identities, or error-string
labels are emitted. No additional configuration or kill switch is introduced.

### Local verification and remaining checks

```sh
# From server/; Chromium must be installed for the opt-in suite.
go test -race ./lib/cdpmonitor ./lib/metrics ./cmd/api/api
KERNEL_CDPMONITOR_CHROME_E2E=1 go test -race ./lib/cdpmonitor -count=1 -v
go test ./lib/cdpmonitor -run '^$' -bench '^BenchmarkMetricsOnlyTerminalDispatch$' -benchmem
```

The metrics fixture uses local HTTP and TCP RST (`SetLinger(0)`), POST requests to
avoid transparent GET retries, cache-disabled responses, and settled targets.
It independently checks Chromium's error text and exact +10/+10 scrape deltas;
then exercises non-reset outcomes, same-process frames, OOPIFs, dedicated/shared/
service workers, telemetry off/on/off cleanup, socket replacement, actual Chrome
restart, and a fresh monitor's zero counters. API lifecycle/race tests cover
startup, telemetry toggles, and shutdown. Additional regressions delay a cleanup
command while PUT/PATCH/GET continue, fence old capture across coalesced revisions,
and recover page/frame listeners after socket loss or failed cleanup. They check
future navigations and the independent user CDP connection as well. Extension
background pages are included in discovery but are not validated by a real extension
fixture here. A delayed-Network-reply regression also verifies that pending
attachments finish recovery before optional instrumentation starts, and that
click events arrive without navigation through subsequent telemetry toggles.

Known limitation: re-enabling telemetry can leave already-loaded same-process
iframes without interaction listeners. This also occurs with the previous
Stop/Start lifecycle and is not addressed by the attachment-ordering fix.

The microbenchmark measures Go terminal ingestion, not total Chromium CPU/memory
or live workload overhead. Full image/API-process restart, suspend/resume, snapshot
fork, and staging checks are not covered by these local tests. A restored process
retains its counters and dedup history; a fork of its memory may inherit its parent's
baseline. Applying fork identity does not reset these process-lifetime counters.
Scrapers must use the new instance identity and treat the first sample as a baseline,
not a count of post-fork activity. Half-open socket detection uses a 5-second
probe interval plus a 5-second timeout after execution resumes; health can lag a
silent failure until that probe. Reattachment then follows the retry/setup limits
below. No Chromium patch is involved.

## Real-Chromium network regression tests

From `server/`, with Chromium on `PATH`:

```sh
KERNEL_CDPMONITOR_CHROME_E2E=1 go test ./lib/cdpmonitor \
  -run '^TestNetworkCaptureAcrossFrames$' -count=1 -v
```

The fixture uses local HTTP servers and forces site isolation; it needs no Kernel
credentials or external websites. It checks sent and aborted POSTs from top-level,
same-origin, cross-origin, and nested cross-origin frames, in default and isolated
browser contexts, with frames created before and after the monitor starts. It
verifies browser results and server-received bodies before asserting telemetry
request/response bodies and failure correlation.

All 32 cases assert successful capture, without prescribing an attachment strategy.
`TestNetworkCaptureFromWorkers` covers dedicated, shared, and service workers;
`TestTelemetryConnectionOwnershipAndReconnect` checks independent client ownership
and fresh sessions after reconnect. Without the environment variable these tests
skip, matching the other real-Chromium tests in this package. They exercise settled
targets, not requests issued before their capture domains finish initializing.

## Event taxonomy

**CDP-derived** (1-to-1 with a CDP notification): `console_log`, `console_error`, `network_request`, `network_response`, `network_loading_failed`, `proxy_error` (classified from a branded 5xx response carrying the `X-Kernel-Proxy-Error` header), `page_tab_opened`, `page_navigation`, `page_dom_content_loaded`, `page_load`, `page_layout_shift`, `page_lcp`. `proxy_error` is an opt-in per-session/per-URL refinement of the raw `network` events: it is only observable while the network category (CDP collector) is running, so it is not a default-on alerting signal.

**Computed** (inferred from sequences of CDP events): `network_idle` (fires when in-flight requests drop to zero), `page_layout_settled` (1 s after `page_load` with no intervening layout shifts), `page_navigation_settled` (fires once `page_dom_content_loaded` and `page_layout_settled` have both fired for the same navigation; intentionally independent of `network_idle` so that a single hung request cannot stall the event).

**Interaction** (fired by `interaction.js` via `Runtime.bindingCalled`): `interaction_click`, `interaction_key`, `interaction_scroll_settled`

**Monitor lifecycle** (emitted by the monitor itself, not by Chrome): `monitor_screenshot`, `monitor_disconnected`, `monitor_reconnected`, `monitor_reconnect_failed`, `monitor_init_failed`

## Responsibilities

| Concern | Where |
| --- | --- |
| Connection ownership and reconnect | `monitor.go` |
| CDP transport and command routing | `../cdpclient` |
| Target discovery and attachment lifecycle | `../browsersurface` |
| CDP domain setup per session | `domains.go` |
| Desired telemetry revision and asynchronous reconcile | `telemetry.go` |
| Interaction cleanup across connection replacement | `interaction_cleanup.go` |
| Event translation (CDP params to `events.Event`) | `handlers.go` |
| Synthetic event state machines | `computed.go` |
| Screenshot capture via ffmpeg | `screenshot.go` |
| CDP protocol types | `cdp_proto.go`, `types.go` |
| Interaction tracking injected into the page | `interaction.js` |
| Body/MIME capture sizing, text truncation, and typed payload helpers | `util.go` |

## Internals

### Connection ownership and discovery

Each monitor owns a `cdpclient.Client` and a `browsersurface.Tracker`. No connection,
CDP session, or tracker state is shared with WebMCP. Telemetry adds worker and
background-page targets with `WithAdditionalTargets` and uses `WithoutLocations`: it receives attachment events without waiting for window
lookup or frame-tree initialization, and enables its own capture domains.

The tracker explicitly discovers and attaches pages, OOPIFs, shared workers, and
service workers (and extension background pages). Dedicated workers require parent-session `Target.setAutoAttach`;
that subscription is also installed on worker sessions to discover nested workers.
Only `worker` targets match this auto-attach filter, avoiding duplicate attachment
of explicitly discovered OOPIFs. WebMCP's default tracker still tracks page/frame
locations and does not subscribe to workers.

### Reconnect and shutdown

`Start` subscribes before reading the current URL. The supervisor reacts to socket
closure, upstream notifications, and required initialization failure; a 5-second
browser-level probe detects silent socket failure. It cancels and drains the old
connection before clearing identities and creating the next client/tracker. Retries
continue for the API lifecycle, with delays doubling from 250 ms to a 5-second cap.
A healthy probe resets the delay. Dials take at most 5 seconds; initial discovery
and Network commands have 30-second limits. Health requires domain readiness,
not just a successful dial or probe. The existing `monitor_disconnected` payload
retains its legacy `chrome_restarted` reason for connection replacement, including
socket loss; use the capture-health gauge rather than that reason to diagnose
availability. Retries no longer exhaust, so `monitor_reconnect_failed` is not emitted.

`asyncWg` tracks the supervisor, telemetry reconciler, and request sweeper. `captureWg` tracks discovery
and domain setup; `telemetryWg` drains optional body/screenshot work. `Stop` cancels
the lifecycle and drains all three. Closing the protocol unblocks pending commands.

`SetTelemetry` commits a desired revision and fences customer publication without
waiting for CDP work. The telemetry endpoints report this accepted desired state,
not completion of background cleanup. A lifecycle-owned worker serializes teardown
and setup outside the API-wide lock. An off/on pair still drains the old revision;
a newer disable prevents an obsolete enable from running after slow cleanup.
Reconciliation schedules optional setup only for attachment-ready sessions;
pending sessions finish Network setup and orphan cleanup in the attachment path
before starting optional capture.
`TelemetrySession.Publish` continues to enforce the customer's category/session gate.

The worker drains bounded setup/body work without aborting socket writes, stops
computed timers, removes new-document registrations and live-document listeners,
and disables optional domains. Cleanup commands share a 3-second budget; failure
replaces only the monitor connection and retries. Setup remains bounded at 30 seconds.
Network counters continue during the drain. Request cancellation does not cancel
cleanup; API shutdown cancels the lifecycle and joins the worker.

Closing CDP does **not** remove document listeners. Cleanup obligations therefore
use target IDs, survive connection/session replacement, and are cleared only on
successful cleanup or confirmed target destruction. On reconnect, a target inventory
prunes vanished targets; reattached targets clean orphan listeners before being
marked ready, even when telemetry is now disabled. A temporary cleanup registration
with `runImmediately` reaches existing main worlds, including same-process frames;
it is then removed. No Runtime/Page domain is enabled solely for this recovery.
Registration IDs remain connection-local and are never reused on another session.
These obligations are in memory; full API-process restart remains an unverified check.

### Synchronization

`controlMu` serializes Start/Stop. `desiredMu` fences only in-memory publication
against desired-revision changes; it never guards CDP work. `restartMu` serializes
connection replacement against background reconcile, and `telemetryMu` protects
optional setup/dispatch. Old body/screenshot/computed work is drained before the
applied revision advances. `sessionsMu` also guards domain readiness, connection-local
script IDs, and target-scoped interaction cleanup obligations.
`lifeMu` protects the connection
pointer and lifecycle context/cancel function; it is released before waiting on
connection or capture work. `sessionsMu` protects target metadata and computed-state
lookup. `pendReqMu` protects requests keyed by **(CDP session ID, request ID)**;
request IDs from different targets must not collide. Detaching a session removes
only that session's requests.

`computed.mu` and `sessionsMu` are never held simultaneously. Navigation updates
can hold `pendReqMu` while acquiring `computed.mu`, not the reverse. Binding/proxy
rate-limit locks are acquired independently. Screenshot state, the main page
session, and running state use atomics. CDP reads and command-response routing are
owned by `cdpclient`; the monitor consumes the tracker's ordered event subscription.

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
| `frame_id` | `page_navigation`, `network_request`, `network_response`, `network_loading_failed`, `proxy_error` | The frame the request or navigation belongs to. Top-level frame has no `parent_frame_id`. |
| `source_frame_id` | `page_layout_shift`, `page_lcp` | The frame where the layout shift or LCP element occurred. Distinct from the nav context `frame_id`, which is always the top-level navigated frame. |
| `loader_id` | `page_navigation`, `network_request`, `network_response`, `proxy_error` | The document load that owns a request. Join `network_request.loader_id` to `page_navigation.loader_id` to correlate requests with the navigation that triggered them. |
| `request_id` | `network_request`, `network_response`, `network_loading_failed`, `proxy_error` | A single request chain (including redirects). Links request to its eventual response or failure. |

### Navigation context fields

Most event `data` objects include a nav context block stamped at the last `page_navigation`. These fields reflect the top-level frame most recently navigated in the session:

| Field | Description |
| --- | --- |
| `session_id` | Same as `source.metadata.cdp_session_id`. Repeated for data-only consumers. |
| `frame_id` | Frame ID of the navigated top-level frame. |
| `loader_id` | Loader ID of the current document. |
| `url` | URL of the current page at the time of the last navigation. |
| `nav_seq` | Monotonically increasing counter, incremented on each `page_navigation`. Use it to detect that the page has navigated between two events in the same session. For `network_request`/`network_response`/`network_loading_failed`/`proxy_error`, the `nav_seq` is captured at request-send time and carried forward to the response so a request/response pair always shares an epoch. |

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
| `proxy_error` | `request_id`, `code` (typed enum matching the metro header values), `status` (502). Optional: `url`, `loader_id`, `frame_id`, `method`, `resource_type`. Emitted when a 502 response carries the `X-Kernel-Proxy-Error` header; unknown codes are dropped and emission is sampled to at most one per session+code+resource_type per second. WebSocket handshakes are not classified (documented non-goal). |

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
