#!/usr/bin/env bash
# Build V4L2 core + v4l2loopback as loadable modules for an existing hypeman
# guest kernel release, without changing the kernel image itself.
#
# The guest kernel ships with CONFIG_MEDIA_SUPPORT off but CONFIG_MODULES=y and
# no module signing, so modules built from the same tree, config and compiler
# (gcc 12.2, Debian bookworm) load into the stock kernel. MODVERSIONS is on, so
# check_crcs below fails the build if the core symbol CRCs drift.
#
# usage: build-modules.sh <kernel/linux tag> <outdir>
#   e.g. build-modules.sh ch-6.12.8-kernel-3.2-202610301 ./out
set -euo pipefail

TAG=${1:?kernel/linux tag}
OUT=$(realpath -m "${2:?output dir}")
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

git clone -q --depth=1 -b "$TAG" https://github.com/kernel/linux.git "$WORK/linux"
git clone -q --depth=1 https://github.com/umlaeute/v4l2loopback.git "$WORK/v4l2loopback"
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

make -s -C "$WORK/v4l2loopback" KERNEL_DIR="$WORK/linux" v4l2loopback.ko utils
cp drivers/media/mc/mc.ko drivers/media/v4l2-core/videodev.ko \
  "$WORK/v4l2loopback/v4l2loopback.ko" "$WORK/v4l2loopback/utils/v4l2loopback-ctl" "$OUT/"
echo "modules for $(make -s kernelrelease) in $OUT:"
ls -la "$OUT"
cat <<'EOF'
load with:
  insmod mc.ko && insmod videodev.ko && \
  insmod v4l2loopback.ko devices=1 video_nr=0 card_label="Integrated Camera" exclusive_caps=1
EOF
