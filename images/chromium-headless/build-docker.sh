#!/usr/bin/env bash
set -e -o pipefail

# Move to the script's directory so relative paths work regardless of the caller CWD
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
cd "$SCRIPT_DIR"
source ../../shared/ensure-common-build-run-vars.sh chromium-headless

source ../../shared/start-buildkit.sh

# Build the Docker image using the repo root as build context
# so the Dockerfile's first stage can access the server sources
(cd "$SCRIPT_DIR/../.." && docker build --platform "$DOCKER_PLATFORM" -f images/chromium-headless/image/Dockerfile -t "$IMAGE" .)
