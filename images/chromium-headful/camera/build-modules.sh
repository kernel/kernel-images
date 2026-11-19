#!/usr/bin/env bash
# Build the V4L2 core modules + the camera-persona v4l2loopback for a hypeman
# guest kernel release, without changing the kernel image itself.
#
# The guest kernel ships with CONFIG_MEDIA_SUPPORT off but CONFIG_MODULES=y
# and no module signing, so modules built from the same tree, config and
# compiler (gcc 12.2, Debian bookworm) load into the stock kernel. MODVERSIONS
# is on, so the CRC check below fails the build if the core symbol CRCs drift.
#
# usage: build-modules.sh <kernel/linux tag> <outdir>
#   e.g. build-modules.sh ch-6.12.8-kernel-3.3-202611131 ./out
#
# The outdir gets the .ko files v4l2loopback needs (its modinfo dependency
# closure) plus a `load-order` file listing them in insmod order. The tag
# must match the guest kernel the image will run on (hypeman's
# DefaultKernelVersion) or insmod rejects the modules.
set -euo pipefail

TAG=${1:?kernel/linux tag}
OUT=$(realpath -m "${2:?output dir}")
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
V4L2LOOPBACK_COMMIT=442d720262e150084e7923068fb3c01acc42a253
PATCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/v4l2loopback-camera-realism.patch"

mkdir -p "$OUT"

git clone -q --depth=1 -b "$TAG" https://github.com/kernel/linux.git "$WORK/linux"
git clone -q https://github.com/umlaeute/v4l2loopback.git "$WORK/v4l2loopback"
git -C "$WORK/v4l2loopback" -c advice.detachedHead=false checkout -q "$V4L2LOOPBACK_COMMIT"
git -C "$WORK/v4l2loopback" apply "$PATCH"
mkdir -p "$WORK/hdr"
curl -fsSL "https://github.com/kernel/linux/releases/download/$TAG/kernel-headers-x86_64.tar.gz" | tar xz -C "$WORK/hdr"

cd "$WORK/linux"
cp "$WORK/hdr/.config" .config
cat >> .config <<'EOF'
CONFIG_MEDIA_SUPPORT=m
CONFIG_MEDIA_SUPPORT_FILTER=y
CONFIG_MEDIA_CAMERA_SUPPORT=y
CONFIG_VIDEO_DEV=m
EOF
make -s olddefconfig
make -s -j"$(nproc)" modules

# Every symbol exported by the stock kernel must have the same CRC in this build.
awk 'NR==FNR { stock[$2] = $1; next } ($2 in stock) && stock[$2] != $1 { bad++; print "crc mismatch:", $2 } END { exit bad > 0 }' \
  "$WORK/hdr/Module.symvers" Module.symvers

make -s -C "$WORK/v4l2loopback" KERNEL_DIR="$WORK/linux" v4l2loopback.ko
cp "$WORK/v4l2loopback/v4l2loopback.ko" "$OUT/"

# Copy v4l2loopback's transitive module dependency closure, in insmod order
# (dependencies first): videodev.ko itself depends on mc.ko, and so on.
cp_ko() { # <module-name>
  local ko
  ko=$(find drivers/media -name "$1.ko" | head -1)
  [ -n "$ko" ] || { echo "dependency $1.ko not found in build" >&2; exit 1; }
  cp "$ko" "$OUT/"
}
deps_of() { # <ko-file> -> space-separated dependency names
  modinfo "$1" | awk -F: '/^depends:/ { gsub(/,/, " ", $2); print $2 }'
}
: >"$OUT/load-order"
visit() { # <module-name>: DFS post-order so every dependency lands first
  local name=$1 dep
  case " $done_deps " in *" $name "*) return ;; esac
  done_deps="$done_deps $name"
  cp_ko "$name"
  for dep in $(deps_of "$OUT/$name.ko"); do
    visit "$dep"
  done
  echo "$name.ko" >>"$OUT/load-order"
}
done_deps=""
for name in $(deps_of "$OUT/v4l2loopback.ko"); do
  visit "$name"
done
echo "v4l2loopback.ko" >>"$OUT/load-order"

echo "modules for $(make -s kernelrelease) in $OUT:"
ls -la "$OUT"
