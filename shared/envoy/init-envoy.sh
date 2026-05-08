#!/bin/bash

set -o pipefail -o errexit -o nounset

# Phase argument lets the Go wrapper split the script into an identity-free
# stage (certs/CA trust/NSS DB — runs early so chromium boots with the cert
# already trusted) and an identity-bound stage (template render with
# INST_NAME/METRO_NAME/XDS_SERVER/KERNEL_INSTANCE_JWT, then envoy start).
#   certs   — generate self-signed cert and install it in trust stores
#   config  — render bootstrap template and start envoy via supervisord
#   all     — both phases (default; preserves legacy single-call behavior)
PHASE="${1:-all}"

case "$PHASE" in
  certs|config|all) ;;
  *)
    echo "[envoy-init] Unknown phase: $PHASE (expected certs|config|all)" >&2
    exit 2
    ;;
esac

run_certs() {
  if [[ ! -f /etc/envoy/templates/bootstrap.yaml ]]; then
    echo "[envoy-init] Template file /etc/envoy/templates/bootstrap.yaml not found. Skipping cert generation."
    return 0
  fi

  echo "[envoy-init] Generating self-signed certificates for TLS forward proxy"
  mkdir -p /etc/envoy/certs

  if [[ -f /etc/envoy/certs/proxy.crt && -f /etc/envoy/certs/proxy.key ]]; then
    echo "[envoy-init] Certificates already exist, skipping generation"
    return 0
  fi

  echo "[envoy-init] Creating new self-signed certificate"
  openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
    -keyout /etc/envoy/certs/proxy.key \
    -out /etc/envoy/certs/proxy.crt \
    -subj "/C=US/ST=CA/O=Kernel/CN=localhost" \
    -addext "subjectAltName = DNS:localhost,IP:127.0.0.1" \
    2>&1 | sed 's/^/[envoy-init] /'
  echo "[envoy-init] Certificate generated successfully"

  echo "[envoy-init] Adding certificate to system trust store"
  cp /etc/envoy/certs/proxy.crt /usr/local/share/ca-certificates/kernel-envoy-proxy.crt
  cp /etc/envoy/certs/proxy.crt /kernel-envoy-proxy.crt
  update-ca-certificates 2>&1 | sed 's/^/[envoy-init] /'
  echo "[envoy-init] Certificate added to system trust store"

  if [[ "${RUN_AS_ROOT:-}" == "true" ]]; then
    mkdir -p /root/.pki/nssdb
    certutil -d /root/.pki/nssdb -N --empty-password 2>/dev/null || true
    certutil -d /root/.pki/nssdb -A -t "C,," -n "Kernel Envoy Proxy" -i /etc/envoy/certs/proxy.crt
    echo "[envoy-init] Certificate added to nssdb as root"
  else
    mkdir -p /home/kernel/.pki/nssdb
    certutil -d /home/kernel/.pki/nssdb -N --empty-password 2>/dev/null || true
    certutil -d /home/kernel/.pki/nssdb -A -t "C,," -n "Kernel Envoy Proxy" -i /etc/envoy/certs/proxy.crt
    chown -R kernel:kernel /home/kernel/.pki
    echo "[envoy-init] Certificate added to nssdb as kernel"
  fi
}

run_config() {
  # Identity envs gate the config phase: without them xDS can't bind, so
  # render+start is a no-op on images that don't run with a JWT.
  INSTANCE_JWT="${KERNEL_INSTANCE_JWT:-}"
  if [[ -z "${INST_NAME:-}" || -z "${METRO_NAME:-}" || -z "${XDS_SERVER:-}" || -z "${INSTANCE_JWT:-}" ]]; then
    echo "[envoy-init] Required environment variables not set. Skipping Envoy config/start."
    return 0
  fi

  if [[ ! -f /etc/envoy/templates/bootstrap.yaml ]]; then
    echo "[envoy-init] Template file /etc/envoy/templates/bootstrap.yaml not found. Skipping Envoy config/start."
    return 0
  fi

  echo "[envoy-init] Preparing Envoy bootstrap configuration"
  mkdir -p /etc/envoy

  echo "[envoy-init] Rendering template with INST_NAME=${INST_NAME}, METRO_NAME=${METRO_NAME}, XDS_SERVER=${XDS_SERVER}, KERNEL_INSTANCE_JWT=***"
  inst_esc=$(printf '%s' "$INST_NAME" | sed -e 's/[\/&]/\\&/g')
  metro_esc=$(printf '%s' "$METRO_NAME" | sed -e 's/[\/&]/\\&/g')
  xds_esc=$(printf '%s' "$XDS_SERVER" | sed -e 's/[\/&]/\\&/g')
  jwt_esc=$(printf '%s' "$INSTANCE_JWT" | sed -e 's/[\/&]/\\&/g')
  sed -e "s|{INST_NAME}|$inst_esc|g" \
      -e "s|{METRO_NAME}|$metro_esc|g" \
      -e "s|{XDS_SERVER}|$xds_esc|g" \
      -e "s|{KERNEL_INSTANCE_JWT}|$jwt_esc|g" \
      /etc/envoy/templates/bootstrap.yaml > /etc/envoy/bootstrap.yaml

  echo "[envoy-init] Starting Envoy via supervisord"
  # `restart` is start-or-stop+start: on first boot this just starts envoy,
  # on a re-render (e.g. post-fork env refresh) it forces a clean re-read
  # of the rendered bootstrap. Either way no callers see stale identity.
  supervisorctl -c /etc/supervisor/supervisord.conf restart envoy

  # Readiness (port 3128 reachable) is probed by the Go wrapper's
  # waitAllReady alongside CDP/chromedriver, so this script returns as soon
  # as the start request has been issued.
}

case "$PHASE" in
  certs)
    run_certs
    ;;
  config)
    run_config
    ;;
  all)
    run_certs
    run_config
    ;;
esac
