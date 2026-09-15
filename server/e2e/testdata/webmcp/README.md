# Declarative WebMCP fixtures

`reservation.html` uses native form annotations from the [Chrome declarative API](https://developer.chrome.com/docs/ai/webmcp/declarative-api), modeled on the [Chrome Labs french-bistro demo](https://github.com/GoogleChromeLabs/webmcp-tools/tree/main/demos/french-bistro). Unlike the demo's manual-submit form, it sets `toolautosubmit` so invocation fills and submits the form. The submit handler updates a visible confirmation and returns the form data through `SubmitEvent.respondWith`. There is no imperative registration, polyfill, or external dependency.

`embedded.html` embeds the same form to exercise frame discovery, provenance, and invocation. Like the audio e2e fixture, both pages are uploaded into the instance and loaded over `file://`, avoiding external URLs and a host-access dependency. The declarative subtests reuse `TestPlaywrightExecuteAPI`'s container and warm Playwright daemon.

Run from the repository root with Docker running:

```sh
DOCKER_BUILDKIT=1 docker build -f images/chromium-headless/image/Dockerfile -t kernel-headless-test .
cd server
E2E_CHROMIUM_HEADLESS_IMAGE=kernel-headless-test \
  GOFLAGS='-run=TestPlaywrightExecuteAPI/WebMCPDeclarative -count=1' make test-e2e
```

Chromium 152.0.7977.42 with the image's default WebMCP flags exposes `reserve_table` with string name/date fields, a numeric party size with bounds, and a seating enum. It adds a date format hint to the field description. Both top-level and embedded invocations return `completed` and the submitted values; the tests also read the DOM through `/playwright/execute` to verify native agent submission occurred exactly once. Discovery and invocation responses are logged by the test. Missing declarative tools fail the test rather than silently skipping supported behavior.
