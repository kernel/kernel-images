# Persistent Browser REPL API

**Status: IMPLEMENTED**

## Decision Summary

Add `POST /browser/execute`, a persistent Node.js execution API preloaded with browser-control primitives.

The public abstraction is a **persistent browser runtime**, not a CDP executor. CDP is the initial implementation and remains available through the `cdp()` escape hatch, but callers should normally use the higher-level helpers. This leaves room to implement individual helpers with CDP, Playwright, WebDriver BiDi, or existing Kernel APIs without changing the endpoint contract.

The API process owns the REPL process:

- The REPL starts lazily on the first `/browser/execute` request.
- The API process is the REPL's direct parent and explicitly terminates it during shutdown.
- Linux parent-death signaling terminates the REPL if the API process exits unexpectedly.
- An API restart therefore loses all REPL state.
- The next execution lazily starts a fresh REPL with a new CUID2 `repl_id`.

## Goals

1. Preserve JavaScript variables, functions, imports, and other top-level bindings across calls.
2. Provide a complete, ergonomic browser-control helper surface plus unrestricted raw CDP access.
3. Return the last expression and ordered text/image output produced by each execution.
4. Recover deterministically from Chromium restarts, REPL crashes, timeouts, and explicit resets.
5. Reuse the current `/playwright/execute` process and Unix-socket patterns without coupling the two runtimes.

## Non-Goals

- Sandboxing untrusted callers.
- Multiple named REPLs within one browser VM.
- Preserving REPL state across API-process restarts.
- Streaming output in v1.
- Guaranteeing that every helper remains implemented with CDP.

## HTTP Contract

### Endpoint

```text
POST /browser/execute
operationId: executeBrowserCode
```

### Request

```json
{
  "code": "const title = (await page_info()).title; repl.write(title); title",
  "timeout_sec": 60,
  "reset": false
}
```

```yaml
ExecuteBrowserCodeRequest:
  type: object
  required: [code]
  properties:
    code:
      type: string
      description: |
        JavaScript evaluated in a persistent Node.js runtime.
        Top-level bindings persist until the API process exits, the REPL is
        reset, or the REPL is terminated after a crash or timeout.
    timeout_sec:
      type: integer
      minimum: 1
      maximum: 300
      default: 60
    reset:
      type: boolean
      default: false
      description: Terminate the current REPL, start a fresh one, and then evaluate code.
  additionalProperties: false
```

`code` may be empty only when `reset` is true. Unknown request fields are
rejected with a 400, matching `additionalProperties: false`.

### Response

```json
{
  "success": true,
  "repl_id": "tz4a8m5x2h7qk9v1c6nw3pde",
  "result": "Example Domain",
  "content": [
    {
      "type": "text",
      "channel": "write",
      "text": "Loaded page"
    },
    {
      "type": "image",
      "mime_type": "image/png",
      "data_b64": "iVBORw0KGgo..."
    }
  ],
  "result_truncated": false,
  "content_truncated": false,
  "repl_terminated": false,
  "duration_ms": 412
}
```

`repl_id` is a CUID2 generated when the API process starts a new REPL child. It identifies the exact state-holding process used for the execution.

It remains stable across calls and Chromium reconnects. It changes after:

- API restart
- explicit reset
- execution timeout
- REPL crash or OOM termination

A timeout response contains the terminated REPL's ID and sets `repl_terminated: true`. No replacement is started until the next API request.

Every failure path — execution errors, timeouts, crashes/OOM, and protocol corruption — populates the same optional response fields (`duration_ms`, `result_truncated`, `content_truncated`) so clients can read them unconditionally. Partial `content` is returned whenever the daemon answered before termination (the timeout path); a child that dies mid-execution cannot flush buffered output, so crash responses carry no content.

### Content Items

Use an ordered discriminated union so writes, console messages, and images retain their relative order.

```yaml
BrowserExecutionTextContent:
  type: object
  required: [type, channel, text]
  properties:
    type:
      type: string
      const: text
    channel:
      type: string
      enum: [write, stdout, stderr]
    text:
      type: string
  additionalProperties: false

BrowserExecutionImageContent:
  type: object
  required: [type, mime_type, data_b64]
  properties:
    type:
      type: string
      const: image
    mime_type:
      type: string
      pattern: "^image/"
    data_b64:
      type: string
      contentEncoding: base64
  additionalProperties: false
```

The response also supports:

- `result`: JSON-compatible value of the final expression. Serialization
  never invokes user code: `toJSON` hooks and other prototype pollution
  installed inside the context cannot corrupt the payload. Only primitives,
  plain objects, arrays, and Dates are JSON-compatible.
- `result_repr`: bounded `util.inspect` output when the value is not JSON-compatible.
  This includes `undefined` (repr `"undefined"`, keeping it distinguishable
  from `null`), non-finite numbers and `-0`, `BigInt`, circular structures,
  and values JSON would serialize lossily such as `Map`, `RegExp`, and class
  instances.
- `error` and `stack`: execution failure details.
- truncation flags rather than silently returning partial content.

## Runtime Globals

Every browser helper is exposed in two forms:

```js
await goto_url("https://example.com");
await browser.goto_url("https://example.com");
```

`browser` is frozen so callers cannot replace methods on the namespace accidentally. Bare aliases are provided for concise agent-authored code. A caller can shadow or overwrite a bare alias; `reset: true` restores the seeded environment.

All browser helpers are asynchronous unless documented otherwise.

### Browser Helpers

The v1 runtime exposes all of the following functions:

| Function | JavaScript contract |
| --- | --- |
| `cdp` | `await cdp(method, params?, session_id?)` sends an unrestricted CDP command. |
| `drain_events` | Returns and clears buffered CDP events for the attached session. |
| `goto_url` | Navigates the attached tab. |
| `page_info` | Returns URL, title, viewport, scroll position, page dimensions, or a pending dialog. While a modal dialog is open the renderer is frozen, so the helper short-circuits: it returns the pending dialog plus last-known target metadata (URL, title) without evaluating in the page. |
| `click_at_xy` | Dispatches mouse press/release events in CSS viewport coordinates. |
| `type_text` | Inserts text through CDP input. |
| `fill_input` | Fills a framework-managed input and dispatches the expected DOM events. |
| `press_key` | Dispatches keyboard events, including modifiers and navigation keys. Modifiers are an array drawn from Alt, Control, Meta, Shift or the object sugar `{ctrl: true}`; anything else is a clear validation error. |
| `scroll` | Dispatches a mouse-wheel event at a viewport coordinate, bounded by a short dispatch timeout. Rarely, a new-headless Chromium instance wedges its mouseWheel path (the command is never answered while every other command answers fine); on dispatch timeout the helper falls back to an in-page `window.scrollBy` and surfaces the fallback as a stderr content item. An earlier upstream limitation — new-headless Chromium ignoring mouse-wheel input on `data:` URL pages — was observed only on non-activated (hidden) tabs and no longer reproduces now that attach foregrounds the tab via `Target.activateTarget`; scroll works on `data:` and http(s) pages. |
| `capture_screenshot` | Saves a PNG in the VM and returns its path. Supports viewport/full-page capture and `max_dim`. |
| `list_tabs` | Lists browser page targets, optionally including internal pages. |
| `current_tab` | Returns metadata for the attached target. |
| `switch_tab` | Attaches the runtime to another page target. |
| `new_tab` | Creates and attaches to a new page target. When a URL is given, waits (best effort) for the initial navigation to commit in the renderer — target metadata shows the URL as soon as the navigation starts, which is too early — so immediate follow-up calls observe it. |
| `close_tab` | Closes a page target, waiting (best effort) for the target to be destroyed so an immediate `list_tabs` no longer counts it. |
| `ensure_real_tab` | Ensures the runtime is attached to a non-internal page. |
| `iframe_target` | Finds an out-of-process iframe target by URL substring. |
| `wait` | Promise-based sleep using seconds for API compatibility. |
| `wait_for_load` | Waits for the document ready state. |
| `wait_for_element` | Waits for a selector, optionally requiring visibility. A non-object `opts` argument is a clear validation error. |
| `wait_for_network_idle` | Uses buffered Network events to detect an idle interval. |
| `js` | Evaluates JavaScript in the attached page or specified iframe target. |
| `dispatch_key` | Dispatches a DOM `KeyboardEvent` against a selected element. |
| `upload_file` | Sets files on an `<input type=file>` using VM-local paths. |
| `http_get` | Performs an HTTP GET from the VM and returns the response body. |
| `start_recording` | Starts a Kernel browser recording and returns its identifier or path. Recorder IDs cannot be reused after stop+delete within one API process lifetime (existing recording API behavior); use a fresh ID per recording. Options accept the recording API's `id` field or the `recorder_id` alias (matching the response shape); `id` wins when both are given. |
| `stop_recording` | Stops the active Kernel browser recording. Accepts the same `id` / `recorder_id` alias as `start_recording`. |
| `recording_dir` | Returns the active recording directory or null. |

Where Kernel already has first-class recording or file APIs, these helpers should delegate to the existing implementation rather than create a second recording or storage system.

Wait-style helpers (`wait_for_load`, `wait_for_element`, `wait_for_network_idle`) clamp their internal deadline to just below the executing request's deadline, so a routine helper timeout surfaces the helper's own error instead of tying the destructive execution timeout and killing the REPL.

Individual CDP commands are clamped the same way: every command's effective timeout is bounded to just below the executing request's deadline, and domain enables during attach run under a bounded budget. A renderer frozen behind a modal JavaScript dialog never answers session-routed commands; the clamp turns that into a clean error (with a recovery hint) instead of a destructive execution timeout, and the REPL survives. Browser-level commands (e.g. `Target.*`) are unaffected, and `Page.handleJavaScriptDialog` / `Page.reload` remain answerable because Chromium handles them browser-side.

### REPL Helpers

```ts
type Repl = {
  readonly id: string;
  write(value: unknown): void;
  emitImage(image: ImageInput): Promise<void>;
};

type ImageInput =
  | string // image/* data URL
  | Buffer
  | Uint8Array
  | ArrayBuffer
  | { bytes: Buffer | Uint8Array | ArrayBuffer; mimeType?: string }
  | { path: string; mimeType?: string };
```

`repl.write(value)` adds a `text/write` item without an implied newline. Strings are unchanged; other values use bounded console-style formatting.

`console.log`, `console.info`, and `console.debug` add `text/stdout` items. `console.warn` and `console.error` add `text/stderr` items.

`repl.emitImage()` adds one image item at its position in the content stream. It accepts PNG, JPEG, or WebP bytes with MIME sniffing, an explicit MIME type, an `image/*` data URL, or a VM-local path.

### Screenshot Workflow

`capture_screenshot()` remains file-oriented and does not automatically enlarge every API response with image bytes:

```js
const screenshot_path = await capture_screenshot(
  "/tmp/page.png",
  false,
  1800,
);
await repl.emitImage({ path: screenshot_path });
```

Image bytes are base64-encoded only inside typed image content. They are never embedded into text, stdout, stderr, or a data URL in the public response.

Initial limits:

- 8 MiB decoded per emitted image
- 16 MiB decoded image data per response
- 256 KiB combined text output per response
- 256 KiB serialized result per response

An oversized screenshot remains available through its returned VM path and the filesystem API.

## Process Architecture

```text
Client
  │ POST /browser/execute
  ▼
API process
  │ browserReplMu: one execution at a time
  │ owns child lifecycle and CUID2 repl_id
  ▼
Node REPL child
  │ persistent JavaScript context
  │ Unix socket request/response protocol
  │ persistent raw browser connection
  ▼
DevTools proxy :9222
  ▼
Chromium :9223
```

### Ownership

The API process is the sole REPL owner and supervisor.

On lazy startup it:

1. Generates `repl_id` with `github.com/nrednav/cuid2`.
2. Removes or rejects a stale socket left by a previous process.
3. Starts `node /usr/local/lib/browser-repl.js` with the ID in an environment variable or argument.
4. Configures the child in its own process group.
5. Configures Linux parent-death signaling so the child is killed if the API process dies.
6. Polls the Unix socket until the child reports ready.

On graceful API shutdown it closes the socket, terminates the process group, waits with a short deadline, and escalates to `SIGKILL` if needed.

The API must not reconnect to or adopt an orphaned REPL from an earlier API process. A stale socket or child is terminated and replaced: when lazy startup finds a live listener on the socket that it does not own (parent-death signaling covers only children the API itself spawned), the owning daemon process is located via `/proc/net/unix` and `/proc/<pid>/fd` and SIGKILLed before the socket file is removed, so an orphaned REPL cannot leak for the container lifetime.

### Browser Connection

The REPL connects through `ws://127.0.0.1:9222`, not directly to Chromium's internal port. This preserves the existing proxy's telemetry and scale-to-zero behavior.

The browser connection is established lazily on the first browser helper call. Pure Node.js code can execute while Chromium is unavailable.

The runtime maintains:

- one browser-level WebSocket
- one attached page target/session
- enabled Page, DOM, Runtime, Network, and related domains
- a bounded event ring
- pending JavaScript-dialog state

Attach also activates the target (`Target.activateTarget`) so it behaves as the foreground tab: in headless Chromium a hidden tab's JavaScript dialogs are auto-cancelled (`Page.javascriptDialogClosed` with `result:false` immediately after opening), and which tab is foreground after a Chromium restart is not deterministic. Activation keeps the documented dialog semantics stable across restarts.

A Chromium restart clears only browser connection/session state. The REPL reconnects and reattaches on the next helper call without changing `repl_id` or JavaScript bindings.

### Stale dialogs at attach

A modal JavaScript dialog freezes its renderer's main thread, and a dialog left open by a REPL that was subsequently killed has no live execution that could ever handle it. On attach the runtime therefore:

1. Enables domains under a bounded budget; a renderer that never answers marks the session as unresponsive instead of consuming the whole execution deadline.
2. Attempts a best-effort `Page.handleJavaScriptDialog` (accept) when a dialog is detected at attach (Chromium re-emits `Page.javascriptDialogOpening` on `Page.enable` in many versions) or when the renderer was unresponsive during domain enable. A successful dismissal is surfaced as a `stderr` content item, and domain enables are retried.
3. Retries domain enables on the next session command when they previously failed, so the session recovers in place once the renderer unfreezes.

Some Chromium builds drop the browser-side dialog record when the owning DevTools session disconnects while leaving the renderer frozen; in that case no dismissal is possible and `Page.handleJavaScriptDialog` answers "No dialog is showing". The tab stays frozen but the endpoint does not: session commands fail cleanly (the error message names the recovery options), browser-level helpers such as `list_tabs` keep working, and the caller recovers without restarting Chromium via `cdp("Page.reload")`, `new_tab()`, or `close_tab()` of the frozen tab.

## Evaluation Semantics

The runtime supports:

- one complete JavaScript cell per request, evaluated as a fresh `vm.SourceTextModule`
- top-level `await` and implicit final-expression results
- persistent `var`, `let`, `const`, function, and class bindings
- dynamic `import()`

Static top-level imports and exports are rejected; callers use dynamic `import()`. Top-level `return` is rejected.

Meriyah is the exact-pinned normal dependency used for cell metadata: declaration
kinds, binding names, static-module rejection, and the final expression are
identified before module construction. A declaration registry performs the
cross-cell early-error check before effects. `var` and `function` may redeclare
each other; `let`/`const`/`class` conflict with every prior declaration. Each
fresh modules do not snapshot or import prior-cell values; persistent names
are backed by accessor properties on the context's `globalThis`. The accessor
is the single binding for a name, so a closure created in one cell, a later
cell, and a timer all observe the same value. Persistent top-level declarations
and top-level `var` declarations in nested statements are lowered to those
accessors; function declarations are renamed to module-local aliases and
initialized through the accessor prelude, so same-cell closures and assignments
cannot capture a shadow binding. Declarations inside nested functions or blocks
remain ordinary locals.

Initializer writes use a nonce-named initialization target that exists only
for the active cell, so ordinary writes before a lexical declaration throw the
same TDZ `ReferenceError` as JavaScript and user code cannot retain the target.
A failed multi-declarator cell retains every initializer that completed before
the failure, while failed lexical initializers remain uninitialized and cannot
be repaired by assignment. Lexical names are reserved after linking, so a
failed `let`/`const`/`class` cell cannot be retried verbatim; use a new name or
reset the REPL. The REPL process is not reset for an ordinary exception. Reset
and destructive timeout/crash recovery still create a new process.

## Timeout and Failure Semantics

A timeout is destructive because JavaScript cannot be reliably interrupted inside the same Node process.

When the HTTP deadline or execution timeout fires, the API process:

1. kills the REPL process group;
2. waits for process exit;
3. removes the stale socket;
4. clears the in-memory child handle and `repl_id`;
5. returns `success: false`, the terminated ID, and `repl_terminated: true`.

The next request lazily creates a fresh child and a fresh CUID2.

The same reset behavior applies after:

- child exit
- OOM termination
- malformed or mismatched daemon responses
- unrecoverable protocol corruption

Ordinary user-code exceptions return `success: false` without terminating the child.

### Process-Level User-Code Failures

Code evaluated in the REPL can also fail *outside* the execution that
submitted it (timers, floating promises). The daemon splits these by Node
lifecycle semantics:

- **Unhandled promise rejections** leave no inconsistent in-flight state —
  the promise has already settled — and Node sanctions keeping the process
  alive when a handler is installed. The daemon surfaces the rejection as a
  bounded `stderr` content item (into the active execution, or drained into
  the next one) and the REPL, including all bindings, survives. This
  matters because floating promises are common in agent-authored code
  (e.g. `js("alert('x')")` without `await`, whose CDP timeout rejection
  would otherwise kill the REPL 30s later, mid-unrelated-request).
- **Uncaught exceptions** (e.g. a throwing `setTimeout` callback) are
  different: Node documentation is explicit that resuming after one is
  unsafe because the process may be in an undefined state. The daemon
  therefore terminates deterministically rather than blindly staying alive
  in a possibly-corrupt state: it logs the exception, answers the
  in-flight execution (when one exists) with `success: false`, the
  exception details, partial content, and `exiting: true`, and exits
  non-zero. The API maps `exiting` to `repl_terminated: true`, so the
  caller sees the state loss explicitly instead of as a bare EOF, and the
  next request lazily starts a fresh REPL with a new `repl_id` — the
  documented signal that all prior bindings were lost.

## Daemon Protocol

Continue using newline-delimited JSON over a Unix socket, with one socket connection per API call.

Request:

```json
{
  "id": "request-id",
  "code": "await page_info()",
  "timeout_ms": 60000
}
```

Response:

```json
{
  "id": "request-id",
  "repl_id": "tz4a8m5x2h7qk9v1c6nw3pde",
  "success": true,
  "result": {},
  "content": [],
  "duration_ms": 12
}
```

When the daemon-side `timeout_ms` fires on an interruptible execution, the daemon responds with `success: false` and `timed_out: true` plus the partial content produced before the deadline. The abandoned execution is still running inside the child, so the API treats `timed_out` exactly like a transport failure: it kills the process group and returns the terminated ID with `repl_terminated: true`. This keeps every timeout destructive while still returning partial output promptly; an uninterruptible execution (e.g. `while (true) {}`) never answers and is killed at the API's socket read deadline.

The incoming request line is capped at 8 MiB per accumulated line,
independent of how the request was chunked across writes; an oversized
line is rejected and the connection closed. The server runs with
`allowHalfOpen`, so a client that half-closes (`SHUT_WR`) after sending
its request still receives the execution response; the daemon ends its own
side once the final queued response is flushed.

The API validates both request ID and `repl_id`. A mismatch terminates the child rather than risking responses from stale state. Both timeout paths use the same error message (`execution timed out after <N>ms`); the uninterruptible path skips the SIGTERM grace period (the blocked event loop could never handle it) and SIGKILLs the process group at the read deadline. A child that dies mid-execution (e.g. OOM-killed near the heap cap) reports its exit reason in the error instead of a bare transport error.

The child serializes executions internally as defense in depth even though the Go handler also holds `browserReplMu`.

## Security Model

This endpoint is unrestricted code execution inside the browser VM, equivalent in trust level to the existing process and Playwright execution APIs.

User code can access:

- Node built-ins and installed packages
- filesystem
- network
- environment variables
- processes
- the browser-level CDP endpoint

A Node `vm.Context` is a state container, not a security boundary. The API and documentation must not describe the REPL as sandboxed.

Protect only service integrity:

- keep protocol transport on the Unix socket rather than process stdout;
- cap results, text, images, event buffers, and incoming lines;
- retain private references for protocol serialization so global/prototype modification cannot corrupt framing;
- kill the process on timeout or protocol corruption;
- launch Node with a configurable heap cap.

## Implementation Plan

### OpenAPI and Go

| File | Change |
| --- | --- |
| `server/openapi.yaml` | Add `/browser/execute`, request/response schemas, and typed content items. |
| `server/lib/oapi/oapi.go` | Regenerate with `make oapi-generate`. |
| `server/cmd/api/api/browser_repl.go` | Add child lifecycle, CUID2 generation, Unix-socket client, response mapping, reset, and timeout termination. |
| `server/cmd/api/api/api.go` | Add `browserReplMu`, child command/process-group state, socket path, and active `repl_id`. |
| `server/cmd/api/api/api.go` shutdown path | Explicitly terminate and wait for the REPL child. |

Do not refactor `/playwright/execute` during the initial implementation. Shared daemon plumbing can be extracted afterward once both paths have stable tests.

### Node Runtime

| File | Change |
| --- | --- |
| `server/runtime/browser-repl.ts` | Unix server, persistent evaluator, request queue, output capture, serialization, and seeded globals. |
| `server/runtime/browser-cdp-client.ts` | WebSocket correlation, events, reconnection, target attachment, and session routing. |
| `server/runtime/browser-helpers.ts` | Complete browser helper implementation and frozen namespace construction. |

### Images

Bundle the runtime in both headful and headless Dockerfiles with the exact-pinned Meriyah dependency. Launch the daemon with `--experimental-vm-modules` on Node 22. The image build may continue using its existing bundler for build-time compilation; the browser REPL has no runtime bundler or TypeScript dependency. Add an image-resizing dependency only if CDP screenshot scaling cannot implement `max_dim` accurately.

## Test Plan

### Persistent Runtime

- Define variables, functions, classes, and dynamic imports in one call; consume them in later calls.
- Verify `repl_id` remains stable across successful calls and Chromium restarts.
- Verify explicit reset creates a new CUID2 and removes all prior bindings.
- Restart the API process and verify the child dies and the next call receives a new ID.
- Kill the REPL child and verify lazy recovery with a new ID.

### Timeout and Concurrency

- Run an unresolved promise and verify timeout termination.
- Run `while (true) {}` and verify the Go parent still kills the process promptly.
- Verify no replacement starts until the next request.
- Send concurrent requests and verify strict serialization.
- Verify output from a terminated execution cannot leak into a later execution.

### Output

- Interleave `repl.write`, console methods, and multiple images; verify content order.
- Cover strings, BigInt, errors, circular values, undefined, NaN, and large values.
- Verify result and output truncation flags.
- Verify image MIME sniffing and rejection of non-image data URLs.
- Verify image and aggregate size limits.

### Browser Helpers

- Exercise every seeded helper at least once in unit or e2e coverage.
- Verify tab creation, switching, closing, and reconnect behavior.
- Verify DOM input, keyboard, upload, iframe, dialog, event, and network-idle semantics.
- Verify screenshot path retrieval and explicit image emission.
- Verify recording helpers delegate to the existing recording implementation.

## Known Limitations

- **Mouse-wheel scroll on `data:` URLs (historical; no longer reproduces).** This was observed only on non-activated (hidden) tabs: in new headless mode Chromium deprioritizes wheel input on hidden tabs, and on `data:` URL pages the scroll position never changed (confirmed via raw CDP). Since attach now foregrounds the tab with `Target.activateTarget`, wheel scroll works on `data:` pages on both variants (raw CDP included). The `scroll()` settle/retry logic stays as harmless defense in depth for hidden-tab or future upstream regressions.
- **First wheel event after a navigation is swallowed (upstream; no longer reproduces on activated tabs).** Like the `data:` URL no-op above, this was observed on non-activated tabs: Chromium silently dropped the first `Input.dispatchMouseEvent` mouseWheel after each navigation — the command answered normally but the page never scrolled (verified via raw CDP; a `wait()` after navigation did not prevent it). After `Target.activateTarget` was added at attach, the first wheel after a navigation lands exactly at its target on both headless variants. The defensive retry remains in place: `scroll()` probes the window scroll offset around each dispatch and, when the window itself is scrollable in a requested direction and the offset did not change, retries the dispatch once (surfaced as a stderr content item). Because Chromium applies a wheel dispatch asynchronously after the CDP command answers, the post-dispatch probe polls the offset through a short settle window before concluding the dispatch had no effect — an immediate probe reads the pre-scroll offset and the retry would land the page 2x past its target. Verification is skipped when the window cannot scroll in the requested direction (unscrollable document, already at the edge, or a wheel targeted at an inner scrollable element), so those dispatches behave exactly as before. If the page still has not scrolled after one retry, a stderr note says so (the page may intercept wheel events).
- **Wedged mouseWheel path (rare, upstream).** QA observed one new-headless Chromium instance where `Input.dispatchMouseEvent` mouseWheel commands were never answered (a ~5-minute window on one process; not reproducible after a Chromium restart) while every other command on the same session answered fine. The signature is a `CDP Input.dispatchMouseEvent timed out` error with `window.scrollY` unchanged. `scroll()` bounds the dispatch to a short timeout and falls back to an in-page `window.scrollBy` (surfaced as a stderr content item), so the wedge costs one bounded fallback instead of a 30s hang per attempt. The fallback is DOM scrolling, not synthetic input; it does not trigger wheel event listeners.
- **Stale dialogs from a killed REPL.** See "Stale dialogs at attach": on Chromium builds that drop the browser-side dialog record on session disconnect, the frozen tab cannot be dismissed and must be recovered with `cdp("Page.reload")`, `new_tab()`, or `close_tab()`.

## Milestones

1. **Runtime spike (1–2 days):** parent-owned child, persistent bindings, CUID2 `repl_id`, explicit output, timeout kill, and API-restart cleanup.
2. **Core endpoint (3–4 days):** OpenAPI, typed content, CDP client, navigation/tab/input helpers, screenshots, and e2e persistence tests.
3. **Complete helper surface (3–4 days):** events, network idle, uploads, recording wrappers, screenshot scaling, and helper-by-helper tests.
4. **Hardening (2–3 days):** size limits, protocol corruption, OOM/crash recovery, shutdown races, documentation, and full image test matrix.

Estimated total: approximately two engineer-weeks after the runtime spike validates the evaluation strategy.
