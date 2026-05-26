#!/bin/bash

set -o errexit -o nounset -o pipefail

mkdir -p /tmp/pulse /tmp/runtime-kernel /home/kernel/.config/pulse
chown -R kernel:kernel /tmp/pulse /tmp/runtime-kernel /home/kernel/.config/pulse
chmod 1777 /tmp/pulse
chmod 700 /tmp/runtime-kernel

exec runuser -u kernel -- env \
  -u DBUS_SESSION_BUS_ADDRESS \
  -u DBUS_SYSTEM_BUS_ADDRESS \
  HOME=/home/kernel \
  XDG_CONFIG_HOME=/home/kernel/.config \
  XDG_RUNTIME_DIR=/tmp/runtime-kernel \
  PULSE_SERVER=unix:/tmp/pulse/native \
  pulseaudio \
    -n \
    --daemonize=no \
    --log-target=stderr \
    --exit-idle-time=-1 \
    --load="module-native-protocol-unix socket=/tmp/pulse/native auth-anonymous=1" \
    --load="module-null-sink sink_name=KernelOutput rate=48000 channels=2 sink_properties=device.description=KernelOutput"
