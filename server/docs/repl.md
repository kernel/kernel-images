# Browser REPL

`POST /repl` evaluates JavaScript in a persistent Node.js runtime associated with one browser instance. The runtime keeps top-level bindings between calls and includes browser-control helpers as both bare globals and properties of the frozen `browser` object. The same frozen WebMCP client is available as `webmcp` and `browser.webmcp`.

For operational guidance aimed at browser-control agents, see [repl-agent-guidance.md](repl-agent-guidance.md).

```json
{
  "code": "const title = (await pageInfo()).title; title",
  "timeout_sec": 60,
  "reset": false
}
```

The endpoint is unrestricted code execution inside the browser VM, not a sandbox. Code can access Node built-ins, installed packages, files, environment variables, processes, and the network.

## Evaluation

- JavaScript only; TypeScript is not supported.
- Top-level `await` and dynamic `import()` are supported.
- Expression values are not returned automatically. A successful execution may produce zero output.
- Top-level `var`, `let`, `const`, function, and class bindings persist across calls.
- Static imports/exports and top-level `return` are rejected.
- Calls are serialized. Concurrent requests never execute at the same time, but callers that require a particular order should await each call because lock acquisition is not a public FIFO guarantee.
- Syntax errors and ordinary exceptions return `success: false` with `error` and, when available, `stack`; they do not terminate the REPL. A failed lexical initializer reserves its name until reset.
- Timeouts, crashes, OOMs, uncaught asynchronous exceptions, and protocol corruption terminate the current REPL. Such responses set `repl_terminated: true`; the next request automatically starts a fresh REPL with a new `repl_id`.

Use `{ "code": "", "reset": true }` to explicitly replace the REPL and clear all state.

## Output

Output is optional. Code may produce no content, use `repl.write(...)` or `repl.emitImage(...)`, call console methods, or combine those mechanisms.

`repl.write(value)` creates a `{type: "text", channel: "write"}` item without appending a newline, and non-string values receive a bounded inspection.

```js
const info = await pageInfo();
repl.write({url: info.url, title: info.title});
```

Expression values are intentionally ignored.

`console.log`, `console.info`, and `console.debug` are captured as `stdout`; `console.warn` and `console.error` use `stderr`.

`repl.emitImage(input)` creates ordered image output. It accepts PNG, JPEG, or WebP bytes, an image data URL, `{bytes, mimeType?}`, or `{path, mimeType?}`. `captureScreenshot()` writes a VM-local file; it can optionally be included in the response:

```js
const path = await captureScreenshot("/tmp/page.png");
await repl.emitImage({path});
```

`repl.id` is the CUID2 of the state-holding process and matches the response's `repl_id`.

## Browser helpers

Every helper below is available directly and under `browser`, for example `await gotoUrl(url)` and `await browser.gotoUrl(url)`.

- **`cdp(method, params?, sessionId?)`** — Send an unrestricted DevTools Protocol command. Omit `sessionId` for the attached page session; pass a target session ID explicitly, or `null` for a browser-level command. After a connection loss, only observational or idempotent setup commands may retry; mutations and page evaluation throw with an unknown outcome instead of risking duplicate execution.
- **`drainEvents()`** — Return and remove all buffered DevTools events across sessions. The connection-wide event ring retains at most the newest 500 events; each item includes its originating `sessionId` when DevTools supplied one.
- **`waitForEvent(method, options?)`** — Arm a one-shot DevTools event waiter before triggering an action. It matches the attached page session by default; use `sessionId: null` for a browser-level event or a session ID for another target. `predicate(event)` receives `{method, params, sessionId?, time}`. It returns that event or `null` after `timeoutSec` (default `30`), while connection and predicate failures throw. Attach a page with `ensureRealTab()` or `newTab()` before using the default session.
- **`gotoUrl(url)`** — Navigate the attached tab and return the raw `Page.navigate` result.
- **`pageInfo()`** — Return URL, title, viewport, document dimensions, scroll offset, ready state, and any pending JavaScript dialog.
- **`accessibilitySnapshot()`** — Return `{url, title, nodes}` from Chromium's computed accessibility tree. Each non-ignored DOM-backed node has `backendNodeId`, role, compacted accessible name, optional value, and control states. `backendNodeId` is Chromium's `DOM.BackendNodeId`; it can be passed directly to element helpers but becomes stale when navigation or DOM replacement removes that node.
- **`click(target, options?)`** — Click a CSS selector, an accessibility node or `{backendNodeId}`, or viewport coordinates `{x, y}`. Selector and backend-node clicks wait for a visible, enabled, stable, unobscured target, scroll it into view, and dispatch physical mouse input. Coordinate clicks dispatch immediately. Options are `button`, `clickCount`, and element-only `timeoutSec`.
- **`typeText(text)`** — Insert text into the currently focused element.
- **`fillInput(target, text, options?)`** — Target a selector, accessibility node, or `{backendNodeId}`; wait until it is visible, enabled, and editable; scroll and focus it; optionally clear it; type with physical-style key events; then dispatch `input` and `change`. Options are `clearFirst` (default `true`) and `timeoutSec` (default `10`).
- **`pressKey(key, modifiers?)`** — Send a physical-style key press using a self-contained US keyboard layout. Multi-character key names are case-insensitive and common aliases such as `Return`, `Esc`, and `Spacebar` are normalized. Single characters retain their exact case. Modifiers may be the DevTools bitfield (`1=Alt`, `2=Control`, `4=Meta`, `8=Shift`), an array such as `["Control"]`, or an object such as `{ctrl: true}`.
- **`scroll(x, y, dy?, dx?)`** — Dispatch one wheel event at viewport coordinates. Vertical `dy` defaults to `-300`; horizontal `dx` defaults to `0`. It never retries or substitutes another scrolling mechanism when the outcome is unknown; verify the resulting scroll state explicitly.
- **`dispatchKey(selector, key?, event?)`** — Dispatch one page-JavaScript keyboard event with `keyCode` and `which` on a selected element. Defaults to `key="Enter"` and `event="keypress"`; unlike `pressKey`, it does not synthesize native browser input.
- **`captureScreenshot(path?, fullPage?, maxDim?)`** — Capture a PNG to a VM-local path and return that path. The default is `/tmp/shot.png`. When set, `maxDim` post-processes the captured pixels so neither output dimension exceeds the positive integer limit, without enlargement. It does not emit the image automatically.
- **`listTabs(includeChrome?)`** — List page targets as `{targetId, title, url}`. Internal browser pages are included by default; pass `false` to exclude them.
- **`currentTab()`** — Return `{targetId, title, url}` for the attached tab.
- **`switchTab(target)`** — Attach to a target ID or a tab object returned by `listTabs()`/`currentTab()`, and return the DevTools session ID.
- **`newTab(url?)`** — Reuse the attached blank/new-tab page when possible; otherwise create and attach a blank tab. Navigate when `url` is supplied and return the target ID.
- **`closeTab(target?)`** — Close a target ID, a tab object, or the currently attached tab when omitted.
- **`ensureRealTab()`** — Keep or attach to an existing non-internal page and return its tab metadata; return `null` if none exists.
- **`iframeTarget(urlSubstring)`** — Find an out-of-process iframe target and return `{targetId, url, title, type}`, or return `null`. Use that `targetId` with `js(..., {targetId})` to inspect or manipulate cross-origin frame content.
- **`waitMs(milliseconds?)`** — Sleep for a number of milliseconds, defaulting to `1000`.
- **`waitForLoad(timeoutSec?)`** — Poll until `document.readyState === "complete"`; return `true` when loaded or `false` after the default 15-second timeout.
- **`waitForElement(target, options?)`** — Poll until a selector, accessibility node, or `{backendNodeId}` reaches `state: "attached" | "detached" | "visible" | "hidden"`; return `true` on success or `false` after `timeoutSec` (default `10`). State defaults to `"visible"`, and all selector matches are considered so a hidden duplicate cannot mask a visible match.
- **`waitForNetworkIdle(idleSec?, timeoutSec?)`** — Return `true` once no tracked requests remain in flight for the idle interval, or `false` on timeout. Defaults to 0.5 idle seconds and a 30-second timeout.
- **`js(expressionOrFunction, options?)`** — Evaluate a string expression or invoke a page function in the attached page or `options.targetId`, and return its by-value result. String expressions and returned promises are awaited. Function mode supports `return`, `await`, and one explicit `options.arg` value without capturing Browser REPL closures. DevTools edge result values such as bigint, `NaN`, infinities, and `-0` are decoded.
- **`uploadFile(target, pathOrPaths)`** — Set a selector-, accessibility-node-, or `{backendNodeId}`-targeted file input to one VM-local path or a non-empty array of paths.
- **`httpGet(url, headers?, timeoutSec?)`** — Fetch a URL from the VM and return the response body as text. Supports custom headers and a default 20-second timeout; non-2xx responses throw. Its timeout is clamped below the active execution deadline.

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

`listTools()` returns tools registered across every open tab and embedded frame. Each tool includes its opaque live `tool_ref`, input schema, annotations, and source window/tab/frame. Invocation uses that exact registration, so callers do not switch the Browser REPL's attached target for frame-provided tools.

Non-autosubmit declarative form tools return `status: "awaiting_submission"` after populating fields. Other invocations wait for a terminal result. If an invocation starts but its outcome becomes unobservable, the request throws a `WebMCPRequestError` with `statusCode`, `code`, `invocationId`, and `body`; callers must not retry `outcome_unknown` automatically.

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

Dropping or truncating output sets `content_truncated`.

`BROWSER_REPL_HEAP_MB` configures V8 old-space only. It is not a total RSS, CPU, or subprocess-tree quota. The Browser REPL has the same unrestricted process access and VM-level resource boundary as `/process/exec`; browser-VM/container resource controls remain the total process-tree budget.
