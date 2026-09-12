#!/usr/bin/env bash
set -euo pipefail

KERNEL_IMAGES_REF=${1:-}
if [[ ! "$KERNEL_IMAGES_REF" =~ ^[0-9a-f]{40}$ ]]; then
  echo "usage: $0 <kernel-images-commit-sha>" >&2
  exit 2
fi
if [[ $(uname -m) != x86_64 ]]; then
  echo "the dynamic Hypeman Docker runtime installer requires x86_64" >&2
  exit 1
fi
if [[ $(id -u) -ne 0 ]]; then
  echo "the dynamic Hypeman Docker runtime installer must run as root" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get -o DPkg::Lock::Timeout=300 update
apt-get -o DPkg::Lock::Timeout=300 --no-install-recommends -y install \
  build-essential \
  ca-certificates \
  curl \
  fuse-overlayfs \
  iptables \
  libseccomp-dev \
  patch \
  pkg-config
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

if ! command -v runc-hypeman >/dev/null 2>&1; then
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT

  readonly raw_base="https://raw.githubusercontent.com/kernel/kernel-images/${KERNEL_IMAGES_REF}/shared/docker"
  curl -fsSL --retry 3 --connect-timeout 15 \
    -o "$tmp/build-runc-hypeman.sh" "$raw_base/build-runc-hypeman.sh"
  curl -fsSL --retry 3 --connect-timeout 15 \
    -o "$tmp/runc-hypeman.patch" "$raw_base/runc-hypeman.patch"
  chmod 0755 "$tmp/build-runc-hypeman.sh"

  readonly go_archive_sha256=2852af0cb20a13139b3448992e69b868e50ed0f8a1e5940ee1de9e19a123b613
  curl -fsSL --retry 3 --connect-timeout 15 \
    -o "$tmp/go.tar.gz" https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
  echo "$go_archive_sha256  $tmp/go.tar.gz" | sha256sum -c -
  mkdir "$tmp/go"
  tar -xzf "$tmp/go.tar.gz" --strip-components=1 -C "$tmp/go"

  PATH="$tmp/go/bin:$PATH" \
    RUNC_HYPEMAN_PATCH="$tmp/runc-hypeman.patch" \
    "$tmp/build-runc-hypeman.sh" /usr/local/bin/runc-hypeman
fi

mkdir -p /etc/docker
cat >/etc/docker/daemon.json <<'JSON'
{
  "storage-driver": "fuse-overlayfs",
  "default-runtime": "hypeman-runc",
  "runtimes": {
    "hypeman-runc": {
      "path": "/usr/local/bin/runc-hypeman"
    }
  }
}
JSON

runc-hypeman --version
if command -v dockerd >/dev/null 2>&1; then
  dockerd --validate --config-file=/etc/docker/daemon.json
fi
