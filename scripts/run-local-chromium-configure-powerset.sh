#!/usr/bin/env bash
# 1) Build the headless chromium image via images/chromium-headless/build-docker.sh
# 2) Run chromium/configure multipart powerset e2e (31 part combinations + JSON start_url + legacy bare-start_url test).
#
# Prereqs: Docker, Go, network for pulls.
#
# Tests use testcontainers-go (dynamic host ports).
# For manual experiments: DETACHED=1 ./images/chromium-headless/run-docker.sh → API on http://127.0.0.1:444
#
# Usage:
#   ./scripts/run-local-chromium-configure-powerset.sh
#   IMAGE=onkernel/chromium-headless-test:mytag ./scripts/run-local-chromium-configure-powerset.sh
#
# Skip image rebuild:
#   ./scripts/run-local-chromium-configure-powerset.sh --skip-build

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SKIP_BUILD=0
for arg in "$@"; do
  case "$arg" in
    --skip-build) SKIP_BUILD=1 ;;
    *)
      echo "unknown arg: $arg" >&2
      exit 1
      ;;
  esac
done

IMAGE="${IMAGE:-onkernel/chromium-headless-test:latest}"
export E2E_CHROMIUM_HEADLESS_IMAGE="$IMAGE"

if [[ "$SKIP_BUILD" == "0" ]]; then
  (cd "$ROOT/images/chromium-headless" && IMAGE="$IMAGE" ./build-docker.sh)
fi

echo "Running chromium/configure permutation tests against image $E2E_CHROMIUM_HEADLESS_IMAGE ..."
(cd "$ROOT/server" &&
  go test ./e2e -count=1 -timeout 120m -v \
    -run 'TestChromiumConfigure(StartURLBare|MultipartPowerset|StartURLJSONObject)$')
