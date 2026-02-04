#!/bin/bash

set -o pipefail -o errexit -o nounset

# If we are outside Docker-in-Docker make sure /dev/shm exists
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
if h=$(cat /proc/sys/kernel/hostname 2>/dev/null); then
  if [ -z "$h" ] || [ "$h" = "(none)" ]; then
    if command -v hostname >/dev/null 2>&1; then
      hostname kernel-vm 2>/dev/null || true
    fi
    echo -n "kernel-vm" > /proc/sys/kernel/hostname 2>/dev/null || true
  fi
fi
export HOSTNAME="${HOSTNAME:-kernel-vm}"

# if CHROMIUM_FLAGS is not set, default to the flags used in playwright_stealth
# NOTE: --disable-background-networking was intentionally removed because it prevents
# Chrome from fetching extensions via ExtensionInstallForcelist enterprise policy.
# Enterprise extensions require Chrome to make HTTP requests to fetch update.xml and .crx files.
if [ -z "${CHROMIUM_FLAGS:-}" ]; then
  CHROMIUM_FLAGS="--accept-lang=en-US,en \
    --allow-pre-commit-input \
    --blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4 \
    --crash-dumps-dir=/tmp/chromium-dumps \
    --disable-back-forward-cache \
    --disable-background-timer-throttling \
    --disable-backgrounding-occluded-windows \
    --disable-blink-features=AutomationControlled \
    --disable-breakpad \
    --disable-client-side-phishing-detection \
    --disable-component-extensions-with-background-pages \
    --disable-crash-reporter \
    --disable-crashpad \
    --disable-dev-shm-usage \
    --disable-features=AcceptCHFrame,AutoExpandDetailsElement,AvoidUnnecessaryBeforeUnloadCheckSync,CertificateTransparencyComponentUpdater,DeferRendererTasksAfterInput,DestroyProfileOnBrowserClose,DialMediaRouteProvider,ExtensionManifestV2Disabled,GlobalMediaControls,HttpsUpgrades,ImprovedCookieControls,LazyFrameLoading,LensOverlay,MediaRouter,PaintHolding,ThirdPartyStoragePartitioning,Translate \
    --disable-field-trial-config \
    --disable-gcm-registration \
    --disable-gpu \
    --disable-gpu-compositing \
    --disable-hang-monitor \
    --disable-ipc-flooding-protection \
    --disable-notifications \
    --disable-popup-blocking \
    --disable-prompt-on-repost \
    --disable-renderer-backgrounding \
    --disable-search-engine-choice-screen \
    --disable-software-rasterizer \
    --enable-use-zoom-for-dsf=false \
    --export-tagged-pdf \
    --force-color-profile=srgb \
    --hide-crash-restore-bubble \
    --hide-scrollbars \
    --metrics-recording-only \
    --mute-audio \
    --no-default-browser-check \
    --no-first-run \
    --no-sandbox \
    --no-service-autorun \
    --ozone-platform=headless \
    --password-store=basic \
    --unsafely-disable-devtools-self-xss-warnings \
    --use-angle=swiftshader \
    --use-gl=angle \
    --use-mock-keychain"
fi
export CHROMIUM_FLAGS

# -----------------------------------------------------------------------------
# House-keeping for the unprivileged "kernel" user ----------------------------
# When RUN_AS_ROOT is true, we skip ownership changes since we're running as root.
# -----------------------------------------------------------------------------
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

# Track background tailing processes for cleanup
tail_pids=()

# Start log aggregator early so we see supervisor and service logs as they appear
start_dynamic_log_aggregator

# Export common env used by services
export DISPLAY=:1
export HEIGHT=${HEIGHT:-1080}
export WIDTH=${WIDTH:-1920}
export INTERNAL_PORT="${INTERNAL_PORT:-9223}"
export CHROME_PORT="${CHROME_PORT:-9222}"

# Cleanup handler
cleanup () {
  echo "[wrapper] Cleaning up..."
  # Re-enable scale-to-zero if the script terminates early
  enable_scale_to_zero
  supervisorctl -c /etc/supervisor/supervisord.conf stop chromium || true
  supervisorctl -c /etc/supervisor/supervisord.conf stop xvfb || true
  supervisorctl -c /etc/supervisor/supervisord.conf stop dbus || true
  supervisorctl -c /etc/supervisor/supervisord.conf stop kernel-images-api || true
  # Stop log tailers
  if [[ -n "${tail_pids[*]:-}" ]]; then
    for tp in "${tail_pids[@]}"; do
      kill -TERM "$tp" 2>/dev/null || true
    done
  fi
}
trap cleanup TERM INT

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

echo "[wrapper] Starting system D-Bus daemon via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start dbus
for i in {1..50}; do
  if [ -S /run/dbus/system_bus_socket ]; then
    break
  fi
  sleep 0.2
done
export DBUS_SESSION_BUS_ADDRESS="unix:path=/run/dbus/system_bus_socket"

echo "[wrapper] Starting Xvfb via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start xvfb
for i in {1..50}; do
  if xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

echo "[wrapper] Starting Chromium via supervisord on internal port $INTERNAL_PORT"
supervisorctl -c /etc/supervisor/supervisord.conf start chromium
for i in {1..100}; do
  if (echo >/dev/tcp/127.0.0.1/"$INTERNAL_PORT") >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

echo "[wrapper] ✨ Starting kernel-images API via supervisord."
supervisorctl -c /etc/supervisor/supervisord.conf start kernel-images-api
API_PORT="${KERNEL_IMAGES_API_PORT:-10001}"
echo "[wrapper] Waiting for kernel-images API on 127.0.0.1:${API_PORT}..."
while ! (echo >/dev/tcp/127.0.0.1/"${API_PORT}") >/dev/null 2>&1; do
  sleep 0.5
done

echo "[wrapper] startup complete!"
# Re-enable scale-to-zero once startup has completed (when not under Docker)
if [[ -z "${WITHDOCKER:-}" ]]; then
  enable_scale_to_zero
fi
# Keep the container running while streaming logs
wait
