# Page recovery

Replays a top-level navigation that the site refused, so the caller driving the
browser does not have to.

A client asks for a page, the site answers `429 Too Many Requests`, and the
client is left holding a block page. A person in that position reloads. An agent
usually does not: it reads the document it was given, concludes the site is
unavailable, and says so. The work this package does is that reload, early
enough that the caller never sees the refusal.

Off by default. `PAGE_RECOVERY_ENABLED=true` turns it on for a browser.

## What it acts on

One `Fetch.requestPaused` notification, at the response stage, for a main-frame
document. Everything the decision needs is in the response:

| Condition | Replayed |
| --- | --- |
| `408`, `429`, `502`, `503`, `504`, `507` | yes |
| `ConnectionReset`, `ConnectionClosed`, `ConnectionFailed`, `ConnectionAborted`, `TimedOut` | yes |
| `403` | no |
| `502` carrying `X-Kernel-Proxy-Error` | no |
| Any status on a subresource, an iframe document, or a non-`GET` navigation | no |

`403` is absent because it is as often a settled answer about the session as a
throttle, and replaying a settled answer adds latency and requests without
changing anything. A branded `502` is Kernel's own egress failing rather than
the site refusing; `cdpmonitor` already reports those as typed `proxy_error`
events, and retrying one would spend the budget hiding a Kernel-side signal.
Non-`GET` is excluded because the replay preserves method and body: a POST a
gateway refused may still have been recorded upstream, and a duplicate order is
a worse outcome than a visible refusal.

## How the replay stays invisible

The refusal is answered with a `307` back to the same URL rather than being let
through. Chromium treats that as one more hop in the navigation that is already
in flight, so the client's `Page.navigate` — a Playwright `goto`, a Puppeteer
`goto` — resolves once, on the page it asked for, having waited out the retries.
It is not a second navigation, so nothing the client is waiting on is
interrupted.

Cookies the refusal set are still applied. The network stack processes
`Set-Cookie` before the request is paused, so a clearance cookie handed out by a
block page is present on the replay, which is the mechanism that makes a manual
reload work in the first place.

A fulfillment must carry a body, even an empty one. Chromium treats a
fulfillment without one as no fulfillment at all and lets the original response
through.

## Budget

Retries are bounded by attempts (`PAGE_RECOVERY_MAX_ATTEMPTS`, default 2) and by
wall clock (`PAGE_RECOVERY_BUDGET`, default 8s), per CDP session and URL. The
budget is sized against what the caller is waiting on: a `goto` typically
carries a 30s timeout with the real page load still to come. A block that needs
longer than the budget is a block the caller should see, so the refusal goes
through rather than turning into a navigation that looks hung.

`Retry-After` is honoured when the site sends one and it fits in what the budget
has left; otherwise the wait is exponential with full jitter from 300 ms. The
jitter matters more than the growth — many browser VMs retrying one throttled
origin in lockstep is how a short block becomes a long one.

A URL's budget is released once it answers with something that was not retried,
so the next navigation to it is judged on its own.

## Boundaries

It runs on its own CDP connection and browser-surface tracker, sharing no state
with `cdpmonitor` or WebMCP: interception has to run whether or not customer
telemetry is capturing, and a connection of its own is what keeps the two
lifecycles from having to agree. A client's own `Fetch` interception — what
Playwright's `page.route` installs — coexists with it; both see every request.

It does not cover a block that answers `200` and puts an interstitial in the
document, where the evidence is the rendered page rather than the status line.
Recognising those is the anti-bot extension's reading, and acting on one means a
real reload after the document has run, not a redirect before it. This package
deliberately handles only the half that can be decided from the response itself.

## Metrics

Served label-free on the existing `GET /metrics`:

| Metric | Meaning |
| --- | --- |
| `kernel_page_recovery_retries_total` | Navigations replayed after a refused response. |
| `kernel_page_recovery_recovered_total` | Replayed navigations that went on to answer below 400. |
| `kernel_page_recovery_exhausted_total` | Refusals passed through with the budget spent. |
| `kernel_page_recovery_up` | Whether interception is installed. Zero while off, during setup, and across reconnects. |

## Tests

```sh
go test -race ./lib/pagerecovery
KERNEL_PAGERECOVERY_CHROME_E2E=1 go test ./lib/pagerecovery -count=1 -v
```

The real-Chromium suite uses a local origin that refuses a fixed number of
requests per path. It checks the transparent case, an exhausted budget landing
the caller on the site's own answer, an unrefused navigation going untouched, a
`403` left alone, a client interceptor still seeing every request, and six tabs
refused at once each recovering independently. It does not cover image restart,
snapshot fork, or a site that blocks with a rendered page.
