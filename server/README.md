# Kernel Images Server

A REST API server to start, stop, and download screen recordings.

## 🛠️ Prerequisites

### Required Software

- **Go 1.24.3+** - Programming language runtime
- **ffmpeg** - Video recording engine
  - macOS: `brew install ffmpeg`
  - Linux: `sudo apt install ffmpeg` or `sudo yum install ffmpeg`
- **pnpm** - For OpenAPI code generation
  - `npm install -g pnpm`

### System Requirements

- **macOS**: Uses AVFoundation for screen capture
- **Linux**: Uses X11 for screen capture
- **Windows**: Not currently supported

## 🚀 Quick Start

### Running the Server

```bash
make dev
```

The server will start on port 10001 by default and log its configuration.

#### Example use

```bash
# 1. Start a new recording
curl http://localhost:10001/recording/start -d {}

# (recording in progress)

# 2. Stop recording
curl http://localhost:10001/recording/stop -d {}

# 3. Download the recorded file
curl http://localhost:10001/recording/download --output recording.mp4
```

### ⚙️ Configuration

Configure the server using environment variables:

| Variable       | Default   | Description                                 |
| -------------- | --------- | ------------------------------------------- |
| `PORT`         | `10001`   | HTTP server port                            |
| `METRICS_PORT` | `10002`   | Prometheus metrics port (`GET /metrics`)    |
| `FRAME_RATE`   | `10`      | Default recording framerate (fps)           |
| `DISPLAY_NUM`  | `1`       | Display/screen number to capture            |
| `MAX_SIZE_MB`  | `500`     | Default maximum file size (MB)              |
| `OUTPUT_DIR`   | `.`       | Directory to save recordings                |
| `FFMPEG_PATH`  | `ffmpeg`  | Path to the ffmpeg binary                   |

#### Example Configuration

```bash
export PORT=8080
export FRAME_RATE=30
export MAX_SIZE_MB=1000
export OUTPUT_DIR=/tmp/recordings
./bin/api
```

### API Documentation

- **YAML Spec**: `GET /spec.yaml`
- **JSON Spec**: `GET /spec.json`

### Secret-safe credential fills

Probe `GET /vault/fill/capabilities` and require HTTP 200 with `{"version":1}`
**before** transmitting credentials. Older images do not implement this route.
The probe verifies both the API and the daemon's version; it does not guarantee
that a page or browser connection will remain available.

`POST /vault/fill` accepts:

```json
{
  "bindings": [
    {"selector": "#username", "type": "email", "value": "resolved-value"},
    {"selector": "#password", "type": "password", "value": "resolved-value"},
    {"selector": "#otp", "type": "totp", "value": "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"}
  ],
  "page_url": "https://example.com/login",
  "timeout_ms": 10000
}
```

`page_url` is optional: without it, exactly one page must exist across all browser
contexts; with it, exactly one page must match the URL literally. Bindings are
ordered (1–100); `type` is `text`, `email`, `password`, or `totp`. The whole executor
deadline is 1–30000 ms (default 10000), excluding daemon startup and a 2-second
transport grace period. Bodies are limited to 1 MiB; selectors to 4096 bytes,
values to 65536 bytes, and page URLs to 8192 bytes.

Every selector must match exactly one element across all frames. It must be a
visible editable input/textarea or contain exactly one editable input/textarea.
Contenteditable elements are not supported. All targets and Base32 seeds are
preflighted before writing. Duplicate targets fail even when selectors differ.
Pinned element handles are never re-resolved after replacement or navigation.
TOTP uses RFC 6238 SHA1, six digits and 30-second periods; the seed is decoded
during preflight and the code is generated immediately before its write.

HTTP 200 returns only ordered indices and statuses:

```json
{"status":"filled","fields":[{"index":0,"status":"filled"},{"index":1,"status":"filled"},{"index":2,"status":"filled"}]}
```

Field statuses are `filled`, `failed`, `not_attempted`, or `unknown`; aggregate
statuses are `filled`, `failed`, `partial`, or `unknown`. A preflight error marks
its field `failed`, leaving all others `not_attempted` (page-selection errors mark
index 0). `partial` means an earlier field was filled before a later failure.
A failure after starting a write is conservatively `unknown`, even if the browser
may have rejected it before mutation. Transport failures after dispatch return all
fields `unknown`. Do not automatically retry unknown outcomes. HTTP 400 returns
`{"message":"invalid_request"}`; HTTP 503 returns
`{"message":"executor_unavailable"}` before any write, including when busy.

This uses the existing session API trust boundary, not a new authentication
scheme. The control plane must authorize vault/session access and gate the image
capability. It must not log this body or route credentials through
`/playwright/execute`. The dedicated daemon method never generates user code or
returns raw errors. The daemon disables Playwright debug logging because it can
contain fill values; API telemetry contains operation metadata only.

The executor never clicks or submits, but site input handlers can submit or
otherwise react to a fill. Browser access, site scripts, independently enabled
browser tracing, or page-generated console/network telemetry can expose filled
values. This endpoint does not make the page a secret storage boundary, prevent
concurrent browser clients from modifying it, or erase immutable strings from
process memory. No seed is sent to the page.

## 🔧 Development

### Code Generation

The server uses OpenAPI code generation. After modifying `openapi.yaml`:

```bash
make oapi-generate
```

## 🧪 Testing

### Running Tests

```bash
make test
```

The vault runtime unit tests run with `make test-runtime` without a browser.
The local Chromium/daemon integration tests are opt-in. With `playwright-core`,
`patchright`, and `esbuild` installed where Node can resolve them, run:

```bash
VAULT_FILL_BROWSER_TESTS=1 node --test runtime/vault-fill*.test.ts
VAULT_FILL_BROWSER_TESTS=1 VAULT_FILL_ENGINE=patchright node --test runtime/vault-fill-daemon.test.ts
```

These use `/usr/bin/chromium` by default; override with `CHROMIUM_PATH`. They test
actual pinned handles, cross-frame/page selection, no-write preflight failures,
TOTP vectors, navigation/detachment, deadline cancellation, and daemon response/log
redaction even with Playwright debugging enabled. No image deployment is needed.
