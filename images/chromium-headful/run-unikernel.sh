#!/usr/bin/env bash
set -euo pipefail

# Move to the script's directory so relative paths work regardless of the caller CWD
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$SCRIPT_DIR"
source ../../shared/ensure-common-build-run-vars.sh chromium-headful require-ukc-vars

kraft cloud inst rm $NAME || true

# Name for the Kraft Cloud volume that will carry Chromium flags
volume_name="${NAME}-flags"

# ------------------------------------------------------------------------------
# Prepare Kraft Cloud volume containing Chromium flags
# ------------------------------------------------------------------------------
# Build a temporary directory with a single file "flags" that holds all
# Chromium runtime flags. This directory will be imported into a Kraft Cloud
# volume which we then mount into the image at /chromium.
# RUN_AS_ROOT defaults to true in unikernel (for now, until we figure it out)
RUN_AS_ROOT="${RUN_AS_ROOT:-true}"

chromium_flags_default="--user-data-dir=/home/kernel/user-data --disable-dev-shm-usage --disable-gpu --start-maximized --disable-software-rasterizer --remote-allow-origins=*"
if [[ "$RUN_AS_ROOT" == "true" ]]; then
  chromium_flags_default="$chromium_flags_default --no-sandbox --no-zygote"
fi
CHROMIUM_FLAGS="${CHROMIUM_FLAGS:-$chromium_flags_default}"
rm -rf .tmp/chromium
mkdir -p .tmp/chromium
FLAGS_DIR=".tmp/chromium"

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
echo "$FLAGS_JSON" > "$FLAGS_DIR/flags"

echo "flags file: $FLAGS_DIR/flags"
cat "$FLAGS_DIR/flags"

# Re-create the volume from scratch every run
kraft cloud volume rm "$volume_name" || true
kraft cloud volume create -n "$volume_name" -s 16M
# Import the flags directory into the freshly created volume
kraft cloud volume import --image "${VOLIMPORT_PREFIX:-onkernel}/utils/volimport:1.0" -s "$FLAGS_DIR" -v "$volume_name"

# Ensure the temp directory is cleaned up on exit
trap 'rm -rf "$FLAGS_DIR"' EXIT


deploy_args=(
  --vcpus ${VCPUS:-4}
  -M 4096
  -p 9222:9222/tls
  -p 444:10001/tls
  -e DISPLAY_NUM=1
  -e HEIGHT=1080
  -e WIDTH=1920
  -e TZ=${TZ:-'America/Los_Angeles'}
  -e RUN_AS_ROOT="$RUN_AS_ROOT"
  -e LOG_CDP_MESSAGES=true
  -v "$volume_name":/chromium
  -n "$NAME"
)

# Telemetry configuration (only 2 flags)
# Usage: TELEMETRY_ENDPOINT=https://api.example.com/telemetry ./run-unikernel.sh
# If TELEMETRY_ENDPOINT is set, telemetry is enabled; if not set, telemetry is disabled
if [[ -n "${TELEMETRY_ENDPOINT:-}" ]]; then
  echo "Telemetry ENABLED - endpoint: $TELEMETRY_ENDPOINT"
  deploy_args+=( -e TELEMETRY_ENDPOINT="$TELEMETRY_ENDPOINT" )
  if [[ -n "${TELEMETRY_CAPTURE:-}" ]]; then
    deploy_args+=( -e TELEMETRY_CAPTURE="$TELEMETRY_CAPTURE" )
  fi
fi

if [[ "${ENABLE_WEBRTC:-}" == "true" ]]; then
  echo "Deploying with WebRTC enabled"
  kraft cloud inst create --start \
    "${deploy_args[@]}" \
    -p 443:8080/http+tls \
    -e ENABLE_WEBRTC=true \
    -e NEKO_ICESERVERS="${NEKO_ICESERVERS:-}" "$IMAGE"
else
  echo "Deploying without WebRTC"
  kraft cloud inst create --start \
    "${deploy_args[@]}" \
    -p 443:6080/http+tls \
    "$IMAGE"
fi
