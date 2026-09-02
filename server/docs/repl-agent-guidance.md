# Browser REPL Agent Guidance

Use `POST /repl` as the primary browser-control interface. Browser actions, page inspection, synchronization, and extraction should happen through Browser REPL helpers rather than a separate automation framework.

For the complete API contract and lifecycle semantics, see [repl.md](repl.md).

## Request shape

```http
POST /repl
Content-Type: application/json
```

```json
{
  "code": "JavaScript source",
  "timeout_sec": 60
}
```

Browser REPL declarations persist across requests. Browser REPL expression values are ignored, so use `repl.write(...)` when response text is needed:

```js
const info = await pageInfo();
repl.write(JSON.stringify(info));
```

Successful executions may emit text, images, console output, any combination of those, or no output.

## Browser helpers

Helpers are available as bare globals and under the frozen `browser` namespace:

```text
cdp, drainEvents, gotoUrl, pageInfo,
click, typeText, fillInput, pressKey, scroll,
captureScreenshot,
listTabs, currentTab, switchTab, newTab, closeTab,
ensureRealTab, iframeTarget,
waitMs, waitForLoad, waitForElement, waitForNetworkIdle,
js, dispatchKey, uploadFile, httpGet
```

Common signatures:

```js
js(expressionOrFunction, options?)
waitMs(milliseconds = 1000)
waitForLoad(timeoutSec = 15)
waitForElement(selector, {state = "visible", timeoutSec = 10} = {})
waitForNetworkIdle(idleSec = 0.5, timeoutSec = 30)
click(selectorOrPoint, options?)
fillInput(selector, text, {clearFirst = true, timeoutSec = 10} = {})
pressKey(key, modifiers?)
```

## Page JavaScript

`js()` has two explicit modes and does not intentionally retry user code.

### String expression

A string is evaluated directly. If its value is a Promise, the Promise is awaited.

```js
const title = await js("document.title");
const status = await js("fetch('/health').then(response => response.status)");
```

String mode does not support top-level `return` or top-level `await` syntax.

### Page function

Pass a function when the page code needs statements, `return`, or `await`:

```js
const data = await js(async () => {
  const response = await fetch("/api/data");
  return response.json();
});
```

Page functions execute in the web page and do not capture Browser REPL bindings. Pass data explicitly through `options.arg`:

```js
const selector = "main";
const text = await js(
  ({ selector, limit }) =>
    document.querySelector(selector)?.innerText.slice(0, limit) ?? null,
  { arg: { selector, limit: 1000 } },
);
```

Use `options.targetId` for another tab or out-of-process iframe target:

```js
const frame = await iframeTarget("checkout.example");
if (!frame) throw new Error("checkout frame not found");
const title = await js(() => document.title, {
  targetId: frame.targetId,
});
```

Same-origin iframes are accessible through `iframe.contentDocument`. Not every iframe is a separate target; use raw `cdp()` with `Page.getFrameTree`, `Page.createIsolatedWorld`, and `Runtime.evaluate` when direct frame-context control is needed.

Returned page data should be primitive values, arrays, or plain objects rather than DOM nodes.

## Synchronization policy

Avoid `waitMs()` for ordinary UI transitions. Wait for the rendered state that the next action requires.

### Element waiting

Use `waitForElement()` with stable semantic selectors based on roles, accessible labels, placeholders, or state attributes:

```js
const ready = await waitForElement(
  '[role="option"][aria-label*="(CVG)"]',
  {state: "visible", timeoutSec: 15},
);

if (!ready) {
  throw new Error("CVG airport option did not appear");
}
```

`waitForElement()` supports `attached`, `detached`, `visible`, and `hidden` states and considers all selector matches. It returns:

- `true` when the requested state is reached
- `false` when its timeout expires

It does **not** throw merely because the element was not found before timeout. Always inspect the boolean result before continuing.

Useful semantic selector patterns include:

```js
'[role="option"]'
'[role="option"][aria-label*="(CVG)"]'
'[role="option"][aria-label*="(SFO)"]'
'input[aria-label^="Where from"]'
'input[aria-label^="Where to"]'
'input[aria-label="Departure"]'
'[role="link"][aria-label$="Select flight"]'
```

Do not assume an ARIA role implies a particular HTML tag. For example, a rendered link control may be a `div[role="link"]`, not an `<a>`.

### Navigation and network waiting

Use `waitForLoad()` for actual document navigation:

```js
await gotoUrl(url);
if (!(await waitForLoad(30))) {
  throw new Error("document did not finish loading");
}
```

Use `waitForNetworkIdle()` only when no specific rendered element identifies completion. Network idleness alone does not prove that the desired UI exists.

`drainEvents()` is useful for protocol diagnostics, but a generic DevTools event does not prove that an application completed its UI transition.

### Fixed delays

Use `waitMs()` only when a real pacing delay is unavoidable. Do not hide arbitrary sleeps inside a custom helper. When a fixed delay is necessary, record:

1. Its duration
2. Why no rendered DOM or ARIA condition was available
3. What failed when the delay was removed

## Semantic interaction

Prefer semantic DOM and accessibility metadata over generated CSS classes.

After each significant action, verify the resulting state rather than assuming the action succeeded:

```js
await fillInput('input[aria-label^="Where from"]', "CVG");

if (!(await waitForElement(
  '[role="option"][aria-label*="(CVG)"]',
  {state: "visible", timeoutSec: 15},
))) {
  throw new Error("origin autocomplete did not render CVG");
}

const options = await js(() =>
  [...document.querySelectorAll('[role="option"]')]
    .filter(element => element.getClientRects().length > 0)
    .map(element => ({
      text: element.innerText.trim(),
      aria: element.getAttribute("aria-label"),
    })),
);
```

Use selector clicks for semantic interaction. They wait for one visible, enabled, stable, unobscured match, scroll it into view, and dispatch physical mouse input:

```js
await click('button[aria-label="Search"]');
```

Use the same helper with viewport coordinates only when no semantic selector is available:

```js
await click({x: 420, y: 315});
```

After actions that close transient UI, wait explicitly for the closing state before continuing:

```js
await click(doneSelector);
if (!(await waitForElement(doneSelector, {
  state: "hidden",
  timeoutSec: 5,
}))) {
  throw new Error("date dialog did not close");
}
```

## Keyboard input

`pressKey()` uses a US keyboard layout. Multi-character key names are case-insensitive, and common aliases are normalized:

```js
await pressKey("ENTER");
await pressKey("Esc");
await pressKey("Return");
await pressKey("Digit1", ["Shift"]); // emits "!"
await pressKey("a", { ctrl: true });
```

A key event being accepted only proves that the event was dispatched. Verify the resulting input value, selection, dialog state, or other rendered effect afterward.

## Reusable declarations

Define reusable automation functions in the persistent Browser REPL and pass page arguments explicitly into `js()`:

```js
async function visibleControls(pattern = "") {
  return js(
    ({ pattern }) => {
      const matcher = new RegExp(pattern, "i");
      return [...document.querySelectorAll(
        'button,input,[role="button"],[role="combobox"],[role="option"]',
      )]
        .filter(element => element.getClientRects().length > 0)
        .map(element => ({
          tag: element.tagName,
          role: element.getAttribute("role"),
          aria: element.getAttribute("aria-label"),
          text: (element.innerText || element.value || "")
            .trim()
            .replace(/\s+/g, " ")
            .slice(0, 160),
        }))
        .filter(control =>
          matcher.test([
            control.role,
            control.aria,
            control.text,
          ].join(" ")),
        );
    },
    { arg: { pattern } },
  );
}
```

Filter large DOM, accessibility, or event results inside the Browser REPL. Emit only the compact data needed for the next decision.

## Performance guidance

- Batch related actions when their intermediate states do not require inspection.
- Keep reusable declarations across requests.
- Prefer semantic state waits over fixed delays.
- Check boolean wait results immediately so an incorrect selector fails at its first point of use.
- Use narrow output projections instead of returning full DOM or accessibility trees.

## Screenshots

Screenshots are fallback diagnostics rather than the primary control mechanism:

```js
const path = await captureScreenshot("/tmp/shot.png", false, 1800);
await repl.emitImage({ path });
```

`captureScreenshot()` writes a VM-local file. It does not emit the image unless `repl.emitImage()` is called explicitly.
