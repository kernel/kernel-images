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
| `AGENT_CONFIG_PATH` | empty | Trusted launch catalog enabling the optional ACP WebSocket proxy |

#### Example Configuration

```bash
export PORT=8080
export FRAME_RATE=30
export MAX_SIZE_MB=1000
export OUTPUT_DIR=/tmp/recordings
./bin/api
```

### Optional ACP agents

The [ACP API](lib/agentproxy/README.md) exposes one WebSocket endpoint using
`acpremote expose` and its per-connection process lifecycle. Browser images bundle
a pinned Pi runtime and declarative configuration GET/PUT endpoints. Configure a
provider credential binding before connecting. An empty `AGENT_CONFIG_PATH`
disables these routes when running the server separately.

### API Documentation

- **YAML Spec**: `GET /spec.yaml`
- **JSON Spec**: `GET /spec.json`

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
