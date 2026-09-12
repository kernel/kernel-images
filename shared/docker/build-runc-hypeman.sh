#!/usr/bin/env bash
set -euo pipefail

RUNC_VERSION=1.3.4
RUNC_ARCHIVE_SHA256=a9f9646c4c8990239f6462b408b22d9aa40ba0473a9fc642b9d6576126495eee
PATCH_PATH=${RUNC_HYPEMAN_PATCH:-"$(dirname "$0")/runc-hypeman.patch"}
OUTPUT_PATH=${1:-/out/runc-hypeman}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --retry 3 --connect-timeout 15 \
  -o "$tmp/runc.tar.gz" \
  "https://github.com/opencontainers/runc/archive/refs/tags/v${RUNC_VERSION}.tar.gz"
echo "$RUNC_ARCHIVE_SHA256  $tmp/runc.tar.gz" | sha256sum -c -
mkdir "$tmp/source"
tar -xzf "$tmp/runc.tar.gz" --strip-components=1 -C "$tmp/source"
patch -d "$tmp/source" -p1 < "$PATCH_PATH"
make -C "$tmp/source" static
strip --strip-unneeded "$tmp/source/runc"
install -D -m 0755 "$tmp/source/runc" "$OUTPUT_PATH"
"$OUTPUT_PATH" --version
