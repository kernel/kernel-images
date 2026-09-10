# Browser REPL

`POST /repl` evaluates JavaScript in a persistent Node.js runtime associated with one browser instance. The runtime keeps top-level bindings between calls and includes browser-control helpers as both bare globals and properties of the frozen `browser` object. The same frozen WebMCP client is available as `webmcp` and `browser.webmcp`.

The endpoint is unrestricted code execution inside the browser VM, not a sandbox. Code can access Node built-ins, installed packages, files, environment variables, processes, and the network.

## Request and response

```http
POST /repl
Content-Type: application/json
```

```json
{
  "code": "const title = (await pageInfo()).title; repl.write(title)",
  "timeout_sec": 60,
  "reset": false
}
```

`code` is required, but may be empty when `reset` is `true`. `timeout_sec` is an integer from 1 through 300 and defaults to 60. `reset` defaults to `false`. Unknown request fields are rejected.

Execution success and JavaScript failures both return HTTP 200:

```json
{
  "success": true,
  "repl_id": "tz4a98xxat96iws9zmbrgj3a",
  "content": [
    {"type": "text", "channel": "write", "text": "Example Domain"}
  ],
  "content_truncated": false,
  "duration_ms": 12
}
```

`success` and `repl_id` are always present in an execution result. `content`, `content_truncated`, `duration_ms`, `error`, `stack`, and `repl_terminated` are included when applicable. Content items are ordered and are either text items like the example or image items shaped as `{type: "image", mime_type: "image/png", data_b64: "..."}`.

Invalid requests return HTTP 400, request bodies over 8 MiB return HTTP 413, and failure to start the REPL returns HTTP 500. These errors use the API's standard `{message}` error body.

## Evaluation

- JavaScript only; TypeScript is not supported.
- Top-level `await` and dynamic `import()` are supported. Static imports/exports and top-level `return` are rejected; CommonJS `require` is not preloaded.
- Expression values are not returned automatically. A successful execution may produce zero output.
- Top-level `var`, `let`, `const`, function, and class bindings persist across calls. Top-level `var` declarations inside control-flow statements persist too; function locals and nested block-scoped declarations do not.
- Normal JavaScript redeclaration rules apply across cells. `var` and function declarations may redeclare one another, while lexical declarations conflict with every prior declaration.
- Persistent names are live bindings: closures and timers observe assignments made by later cells. Declared function names are preserved, although `Function.prototype.toString()` may expose an internal generated alias.
- Calls are serialized. Concurrent requests never execute at the same time, but callers that require a particular order should await each call because admission order is not a public FIFO guarantee.
- Canceling a request while it is waiting for admission does not execute its code or replace healthy state. Cancellation after dispatch terminates the REPL because the execution outcome may be unknown. API shutdown rejects queued work, cancels active work, and terminates the child.
- Syntax errors and ordinary exceptions return `success: false` with `error` and, when available, `stack`; they do not terminate the REPL. A failed lexical initializer reserves its name in the temporal dead zone until reset. Earlier declarators that initialized before the failure remain initialized.
- A settled unhandled promise rejection does not terminate the REPL; it is emitted on `stderr`, immediately or with the next execution when it occurs between cells. An uncaught exception, timeout, crash, OOM, or protocol failure does terminate it. Such responses set `repl_terminated: true` when the API can return the terminated process's result; the next request starts a fresh REPL with a new `repl_id`.

Use `{ "code": "", "reset": true }` to explicitly replace the REPL and clear all state.

## Runtime globals

The context preloads `repl`, captured `console` methods, every browser helper, `browser`, `webmcp`, timers, `queueMicrotask`, `Buffer`, `process`, `fetch`, `URL`, `URLSearchParams`, text encoders/decoders, abort controllers/signals, `structuredClone`, `atob`, `btoa`, and `crypto`. Node built-ins and installed packages are available through dynamic `import()`.

`repl`, `browser`, and `webmcp` are frozen objects. `webmcp === browser.webmcp`, and each bare browser helper is the same function exposed on `browser`.

## Output

Output is optional. Code may produce no content, use `repl.write(...)` or `repl.emitImage(...)`, call console methods, or combine those mechanisms.

`repl.write(value)` creates a `{type: "text", channel: "write"}` item without appending a newline, and non-string values receive a bounded inspection.

```js
const info = await pageInfo();
repl.write({url: info.url, title: info.title});
```

Expression values are intentionally ignored.

`console.log`, `console.info`, `console.debug`, `console.dir`, and `console.table` are captured as `stdout`; `console.warn`, `console.error`, and `console.trace` use `stderr`. Console output does not append a newline.

Output produced by timers or settled promise rejections between executions is buffered and prepended to the next execution. The buffer retains the newest 1,000 items.

`repl.emitImage(input)` creates ordered image output. It accepts an `image/*` base64 data URL; PNG, JPEG, or WebP `Buffer`, `ArrayBuffer` view, or `ArrayBuffer` data; `{bytes, mimeType?}`; or `{path, mimeType?}`. Without an explicit MIME type, byte and file inputs must be recognizable as PNG, JPEG, or WebP. An explicit MIME type must be a short `image/*` value. `captureScreenshot()` writes a VM-local file; it can optionally be included in the response:

```js
const path = await captureScreenshot("/tmp/page.png");
await repl.emitImage({path});
```

`repl.id` is the CUID2 of the state-holding process and matches the response's `repl_id`.

## Browser helpers

Every helper below is available directly and under `browser`, for example `await gotoUrl(url)` and `await browser.gotoUrl(url)`.

- **`cdp(method, params?, sessionId?)`** — Send an unrestricted DevTools Protocol command. Omit `sessionId` for the attached target session; `Target.*`, `Browser.*`, `SystemInfo.*`, and `Storage.*` commands are automatically routed browser-wide. Pass a session ID explicitly for another attached target, or `null` to force browser-level routing. After a connection loss, only observational or idempotent setup commands may retry; mutations and page evaluation throw with an unknown outcome instead of risking duplicate execution.
- **`drainEvents()`** — Return and remove all buffered DevTools events across sessions. The connection-wide event ring retains at most the newest 500 events. Items have `{method, params, sessionId?, time}`, where `time` is the wall-clock observation time in Unix milliseconds.
- **`waitForEvent(method, options?)`** — Arm a one-shot DevTools event waiter before triggering an action. It matches the attached page session by default; use `sessionId: null` for a browser-level event or a session ID for another target. `predicate(event)` receives `{method, params, sessionId?, time}`. It returns that event or `null` after `timeoutSec` (default `30`), while connection and predicate failures throw. Attach a page with `ensureRealTab()` or `newTab()` before using the default session.
- **`gotoUrl(url)`** — Navigate the attached target and return the raw `Page.navigate` result. It does not wait for document load; use `waitForLoad()`, a rendered-state wait, or a pre-armed CDP event when synchronization is required.
- **`pageInfo()`** — Return `{url, title, viewport: {width, height}, scroll: {x, y}, page: {width, height}, ready_state, dialog}`. A pending JavaScript dialog freezes renderer evaluation, so in that case the helper returns the dialog and best-effort browser-level URL/title instead of the viewport/document fields.
- **`accessibilitySnapshot()`** — Return a flat `{url, title, nodes}` projection of Chromium's computed accessibility tree. Ignored nodes and nodes without a DOM backend ID are omitted. Each node has `backendNodeId`, role, whitespace-normalized accessible name, optional value, and available `checked`, `pressed`, `selected`, `expanded`, or `disabled` state. `checked` and `pressed` may be `"mixed"`. `backendNodeId` is Chromium's `DOM.BackendNodeId`; it can be passed directly to element helpers but becomes stale when navigation or DOM replacement removes that node.
- **`click(target, options?)`** — Click a CSS selector, an accessibility node or `{backendNodeId}`, or finite viewport coordinates `{x, y}`. Selector and backend-node clicks wait for a visible, enabled, stable, unobscured target, scroll it into view, and dispatch physical mouse input. Hidden duplicate selector matches are ignored; multiple visible matches are rejected. Coordinate clicks dispatch immediately. Options are `button: "left" | "right" | "middle"`, positive-integer `clickCount`, and element-only `timeoutSec` (default `10`). The helper does not wait for the action's resulting navigation or UI state.
- **`typeText(text)`** — Insert text into the currently focused element with CDP `Input.insertText`; it is text insertion, not a sequence of physical key presses.
- **`fillInput(target, text, options?)`** — Target a selector, accessibility node, or `{backendNodeId}`; wait until it is visible, enabled, and editable; scroll and focus it; optionally clear it; type with physical-style key events; then dispatch `input` and `change`. Options are `clearFirst` (default `true`) and `timeoutSec` (default `10`).
- **`pressKey(key, modifiers?)`** — Send one physical-style key-down/optional-char/key-up sequence using a self-contained US keyboard layout. Multi-character key names are case-insensitive and common aliases such as `Return`, `Esc`, and `Spacebar` are normalized. Single characters retain their exact case. Modifiers may be the DevTools bitfield (`1=Alt`, `2=Control`, `4=Meta`, `8=Shift`), an array such as `["Control"]`, or an object such as `{ctrl: true}`. Unknown named keys and modifiers throw.
- **`scroll(x, y, dy?, dx?)`** — Dispatch one wheel event at viewport coordinates. Vertical `dy` defaults to `-300`; horizontal `dx` defaults to `0`. It never retries or substitutes another scrolling mechanism when the outcome is unknown; verify the resulting scroll state explicitly.
- **`captureScreenshot(path?, fullPage?, maxDim?)`** — Capture a PNG to a VM-local path and return that path, overwriting an existing file. The default is `/tmp/shot.png`; `fullPage` defaults to `false`. When set, positive-integer `maxDim` post-processes the pixels so neither dimension exceeds the limit, without enlargement. It does not emit the image automatically.
- **`listTabs(includeChrome?)`** — List page targets as `{targetId, title, url}`. Internal browser pages are included by default; pass `false` to exclude them.
- **`currentTab()`** — Return `{targetId, title, url}` for the attached tab.
- **`switchTab(target)`** — Attach to a target ID or an object with `targetId` (including results from `listTabs()`, `currentTab()`, or `iframeTarget()`), and return the DevTools session ID. Selector and backend-node helpers subsequently operate on this attached target.
- **`newTab(url?)`** — Reuse the attached blank/new-tab page when possible; otherwise create and attach a blank tab. Navigate when `url` is supplied and return the target ID.
- **`closeTab(target?)`** — Close a target ID, an object with `targetId`, or the currently attached target when omitted. It waits up to five seconds, best effort, for the target to disappear from the browser target list.
- **`ensureRealTab()`** — Keep or attach to an existing non-internal page and return its tab metadata; return `null` if none exists.
- **`iframeTarget(urlSubstring)`** — Find an out-of-process iframe target and return `{targetId, url, title, type}`, or return `null`. Use that `targetId` with `js(..., {targetId})` to inspect or manipulate cross-origin frame content.
- **`waitMs(milliseconds?)`** — Sleep for a number of milliseconds, defaulting to `1000`. Prefer rendered state or authoritative events for synchronization.
- **`waitForLoad(timeoutSec?)`** — Poll until `document.readyState === "complete"`; return `true` when loaded or `false` after `timeoutSec` (default `15`).
- **`waitForElement(target, options?)`** — Poll until a selector, accessibility node, or `{backendNodeId}` reaches `state: "attached" | "detached" | "visible" | "hidden"`; return `true` on success or `false` after `timeoutSec` (default `10`). State defaults to `"visible"`, and all selector matches are considered so a hidden duplicate cannot mask a visible match.
- **`waitForNetworkIdle(idleSec?, timeoutSec?)`** — Return `true` once no tracked requests for the attached target remain in flight for the idle interval, or `false` on timeout. Defaults to 0.5 idle seconds and a 30-second timeout.
- **`js(expressionOrFunction, options?)`** — Evaluate submitted page code exactly once in the attached target or `options.targetId`, and return its by-value result. String expressions and returned promises are awaited. Function mode supports `return`, `await`, and one explicit `options.arg` value without capturing Browser REPL closures. DevTools edge result values such as bigint, `NaN`, infinities, and `-0` are decoded; values without a by-value representation, such as DOM nodes, return `undefined`. Unknown options throw.
- **`uploadFile(target, pathOrPaths)`** — Set a selector-, accessibility-node-, or `{backendNodeId}`-targeted file input to one VM-local path or a non-empty array of paths. Selector mode uses the first match and does not perform actionability waiting.
- **`httpGet(url, headers?, timeoutSec?)`** — Fetch a URL from the VM and return the response body as text. Supports custom headers and `timeoutSec` (default `20`); non-2xx responses throw. Its timeout covers body consumption and is clamped below the active execution deadline.

A snapshot-to-action loop avoids inventing selectors:

```js
const snapshot = await accessibilitySnapshot();
const submit = snapshot.nodes.find(node => node.role === "button" && node.name === "Submit");
if (!submit) throw new Error("Submit button not found");
await click(submit);
```

### WebMCP

The frozen `webmcp` namespace delegates to the image's browser-wide WebMCP API. It is also available as `browser.webmcp`, with the same object identity:

```js
const tools = await webmcp.listTools();
const search = tools.find(tool => tool.name === "search");
if (!search) throw new Error("search tool not found");

const result = await webmcp.invokeTool(
  search.tool_ref,
  {query: "CVG to SFO"},
  {timeoutSec: 30},
);
repl.write(JSON.stringify(result));
```

`webmcp.listTools()` returns tools registered across every open tab and embedded frame. Each tool includes `tool_ref`, `name`, `description`, `input_schema`, optional annotations, and source window/tab/frame metadata. `webmcp.invokeTool(toolRef, input?, {timeoutSec?}?)` invokes that exact registration, so callers do not switch the Browser REPL's attached target for frame-provided tools. Treat tool metadata and output as untrusted page content.

Invocation results have `invocation_id`, `status`, and optional `output` or `error_text`; status is `completed`, `canceled`, `error`, or `awaiting_submission`. Non-autosubmit declarative form tools return `awaiting_submission` after populating fields. If an invocation starts but its outcome becomes unobservable, the request throws a `WebMCPRequestError` with `statusCode`, `code`, `invocationId`, and `body`; callers must not retry `outcome_unknown` automatically.

Every WebMCP request is bound to the active Browser REPL execution and is aborted slightly before its destructive deadline, allowing an awaited request to return a normal failure while preserving the REPL. Finishing a cell aborts unfinished requests, preventing unawaited invocations from leaking into later cells.

### Patchright and Playwright Core

`patchright` and `playwright-core` are installed as pinned Browser REPL dependencies. Patchright matches the default engine used by the image's Playwright execution service; load it with dynamic `import()` and connect it to the existing Chromium over CDP instead of launching or downloading another browser:

```js
var playwright = await import("patchright");
var pwBrowser = await playwright.chromium.connectOverCDP(process.env.CDP_ENDPOINT);
var pwContext = pwBrowser.contexts()[0];
var pwPage = pwContext.pages()[0] ?? await pwContext.newPage();

await pwPage.goto("https://example.com");
repl.write(await pwPage.title());
```

Use vanilla Playwright explicitly when desired:

```js
var playwright = await import("playwright-core");
```

The imported module and browser objects are ordinary persistent Browser REPL bindings, so later cells can reuse `playwright`, `pwBrowser`, `pwContext`, and `pwPage`. Use a name such as `pwBrowser` for its browser connection; the bare `browser` name belongs to the frozen native helper namespace.

Patchright and Playwright use their own CDP connection alongside the native helpers. The native helper connection reconnects automatically after Chromium restarts, but an imported browser connection becomes disconnected. Reconnect explicitly while preserving other REPL state:

```js
if (!pwBrowser.isConnected()) {
  pwBrowser = await playwright.chromium.connectOverCDP(process.env.CDP_ENDPOINT);
  pwContext = pwBrowser.contexts()[0];
  pwPage = pwContext.pages()[0] ?? await pwContext.newPage();
}
```

A reset, execution timeout, crash, or API restart destroys the REPL process and therefore all imported modules, browser connections, and object bindings. Return values are not emitted automatically; continue to use `repl.write(...)`, console methods, or `repl.emitImage(...)` for output.

### Installing additional packages

Packages installed globally through the process execution API are immediately available to bare dynamic imports. Pin a version when reproducibility matters:

```http
POST /process/exec
Content-Type: application/json

{"command":"npm","args":["install","-g","example-package@1.2.3"]}
```

Then use the package in the Browser REPL without `require`:

```js
var examplePackage = await import("example-package");
```

The installation lasts for the browser VM's lifetime. Node caches imported modules within the REPL process; after replacing an installed version, reset the REPL before importing it again. Do not install into `/usr/local/lib/browser-repl`, because that directory contains the REPL's own locked runtime dependencies.

### Page JavaScript

`js()` has two explicit, single-execution modes. A string is evaluated directly as an expression:

```js
const title = await js("document.title");
const status = await js("fetch('/health').then(response => response.status)");
```

String mode does not accept top-level `return` or top-level `await` syntax. Use a page function for statement bodies, `return`, or `await`:

```js
const data = await js(async () => {
  const response = await fetch("/api/data");
  return response.json();
});
```

Page functions are serialized, invoked once in the page, and do not capture bindings from the Browser REPL. Pass one explicit by-value argument with `options.arg`:

```js
const selector = "main";
const text = await js(
  ({ selector, limit }) => document.querySelector(selector)?.innerText.slice(0, limit) ?? null,
  { arg: { selector, limit: 1000 } },
);
```

Arguments may contain `undefined`, `null`, booleans, strings, numbers (including `NaN`, infinities, and `-0`), bigint, arrays, and plain objects. Function values, symbol values, cycles, and non-plain class instances are rejected. Page exceptions and rejected promises throw from `js()`.

Use `options.targetId` for another target:

```js
const frame = await iframeTarget("checkout.example");
const title = await js(() => document.title, { targetId: frame.targetId });
```

Page functions execute in the web page, not the persistent Node Browser REPL. Navigation replaces their page execution context. Keep reusable automation functions in the Browser REPL and have them call `js()` with explicit arguments.

### Iframes

Same-origin frames are directly accessible from top-page JavaScript through `iframe.contentDocument`. Cross-site frames commonly run as separate DevTools targets; use `iframeTarget()` and `js(..., {targetId})` to evaluate inside them without relying on top-page same-origin access:

```js
const frame = await iframeTarget("checkout.example");
if (!frame) throw new Error("checkout frame not found");
const heading = await js(() => document.querySelector("h1")?.textContent, {
  targetId: frame.targetId,
});
```

Not every iframe is a separate target. For lower-level frame cases, use `cdp()` with `Page.getFrameTree`, `Page.createIsolatedWorld`, and `Runtime.evaluate`; unrestricted CDP access remains the escape hatch for inspecting and manipulating frame execution contexts. Selector helpers operate on the currently attached target, while coordinate `click({x, y})` can interact with the composed viewport.

Wait helpers and DevTools commands clamp internal deadlines below the request's `timeout_sec`, allowing waits to return `false` or `null` and command failures to return cleanly before the destructive execution timeout.

Pre-arm `waitForEvent()` when synchronization depends on a DevTools event that could fire before its triggering command returns:

```js
if (!(await ensureRealTab())) await newTab();
const completed = waitForEvent("Browser.downloadProgress", {
  sessionId: null,
  timeoutSec: 30,
  predicate: event => event.params.state === "completed",
});
await click('[aria-label="Download"]');
const event = await completed;
if (!event) throw new Error("download did not complete");
```

## Limits

- 8 MiB HTTP body and encoded daemon request line
- 8 MiB per image
- 16 MiB aggregate image data per response
- 256 KiB aggregate text per response
- 64 KiB error and 256 KiB stack text per response
- 10,000 ordered output items per execution
- 1,000 output items buffered between executions
- 48 MiB daemon response

Dropping or truncating content sets `content_truncated`. An individual image over 8 MiB throws; exceeding the aggregate image or item limits drops later content. Exceeding the daemon response limit is treated as protocol failure and terminates the REPL.

`BROWSER_REPL_HEAP_MB` configures V8 old-space only. It is not a total RSS, CPU, or subprocess-tree quota. The Browser REPL has the same unrestricted process access and VM-level resource boundary as `/process/exec`; browser-VM/container resource controls remain the total process-tree budget.
