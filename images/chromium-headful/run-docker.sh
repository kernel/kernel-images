#!/usr/bin/env bash
set -ex -o pipefail

# Move to the script's directory so relative paths work regardless of the caller CWD
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$SCRIPT_DIR"
source ../../shared/ensure-common-build-run-vars.sh chromium-headful

# Directory on host where recordings will be saved
HOST_RECORDINGS_DIR="$SCRIPT_DIR/recordings"
mkdir -p "$HOST_RECORDINGS_DIR"

# RUN_AS_ROOT defaults to false in docker
RUN_AS_ROOT="${RUN_AS_ROOT:-false}"

# Build Chromium flags file and mount
CHROMIUM_FLAGS_DEFAULT="--user-data-dir=/home/kernel/user-data --disable-dev-shm-usage --disable-gpu --start-maximized --disable-software-rasterizer --remote-allow-origins=*"
if [[ "$RUN_AS_ROOT" == "true" ]]; then
  CHROMIUM_FLAGS_DEFAULT="$CHROMIUM_FLAGS_DEFAULT --no-sandbox --no-zygote"
fi
CHROMIUM_FLAGS="${CHROMIUM_FLAGS:-$CHROMIUM_FLAGS_DEFAULT}"
rm -rf .tmp/chromium
mkdir -p .tmp/chromium
FLAGS_FILE="$(pwd)/.tmp/chromium/flags"

# Convert space-separated flags to JSON array format, handling quoted strings
# Use eval to properly parse quoted strings (respects shell quoting)
if [ -n "$CHROMIUM_FLAGS" ]; then
  eval "FLAGS_ARRAY=($CHROMIUM_FLAGS)"
else
  FLAGS_ARRAY=()
fi

FLAGS_JSON='{"flags":['
FIRST=true
for flag in "${FLAGS_ARRAY[@]}"; do
  if [ -n "$flag" ]; then
    if [ "$FIRST" = true ]; then
      FLAGS_JSON+="\"$flag\""
      FIRST=false
    else
      FLAGS_JSON+=",\"$flag\""
    fi
  fi
done
FLAGS_JSON+=']}'
echo "$FLAGS_JSON" > "$FLAGS_FILE"

echo "flags file: $FLAGS_FILE"
cat "$FLAGS_FILE"

# Build docker run argument list
RUN_ARGS=(
  --name "$NAME"
  --privileged
  --tmpfs /dev/shm:size=2g
  -v "$HOST_RECORDINGS_DIR:/recordings"
  --memory 8192m
  -p 9222:9222
  -p 444:10001
  -e DISPLAY_NUM=1
  -e HEIGHT=1080
  -e WIDTH=1920
  -e TZ=${TZ:-'America/Los_Angeles'}
  -e RUN_AS_ROOT="$RUN_AS_ROOT"
  --mount type=bind,src="$FLAGS_FILE",dst=/chromium/flags
)

if [[ -n "${PLAYWRIGHT_ENGINE:-}" ]]; then
  RUN_ARGS+=( -e PLAYWRIGHT_ENGINE="$PLAYWRIGHT_ENGINE" )
fi

# Telemetry configuration (only 2 flags)
# Usage: TELEMETRY_ENDPOINT=https://api.example.com/telemetry ./run-docker.sh
# If TELEMETRY_ENDPOINT is set, telemetry is enabled; if not set, telemetry is disabled
if [[ -n "${TELEMETRY_ENDPOINT:-}" ]]; then
  echo "Telemetry ENABLED - endpoint: $TELEMETRY_ENDPOINT"
  RUN_ARGS+=( -e TELEMETRY_ENDPOINT="$TELEMETRY_ENDPOINT" )
  if [[ -n "${TELEMETRY_CAPTURE:-}" ]]; then
    RUN_ARGS+=( -e TELEMETRY_CAPTURE="$TELEMETRY_CAPTURE" )
  fi
fi

# WebRTC port mapping
if [[ "${ENABLE_WEBRTC:-}" == "true" ]]; then
  echo "Running container with WebRTC"
  RUN_ARGS+=( -p 8080:8080 )
  RUN_ARGS+=( -e ENABLE_WEBRTC=true )
  if [[ -n "${NEKO_ICESERVERS:-}" ]]; then
    RUN_ARGS+=( -e NEKO_ICESERVERS="$NEKO_ICESERVERS" )
  else
    RUN_ARGS+=( -e NEKO_WEBRTC_EPR=56000-56100 )
    RUN_ARGS+=( -e NEKO_WEBRTC_NAT1TO1=127.0.0.1 )
    RUN_ARGS+=( -p 56000-56100:56000-56100/udp )
  fi
fi

docker rm -f "$NAME" 2>/dev/null || true
docker run -it "${RUN_ARGS[@]}" "$IMAGE"
