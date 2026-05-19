#!/usr/bin/env bash
set -ex -o pipefail

# Move to the script's directory so relative paths work regardless of the caller CWD
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$SCRIPT_DIR"
source ../../shared/ensure-common-build-run-vars.sh chromium-headless

# Directory on host where recordings will be saved when the API is enabled
HOST_RECORDINGS_DIR="$SCRIPT_DIR/recordings"
mkdir -p "$HOST_RECORDINGS_DIR"

# DETACHED=1 runs in background (--rm cleans up container on stop/failure teardown without -it)
DOCKER_RUN_FRONT=(-it --rm)
if [[ "${DETACHED:-}" == "1" ]]; then
  DOCKER_RUN_FRONT=(-d --rm)
fi

RUN_ARGS=(
  --name "$NAME"
  --privileged
  --tmpfs /dev/shm:size=2g
  -p 9222:9222
  -p 9224:9224
  -p 444:10001
  -v "$HOST_RECORDINGS_DIR:/recordings"
)

if [[ -n "${PLAYWRIGHT_ENGINE:-}" ]]; then
  RUN_ARGS+=( -e PLAYWRIGHT_ENGINE="$PLAYWRIGHT_ENGINE" )
fi

# S2 durable event storage (all three must be set to enable the sink)
if [[ -n "${S2_BASIN:-}" ]]; then
  RUN_ARGS+=( -e S2_BASIN="$S2_BASIN" )
fi
if [[ -n "${S2_ACCESS_TOKEN:-}" ]]; then
  RUN_ARGS+=( -e S2_ACCESS_TOKEN="$S2_ACCESS_TOKEN" )
fi
if [[ -n "${S2_STREAM:-}" ]]; then
  RUN_ARGS+=( -e S2_STREAM="$S2_STREAM" )
fi

# If a positional argument is given, use it as the entrypoint
ENTRYPOINT_ARG=()
if [[ $# -ge 1 && -n "$1" ]]; then
  ENTRYPOINT_ARG+=(--entrypoint "$1")
fi

docker rm -f "$NAME" 2>/dev/null || true

if [[ "${DETACHED:-}" == "1" ]]; then
  echo "Detached mode: Kernel HTTP API mapped to http://127.0.0.1:444 (container port 10001)"
  echo "CDP WS proxy http://127.0.0.1:9222  ChromeDriver proxy http://127.0.0.1:9224"
fi

docker run "${DOCKER_RUN_FRONT[@]}" "${ENTRYPOINT_ARG[@]}" "${RUN_ARGS[@]}" "$IMAGE"

if [[ "${DETACHED:-}" == "1" ]]; then
  echo "Container id/name: $NAME"
  echo "Stop with: docker stop $NAME"
fi
