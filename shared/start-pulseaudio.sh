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
  bash -lc '
  set -o errexit -o nounset -o pipefail

  # KernelOutput is the playback sink the recorder captures from (via its
  # .monitor source). KernelInput is a standalone null-source so the browser
  # sees a real, non-monitor microphone: Chromium excludes monitor sources from
  # navigator.mediaDevices.enumerateDevices(), so without this there would be
  # zero audioinput devices and antibot scripts could flag the missing mic.
  # module-null-source rejects source_properties in this PulseAudio version, so
  # it keeps the default description.
  pulseaudio \
    -n \
    --daemonize=no \
    --log-target=stderr \
    --exit-idle-time=-1 \
    --load="module-native-protocol-unix socket=/tmp/pulse/native auth-anonymous=1" \
    --load="module-null-sink sink_name=KernelOutput rate=48000 channels=2 sink_properties=device.description=KernelOutput" \
    --load="module-null-source source_name=KernelInput format=s16le rate=48000 channels=2" &

  pulse_pid=$!
  keepalive_pid=""

  cleanup() {
    if [ -n "$keepalive_pid" ]; then
      kill "$keepalive_pid" 2>/dev/null || true
    fi
    kill "$pulse_pid" 2>/dev/null || true
    wait 2>/dev/null || true
  }
  trap cleanup EXIT INT TERM

  for _ in $(seq 1 100); do
    if pactl list short sinks 2>/dev/null | grep -q "KernelOutput"; then
      break
    fi
    sleep 0.1
  done

  (
    pacat --raw --rate=48000 --channels=2 --format=s16le --device=KernelOutput /dev/zero
  ) &
  keepalive_pid=$!

  wait -n "$pulse_pid" "$keepalive_pid"
  '
