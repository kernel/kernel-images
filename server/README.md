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

### Persistent Browser REPL

`POST /browser/execute` evaluates JavaScript in a persistent Node.js
runtime that is preloaded with browser-control helpers and an unrestricted
`cdp()` escape hatch.

- The runtime starts lazily on the first request and is owned directly by the
  API process. API restart/shutdown kills it (with Linux parent-death
  signaling as a backstop); an API restart therefore loses all REPL state.
- Each REPL process gets a CUID2 `repl_id`, returned in every response. It is
  stable across calls and Chromium reconnects, and changes after an API
  restart, `reset: true`, an execution timeout, or a REPL crash.
- Top-level `await`, persistent `let`/`const`/`var`/function/class bindings,
  dynamic `import()`, and implicit final-expression results are supported.
  Persistent names are live context-global accessors, so closures and timers
  observe later-cell assignments. Function declarations are lowered through
  those accessors too, including same-cell closures and assignments. `var`
  declarations in top-level nested statements persist, including object/array
  rest destructuring and `for...of` declaration heads; locals inside functions
  or nested lexical blocks do not.
  Braceless multi-declarator `var` statements retain their single-statement
  control-flow semantics. Lexical names are reserved after linking: retry a
  failed declaration with a new name or use `reset: true`. Function `.name` is
  preserved; `Function.prototype.toString()` may expose the generated internal
  alias. Static top-level imports are rejected; use dynamic `import()` instead.
  The implicit result is always the value of the final expression statement, so
  end the snippet with an expression to return a value. Top-level `return` is
  rejected.
- A timeout is destructive (JavaScript cannot be interrupted safely): the API
  kills the REPL process group and responds with `repl_terminated: true` and
  the terminated REPL's ID. The next request lazily starts a fresh REPL.
- Output is an ordered `content` array of typed items: text (`write` =
  `repl.write`, `stdout` = `console.log/info/debug`, `stderr` =
  `console.warn/error`) and images (`repl.emitImage`, base64 with MIME
  sniffing). Limits: 8 MiB per image, 16 MiB aggregate image data, 256 KiB
  combined text, and 256 KiB serialized result per response; violations set
  `content_truncated` / `result_truncated` instead of failing silently; stray
  output, including images emitted between executions, is capped at 1,000
  items and reports `content_truncated` when older items are discarded.
  Request bodies are limited to 8 MiB before strict decoding, and the API
  rejects any marshaled daemon request that would exceed
  the daemon's 8 MiB newline-delimited request-line limit without terminating
  the REPL. HTML-sensitive code is sent without JSON HTML escaping.
- `capture_screenshot()` stays file-oriented (returns a VM path); emit it
  explicitly with `await repl.emitImage({ path })`.
- Helpers are exposed both as bare globals (`goto_url`, `page_info`,
  `click_at_xy`, `type_text`, `fill_input`, `press_key`, `scroll`,
  `capture_screenshot`, `list_tabs`, `current_tab`, `switch_tab`, `new_tab`,
  `close_tab`, `ensure_real_tab`, `iframe_target`, `wait`, `wait_for_load`,
  `wait_for_element`, `wait_for_network_idle`, `js`, `dispatch_key`,
  `upload_file`, `http_get`, `start_recording`, `stop_recording`,
  `recording_dir`, `drain_events`, `cdp`) and on the frozen `browser`
  namespace. Recording helpers delegate to the Kernel recording API.
  `press_key` accepts modifiers as an array (`["Control"]`) or an object
  (`{ctrl: true}`). Wait-style helpers (`wait_for_load`, `wait_for_element`,
  `wait_for_network_idle`) clamp their internal deadline to just below the
  request's `timeout_sec`, so a routine wait miss returns the helper's own
  error instead of tying the destructive execution timeout.
- The REPL connects to the browser through the DevTools proxy on
  `ws://127.0.0.1:9222`, lazily on the first browser helper call; pure
  Node.js code runs fine while Chromium is down, and the connection is
  re-established automatically after a Chromium restart.

**Security**: this endpoint is unrestricted code execution inside the browser
VM (filesystem, network, processes, environment), equivalent in trust level
to the process and Playwright execution APIs. The `vm` context is a state
container, not a sandbox.

The daemon sources live in `server/runtime/` (`browser-repl.ts`,
`browser-cdp-client.ts`, `browser-helpers.ts`) and are bundled to
`/usr/local/lib/browser-repl.js` in both browser images.

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
