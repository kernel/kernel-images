#!/bin/bash

set -o errexit -o nounset -o pipefail

# This script is the authority for the audio topology. Consumers
# (kernel-images-api recorder, chromium via chromium-launcher) hardcode matching
# defaults that must stay in sync with the names defined here.
#   PULSE_SOCKET : the unix socket clients connect to (PULSE_SERVER=unix:<path>)
#   KERNEL_SINK  : playback sink the recorder captures from (via <sink>.monitor)
#   KERNEL_SOURCE: standalone null-source exposed as a real microphone
PULSE_SOCKET=/tmp/pulse/native
KERNEL_SINK=KernelOutput
KERNEL_SOURCE=KernelInput

mkdir -p /tmp/pulse /tmp/runtime-kernel /home/kernel/.config/pulse
chown -R kernel:kernel /tmp/pulse /tmp/runtime-kernel /home/kernel/.config/pulse
chmod 1777 /tmp/pulse
chmod 700 /tmp/runtime-kernel

# Remove any stale socket from a previous unclean exit (autorestart=true). Otherwise
# module-native-protocol-unix may fail to bind, and the container wrapper's socket
# wait could pass on the dead socket and start chromium before the daemon is back.
rm -f "$PULSE_SOCKET"

# Constants are passed into the inner (single-quoted) script via env so the
# topology is defined once at the top of this file.
exec runuser -u kernel -- env \
  -u DBUS_SESSION_BUS_ADDRESS \
  -u DBUS_SYSTEM_BUS_ADDRESS \
  HOME=/home/kernel \
  XDG_CONFIG_HOME=/home/kernel/.config \
  XDG_RUNTIME_DIR=/tmp/runtime-kernel \
  PULSE_SERVER="unix:$PULSE_SOCKET" \
  PULSE_SOCKET="$PULSE_SOCKET" \
  KERNEL_SINK="$KERNEL_SINK" \
  KERNEL_SOURCE="$KERNEL_SOURCE" \
  bash -lc '
  set -o errexit -o nounset -o pipefail

  # KERNEL_SINK is the playback sink the recorder captures from (via its
  # .monitor source). KERNEL_SOURCE is a standalone null-source so the browser
  # sees a real, non-monitor microphone: Chromium excludes monitor sources from
  # navigator.mediaDevices.enumerateDevices(), so without this there would be
  # zero audioinput devices and antibot scripts could flag the missing mic.
  # module-null-source rejects source_properties in this PulseAudio version, so
  # it keeps the default description.
  # Modules load in order, and the unix socket only starts accepting connections
  # once its module loads. Load the sink and source first so that by the time the
  # socket exists (which is all the container wrapper waits for before starting
  # chromium), KernelOutput is already registered and ready to route playback.
  pulseaudio \
    -n \
    --daemonize=no \
    --log-target=stderr \
    --exit-idle-time=-1 \
    --load="module-null-sink sink_name=$KERNEL_SINK rate=48000 channels=2 sink_properties=device.description=$KERNEL_SINK" \
    --load="module-null-source source_name=$KERNEL_SOURCE format=s16le rate=48000 channels=2" \
    --load="module-native-protocol-unix socket=$PULSE_SOCKET auth-anonymous=1" &

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
    if pactl list short sinks 2>/dev/null | grep -q "$KERNEL_SINK"; then
      break
    fi
    sleep 0.1
  done

  # Keep a silent stream open on the sink so its monitor always produces data for
  # the recorder. Self-heal if pacat dies: a keepalive hiccup should not tear down
  # the whole audio stack, so restart it as long as the daemon is alive and only
  # let the script exit when pulseaudio itself does.
  (
    while kill -0 "$pulse_pid" 2>/dev/null; do
      pacat --raw --rate=48000 --channels=2 --format=s16le --device="$KERNEL_SINK" /dev/zero || true
      sleep 0.5
    done
  ) &
  keepalive_pid=$!

  wait "$pulse_pid"
  '
