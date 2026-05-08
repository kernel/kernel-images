#!/bin/bash

set -o pipefail -o errexit -o nounset

# Runtime config for envoy. Cert generation and CA trust install ran at image
# build time (see shared/envoy/bake-certs.sh) so this script only does the
# identity-bound work: render the bootstrap template with the per-instance
# envs and start envoy via supervisord.

# Identity envs gate this script: without them xDS can't bind, so this is a
# no-op on images that don't run with a JWT.
INSTANCE_JWT="${KERNEL_INSTANCE_JWT:-}"
if [[ -z "${INST_NAME:-}" || -z "${METRO_NAME:-}" || -z "${XDS_SERVER:-}" || -z "${INSTANCE_JWT:-}" ]]; then
  echo "[envoy-init] Required environment variables not set. Skipping Envoy config/start."
  exit 0
fi

if [[ ! -f /etc/envoy/templates/bootstrap.yaml ]]; then
  echo "[envoy-init] Template file /etc/envoy/templates/bootstrap.yaml not found. Skipping Envoy config/start."
  exit 0
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
