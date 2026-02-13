#!/bin/bash

set -o pipefail -o errexit -o nounset

# If the WITHDOCKER environment variable is not set, it means we are not running inside a Docker container.
# Docker manages /dev/shm itself, and attempting to mount or modify it can cause permission or device errors.
# However, in a unikernel container environment (non-Docker), we need to manually create and mount /dev/shm as a tmpfs
# to support shared memory operations.
if [ -z "${WITHDOCKER:-}" ]; then
  mkdir -p /dev/shm
  chmod 777 /dev/shm
  mount -t tmpfs tmpfs /dev/shm
fi

# We disable scale-to-zero for the lifetime of this script and restore
# the original setting on exit.
SCALE_TO_ZERO_FILE="/uk/libukp/scale_to_zero_disable"
scale_to_zero_write() {
  local char="$1"
  # Skip when not running inside Unikraft Cloud (control file absent)
  if [[ -e "$SCALE_TO_ZERO_FILE" ]]; then
    # Write the character, but do not fail the whole script if this errors out
    echo -n "$char" > "$SCALE_TO_ZERO_FILE" 2>/dev/null || \
      echo "[wrapper] Failed to write to scale-to-zero control file" >&2
  fi
}
disable_scale_to_zero() { scale_to_zero_write "+"; }
enable_scale_to_zero()  { scale_to_zero_write "-"; }

# Disable scale-to-zero for the duration of the script when not running under Docker
if [[ -z "${WITHDOCKER:-}" ]]; then
  echo "[wrapper] Disabling scale-to-zero"
  disable_scale_to_zero
fi

# -----------------------------------------------------------------------------
# Ensure a sensible hostname ---------------------------------------------------
# -----------------------------------------------------------------------------
# Some environments boot with an empty or \"(none)\" hostname which shows up in
# prompts. Best-effort set a friendly hostname early so services inherit it.
if h=$(cat /proc/sys/kernel/hostname 2>/dev/null); then
  if [ -z "$h" ] || [ "$h" = "(none)" ]; then
    if command -v hostname >/dev/null 2>&1; then
      hostname kernel-vm 2>/dev/null || true
    fi
    echo -n "kernel-vm" > /proc/sys/kernel/hostname 2>/dev/null || true
  fi
fi
# Also export HOSTNAME so shells pick it up immediately.
export HOSTNAME="${HOSTNAME:-kernel-vm}"

# -----------------------------------------------------------------------------
# House-keeping for the unprivileged "kernel" user --------------------------------
# Some Chromium subsystems want to create files under $HOME (NSS cert DB, dconf
# cache).  If those directories are missing or owned by root Chromium emits
# noisy error messages such as:
#   [ERROR:crypto/nss_util.cc:48] Failed to create /home/kernel/.pki/nssdb ...
#   dconf-CRITICAL **: unable to create directory '/home/kernel/.cache/dconf'
# Pre-create them and hand ownership to the user so the messages disappear.
# When RUN_AS_ROOT is true, we skip ownership changes since we're running as root.

if [[ "${RUN_AS_ROOT:-}" != "true" ]]; then
  dirs=(
    /home/kernel/user-data
    /home/kernel/.config/chromium
    /home/kernel/.pki/nssdb
    /home/kernel/.cache/dconf
    /tmp
    /var/log
    /var/log/supervisord
  )

  for dir in "${dirs[@]}"; do
    if [ ! -d "$dir" ]; then
      mkdir -p "$dir"
    fi
  done

  # Ensure correct ownership (ignore errors if already correct)
  chown -R kernel:kernel /home/kernel /home/kernel/user-data /home/kernel/.config /home/kernel/.pki /home/kernel/.cache 2>/dev/null || true
  # Make policy directory writable for runtime updates
  chown -R kernel:kernel /etc/chromium/policies 2>/dev/null || true
else
  # When running as root, just create the necessary directories without ownership changes
  dirs=(
    /tmp
    /var/log
    /var/log/supervisord
    /home/kernel
    /home/kernel/user-data
  )

  for dir in "${dirs[@]}"; do
    if [ ! -d "$dir" ]; then
      mkdir -p "$dir"
    fi
  done
fi

# -----------------------------------------------------------------------------
# Dynamic log aggregation for /var/log/supervisord -----------------------------
# -----------------------------------------------------------------------------
# Tails any existing and future files under /var/log/supervisord,
# prefixing each line with the relative filepath, e.g. [chromium] ...
start_dynamic_log_aggregator() {
  echo "[wrapper] Starting dynamic log aggregator for /var/log/supervisord"
  (
    declare -A tailed_files=()
    start_tail() {
      local f="$1"
      [[ -f "$f" ]] || return 0
      [[ -n "${tailed_files[$f]:-}" ]] && return 0
      local label="${f#/var/log/supervisord/}"
      # Tie tails to this subshell lifetime so they exit when we stop it
      tail --pid="$$" -n +1 -F "$f" 2>/dev/null | sed -u "s/^/[${label}] /" &
      tailed_files[$f]=1
    }
    # Periodically scan for new *.log files without extra dependencies
    while true; do
      while IFS= read -r -d '' f; do
        start_tail "$f"
      done < <(find /var/log/supervisord -type f -print0 2>/dev/null || true)
      sleep 1
    done
  ) &
  tail_pids+=("$!")
}

# Start log aggregator early so we see supervisor and service logs as they appear
start_dynamic_log_aggregator

export DISPLAY=:1

# Optionally pre-trust a custom CA in Chromium's NSS DB before Chromium starts.
# This is useful for HTTPS MITM demos without --ignore-certificate-errors.
import_chromium_nss_ca() {
  local cert_path="${PAC_MITM_CA_CERT_PATH:-}"
  local cert_name="${PAC_MITM_CA_CERT_NICKNAME:-pac-mitm}"
  if [[ -z "$cert_path" || ! -f "$cert_path" ]]; then
    return 0
  fi

  echo "[wrapper] Importing custom Chromium NSS CA from $cert_path"
  runuser -u kernel -- sh -lc "
    set -e
    mkdir -p \"\$HOME/.pki/nssdb\"
    if [ ! -f \"\$HOME/.pki/nssdb/cert9.db\" ]; then
      certutil -N -d sql:\"\$HOME/.pki/nssdb\" --empty-password
    fi
    certutil -D -d sql:\"\$HOME/.pki/nssdb\" -n '$cert_name' >/dev/null 2>&1 || true
    certutil -A -d sql:\"\$HOME/.pki/nssdb\" -n '$cert_name' -t 'C,,' -i '$cert_path'
  "
}

# Predefine ports and export for services
export INTERNAL_PORT="${INTERNAL_PORT:-9223}"
export CHROME_PORT="${CHROME_PORT:-9222}"

# Track background tailing processes for cleanup
tail_pids=()

# Cleanup handler (set early so we catch early failures)
cleanup () {
  echo "[wrapper] Cleaning up..."
  # Re-enable scale-to-zero if the script terminates early
  enable_scale_to_zero
  supervisorctl -c /etc/supervisor/supervisord.conf stop chromium || true
  supervisorctl -c /etc/supervisor/supervisord.conf stop pac-proxy || true
  supervisorctl -c /etc/supervisor/supervisord.conf stop kernel-images-api || true
  supervisorctl -c /etc/supervisor/supervisord.conf stop dbus || true
  # Stop log tailers
  if [[ -n "${tail_pids[*]:-}" ]]; then
    for tp in "${tail_pids[@]}"; do
      kill -TERM "$tp" 2>/dev/null || true
    done
  fi
}
trap cleanup TERM INT

# Start supervisord early so it can manage Xorg and Mutter
echo "[wrapper] Starting supervisord"
supervisord -c /etc/supervisor/supervisord.conf
echo "[wrapper] Waiting for supervisord socket..."
for i in {1..30}; do
if [ -S /var/run/supervisor.sock ]; then
    break
fi
sleep 0.2
done

init-envoy.sh

echo "[wrapper] Starting Xorg via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start xorg
echo "[wrapper] Waiting for Xorg to open display $DISPLAY..."
for i in {1..50}; do
  if xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

echo "[wrapper] Starting Mutter via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start mutter
echo "[wrapper] Waiting for Mutter to be ready..."
timeout=30
while [ $timeout -gt 0 ]; do
  if xdotool search --class "mutter" >/dev/null 2>&1; then
    break
  fi
  sleep 1
  ((timeout--))
done

# -----------------------------------------------------------------------------
# System-bus setup via supervisord --------------------------------------------
# -----------------------------------------------------------------------------
echo "[wrapper] Starting system D-Bus daemon via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start dbus
echo "[wrapper] Waiting for D-Bus system bus socket..."
for i in {1..50}; do
  if [ -S /run/dbus/system_bus_socket ]; then
    break
  fi
  sleep 0.2
done

# We will point DBUS_SESSION_BUS_ADDRESS at the system bus socket to suppress
# autolaunch attempts that failed and spammed logs.
export DBUS_SESSION_BUS_ADDRESS="unix:path=/run/dbus/system_bus_socket"

echo "[wrapper] Starting PAC transparent proxy via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start pac-proxy
echo "[wrapper] Waiting for PAC transparent proxy on 127.0.0.1:15080..."
for i in {1..50}; do
  if nc -z 127.0.0.1 15080 2>/dev/null; then
    break
  fi
  sleep 0.2
done

# Optional CA trust import happens before Chromium launch.
import_chromium_nss_ca

# Start Chromium with display :1 and remote debugging, loading our recorder extension.
echo "[wrapper] Starting Chromium via supervisord on internal port $INTERNAL_PORT"
supervisorctl -c /etc/supervisor/supervisord.conf start chromium
echo "[wrapper] Waiting for Chromium remote debugging on 127.0.0.1:$INTERNAL_PORT..."
for i in {1..100}; do
  if nc -z 127.0.0.1 "$INTERNAL_PORT" 2>/dev/null; then
    break
  fi
  sleep 0.2
done

if [[ "${ENABLE_WEBRTC:-}" == "true" ]]; then
  # use webrtc
  echo "[wrapper] ✨ Starting neko (webrtc server) via supervisord."
  supervisorctl -c /etc/supervisor/supervisord.conf start neko

  # Wait for neko to be ready.
  echo "[wrapper] Waiting for neko port 0.0.0.0:8080..."
  while ! nc -z 127.0.0.1 8080 2>/dev/null; do
    sleep 0.5
  done
  echo "[wrapper] Port 8080 is open"
fi

echo "[wrapper] ✨ Starting kernel-images API."

API_PORT="${KERNEL_IMAGES_API_PORT:-10001}"
API_FRAME_RATE="${KERNEL_IMAGES_API_FRAME_RATE:-10}"
API_DISPLAY_NUM="${KERNEL_IMAGES_API_DISPLAY_NUM:-${DISPLAY_NUM:-1}}"
API_MAX_SIZE_MB="${KERNEL_IMAGES_API_MAX_SIZE_MB:-500}"
API_OUTPUT_DIR="${KERNEL_IMAGES_API_OUTPUT_DIR:-/recordings}"

# Start via supervisord (env overrides are read by the service's command)
supervisorctl -c /etc/supervisor/supervisord.conf start kernel-images-api

echo "[wrapper] Starting PulseAudio daemon via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start pulseaudio

# close the "--no-sandbox unsupported flag" warning when running as root
# in the unikernel runtime we haven't been able to get chromium to launch as non-root without cryptic crashpad errors
# and when running as root you must use the --no-sandbox flag, which generates a warning
if [[ "${RUN_AS_ROOT:-}" == "true" ]]; then
  echo "[wrapper] Running as root, attempting to dismiss the --no-sandbox unsupported flag warning"
  if read -r WIDTH HEIGHT <<< "$(xdotool getdisplaygeometry 2>/dev/null)"; then
    # Work out an x-coordinate slightly inside the right-hand edge of the
    OFFSET_X=$(( WIDTH - 30 ))
    if (( OFFSET_X < 0 )); then
      OFFSET_X=0
    fi

    # Wait for kernel-images API port to be ready.
    echo "[wrapper] Waiting for kernel-images API port 127.0.0.1:${API_PORT}..."
    while ! nc -z 127.0.0.1 "${API_PORT}" 2>/dev/null; do
      sleep 0.5
    done
    echo "[wrapper] Port ${API_PORT} is open"

    # Wait for Chromium window to open before dismissing the --no-sandbox warning.
    target='New Tab - Chromium'
    echo "[wrapper] Waiting for Chromium window \"${target}\" to appear and become active..."
    while :; do
      win_id=$(xwininfo -root -tree 2>/dev/null | awk -v t="$target" '$0 ~ t {print $1; exit}')
      if [[ -n $win_id ]]; then
        win_id=${win_id%:}
        if xdotool windowactivate --sync "$win_id"; then
          echo "[wrapper] Focused window $win_id ($target) on $DISPLAY"
          break
        fi
      fi
      sleep 0.5
    done

    # wait... not sure but this just increases the likelihood of success
    # without the sleep you often open the live view and see the mouse hovering over the "X" to dismiss the warning, suggesting that it clicked before the warning or chromium appeared
    sleep 5

    # Attempt to click the warning's close button
    echo "[wrapper] Clicking the warning's close button at x=$OFFSET_X y=115"
    if curl -s -o /dev/null -X POST \
      http://localhost:${API_PORT}/computer/click_mouse \
      -H "Content-Type: application/json" \
      -d "{\"x\":${OFFSET_X},\"y\":115}"; then
        echo "[wrapper] Successfully clicked the warning's close button"
    else
      echo "[wrapper] Failed to click the warning's close button" >&2
    fi
  else
    echo "[wrapper] xdotool failed to obtain display geometry; skipping sandbox warning dismissal." >&2
  fi
fi

if [[ -z "${WITHDOCKER:-}" ]]; then
  enable_scale_to_zero
fi

# Keep the container running while streaming logs
wait
