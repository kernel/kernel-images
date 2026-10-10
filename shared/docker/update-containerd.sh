#!/bin/sh
set -eux

VERSION=2.4.1
ARCH="$1"
case "$ARCH" in
    amd64) SHA256=02ad3a7e80d7d2c018c7134d5eca7e301283db6895f100b957fb52ad8e5a30fd ;;
    arm64) SHA256=d3aba9347650505df79858bb9cbf1be52080fd8145f72dd9308d4936f487fb74 ;;
    *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

ARCHIVE="containerd-static-${VERSION}-linux-${ARCH}.tar.gz"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT
cd "$TMPDIR"

# Docker's bundled containerd still embeds an older gRPC release.
wget -O "$ARCHIVE" "https://github.com/containerd/containerd/releases/download/v${VERSION}/${ARCHIVE}"
echo "$SHA256  $ARCHIVE" | sha256sum -c -
tar -xzf "$ARCHIVE" -C /usr/local bin/containerd bin/containerd-shim-runc-v2
