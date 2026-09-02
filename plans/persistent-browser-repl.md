# Browser REPL API

**Status: Implemented**

## Decision

`POST /repl` evaluates JavaScript in the Browser REPL, a persistent Node runtime preloaded with browser helpers. The public contract is a browser runtime, not a CDP executor; raw CDP remains available through `cdp()`.

The API process directly owns one lazily started REPL child. State is intentionally lost after API restart, explicit reset, timeout, crash, OOM, or protocol corruption. Each new child receives a CUID2 `repl_id`.

This endpoint is unrestricted code execution inside the browser VM. Its `vm.Context` stores state but is not a security boundary.

## Public API surface

The REPL is scoped to an individual browser instance, so its HTTP path does not repeat the browser namespace:

```text
POST /repl
operationId: executeBrowserRepl
```

The OpenAPI contract uses `BrowserReplRequest`, `BrowserReplResult`, `BrowserReplContent`, `BrowserReplTextContent`, and `BrowserReplImageContent`. The generated Go handler is `ExecuteBrowserRepl`, and strict request-body decoding is applied specifically to `POST /repl`.

This naming is intended to support an object-oriented SDK surface without exposing the instance transport:

```ts
const browser = await kernel.browsers.create();
await browser.repl({code: "const x = 41"});
await browser.repl({code: "x + 1"});
```

SDK implementation lives outside this repository.

## HTTP contract

Request:

```json
{
  "code": "const title = (await pageInfo()).title; repl.write(title); title",
  "timeout_sec": 60,
  "reset": false
}
```

- `code` is JavaScript and may be empty only when `reset` is true.
- `timeout_sec` is an integer from 1 through 300, defaulting to 60.
- Unknown fields are rejected.
- The API rejects request bodies and marshaled daemon envelopes above 8 MiB.

Successful responses contain `success`, `repl_id`, ordered `content`, `content_truncated`, and `duration_ms`. Failures may additionally contain `error`, `stack`, and `repl_terminated`.

`repl_id` stays stable across normal calls and Chromium reconnects. A destructive failure response carries the terminated ID; no replacement starts until the next request.

### Ordered content

Content is an ordered union:

```text
{text, channel: write|stdout|stderr, text}
{image, mime_type: image/*, data_b64}
```

Expression values are not returned automatically. Output is optional: an execution may produce zero content, use `repl.write(...)` or `repl.emitImage(...)`, call console methods, or combine them.

Limits:

- 8 MiB per image
- 16 MiB aggregate images per response
- 256 KiB text per response
- 1,000 buffered items produced between executions
- 48 MiB daemon response

Dropping or truncating output sets `content_truncated`.

## Evaluation model

Each request is one JavaScript cell evaluated as a fresh `vm.SourceTextModule`. Meriyah, installed from an exact lockfile, identifies declarations, binding patterns, and static module syntax.

The runtime supports:

- top-level `await`
- optional text and image output through `repl` and console methods
- persistent `var`, `let`, `const`, function, and class bindings
- dynamic `import()`

Static imports/exports and top-level `return` are rejected. TypeScript is not supported.

A declaration registry performs cross-cell early-error checks before effects. `var` and function may redeclare one another; lexical declarations conflict with every prior declaration. Meriyah-guided lowering backs persistent names with context-global accessors, so closures, timers, and later cells share one binding rather than per-cell snapshots. Synthetic modules adapt dynamic imports into the cell context.

Lowering covers destructuring, nested top-level `var`, loop declaration heads, function hoisting, mutation, TDZ behavior, and partial multi-declarator initialization. Lexical names remain reserved after a failed initializer. Ordinary exceptions do not reset the process; initialized bindings remain observable according to JavaScript semantics.

`Function.prototype.toString()` may expose a generated function alias, but declared function `.name` is preserved.

## Runtime globals

Helpers are available both as bare globals and through a frozen `browser` object:

```js
await gotoUrl("https://example.com");
await browser.gotoUrl("https://example.com");
```

The complete helper reference, including signatures, behavior, and examples, lives in [`server/docs/repl.md`](../server/docs/repl.md). It covers navigation and page state, input, screenshots, tabs and iframe targets, waiting, page JavaScript, uploads, HTTP, raw CDP, and event draining. Wait helpers and CDP commands clamp their deadlines below the request deadline so routine helper failures return cleanly instead of destructively timing out the REPL.

### REPL helpers

```ts
type Repl = {
  readonly id: string;
  write(value: unknown): void;
  emitImage(image: ImageInput): Promise<void>;
};
```

These helpers are optional. `repl.write` emits a dedicated `write` content item without adding a newline and formats non-string values with bounded inspection. Console log/info/debug are captured as stdout and warn/error as stderr. Expression values are intentionally ignored, and an execution may produce no output. `emitImage` accepts PNG, JPEG, or WebP bytes, an image data URL, or a VM-local path. Screenshots remain file-oriented and can optionally be emitted:

```js
const path = await captureScreenshot("/tmp/page.png");
await repl.emitImage({path});
```

## Process and browser lifecycle

```text
client -> API -> Node child -> DevTools proxy :9222 -> Chromium :9223
```

The API serializes calls and is the sole supervisor. Startup creates a process group, configures Linux parent-death signaling, and waits for the Unix socket. Shutdown kills and waits for the group. A stale foreign listener is located through `/proc/net/unix` plus `/proc/<pid>/fd`, killed, and replaced; the API never adopts state from an earlier process.

The browser connection is lazy. Pure Node code works while Chromium is unavailable. The runtime maintains one browser WebSocket, one attached target/session, bounded events, network state, and pending dialog state. Chromium restart clears browser connection state only; the next helper reconnects without changing JavaScript bindings or `repl_id`.

Attach activates the target for deterministic dialog and input behavior. Domain enables and session commands are bounded so a renderer frozen behind a dialog returns a recovery error while browser-level tab commands remain available. A stale frozen tab can be reloaded, replaced, or closed.

## Failure semantics

Timeouts are destructive because abandoned JavaScript cannot safely coexist with later executions. On daemon timeout or API read deadline, the API kills the process group, waits, removes the socket, clears the child handle, and reports the terminated ID. The same reset applies to crash, OOM, mismatched IDs, malformed responses, and protocol corruption.

Unhandled promise rejections are bounded stderr output and do not reset state. Uncaught exceptions may leave Node inconsistent, so the daemon reports the active failure when possible and exits; the API marks the REPL terminated.

The child and API both serialize requests. Transport is newline-delimited JSON over a Unix socket, one connection per call. Request and response IDs plus `repl_id` must match. Socket half-close still permits the queued response to flush.

## Security and integrity

Callers can access Node built-ins, installed packages, filesystem, network, environment, processes, and CDP. Service integrity comes from bounded inputs/outputs, private serializer references resistant to prototype pollution, process destruction after unsafe failures, a configurable heap cap, and keeping protocol traffic off process stdout.

## Verification

Coverage includes:

- binding persistence, mutation, redeclaration, TDZ, closures, destructuring, partial failure, top-level await, dynamic import, and reset
- stable/new `repl_id` behavior across normal calls, browser restart, timeout, crash, OOM, and API shutdown
- zero-output executions, ordered text/images, and absence of output leakage after terminated executions
- edge-value representation, pollution resistance, and all output limits
- every seeded browser helper, reconnect, tabs, input, iframe, dialog, network idle, uploads, and screenshots
- both headless and headful images plus OpenAPI regeneration and SSE regression checks
