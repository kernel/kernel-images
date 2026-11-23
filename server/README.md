# Kernel Images Server

A REST API server to start, stop, and download screen recordings.

## 🛠️ Prerequisites

### Required Software

- **Go 1.27.3+** - Programming language runtime
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
| `KERNEL_METRO_API_BASE_URL` | | Metro API base URL; answers the WebAuthn page hook (`PUT /webauthn/hook`) |
| `KERNEL_INSTANCE_JWT` | | Instance credential sent to the metro API, the OTLP relay, and the storage relay |
| `BTEL_STORAGE_PATH` | | Telemetry storage relay path on the metro API (e.g. `/btel-relay/v1/records`). When set, stored telemetry is posted to the relay, which appends it to the instance's S2 stream, and `S2_BASIN`/`S2_ACCESS_TOKEN` are ignored. The host is `KERNEL_METRO_API_BASE_URL` (or the fork identity's), else `BTEL_OTLP_ENDPOINT` with `BTEL_OTLP_INSECURE` picking the scheme |
| `BTEL_STORAGE_MAX_RECORDS` | `200` | Records per relay request (at most 1000) |
| `BTEL_STORAGE_MAX_BYTES` | `786432` | S2 metered bytes per relay request (at most 1 MiB) |
| `BTEL_STORAGE_LINGER` | `100ms` | How long a partial batch waits before it is sent |
| `BTEL_STORAGE_MAX_QUEUE` | `8192` | Records buffered for the relay; the oldest are dropped when full |
| `S2_BASIN`, `S2_ACCESS_TOKEN`, `S2_STREAM` | | Direct S2 storage, used when `BTEL_STORAGE_PATH` is unset. `S2_STREAM` also gates relay storage: nothing is stored until it (or the fork identity's stream) is known |

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
- **Process execution guide**: [`docs/processes.md`](docs/processes.md)
- **Browser REPL guide**: [`docs/repl.md`](docs/repl.md)

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
