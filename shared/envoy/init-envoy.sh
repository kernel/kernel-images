#!/bin/bash

set -o pipefail -o errexit -o nounset

# Check for required environment variables, to see if envoy is enabled
if [[ -z "${INST_NAME:-}" || -z "${METRO_NAME:-}" || -z "${XDS_SERVER:-}" || -z "${XDS_JWT:-}" ]]; then
  echo "[envoy-init] Required environment variables not set. Skipping Envoy initialization."
  exit 0
fi

# Also check for template file
if [[ ! -f /etc/envoy/templates/bootstrap.yaml ]]; then
  echo "[envoy-init] Template file /etc/envoy/templates/bootstrap.yaml not found. Skipping Envoy initialization."
  exit 0
fi

echo "[envoy-init] Preparing Envoy bootstrap configuration"
mkdir -p /etc/envoy

# Generate self-signed certificates for TLS forward proxy
echo "[envoy-init] Generating self-signed certificates for TLS forward proxy"
mkdir -p /etc/envoy/certs

if [[ ! -f /etc/envoy/certs/proxy.crt || ! -f /etc/envoy/certs/proxy.key ]]; then
  echo "[envoy-init] Creating new self-signed certificate"
  openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
    -keyout /etc/envoy/certs/proxy.key \
    -out /etc/envoy/certs/proxy.crt \
    -subj "/C=US/ST=CA/O=Kernel/CN=localhost" \
    -addext "subjectAltName = DNS:localhost,IP:127.0.0.1" \
    2>&1 | sed 's/^/[envoy-init] /'
  echo "[envoy-init] Certificate generated successfully"
  
  # Add certificate to system trust store for Chrome/Chromium
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
 echo "[envoy-init] Certificate added to nssdb"
else
  echo "[envoy-init] Certificates already exist, skipping generation"
fi

# Render template with provided environment variables
echo "[envoy-init] Rendering template with INST_NAME=${INST_NAME}, METRO_NAME=${METRO_NAME}, XDS_SERVER=${XDS_SERVER}, XDS_JWT=***"
inst_esc=$(printf '%s' "$INST_NAME" | sed -e 's/[\/&]/\\&/g')
metro_esc=$(printf '%s' "$METRO_NAME" | sed -e 's/[\/&]/\\&/g')
xds_esc=$(printf '%s' "$XDS_SERVER" | sed -e 's/[\/&]/\\&/g')
jwt_esc=$(printf '%s' "$XDS_JWT" | sed -e 's/[\/&]/\\&/g')
sed -e "s|{INST_NAME}|$inst_esc|g" \
    -e "s|{METRO_NAME}|$metro_esc|g" \
    -e "s|{XDS_SERVER}|$xds_esc|g" \
    -e "s|{XDS_JWT}|$jwt_esc|g" \
    /etc/envoy/templates/bootstrap.yaml > /etc/envoy/bootstrap.yaml

echo "[envoy-init] Starting Envoy via supervisord"
supervisorctl -c /etc/supervisor/supervisord.conf start envoy

# Wait for Envoy port to be open
echo "[envoy-init] Waiting for Envoy port to open..."
port_open=false
for i in {1..50}; do
  if nc -z 127.0.0.1 "3128" 2>/dev/null; then
    echo "[envoy-init] Envoy port confirmed open"
    port_open=true
    break
  fi
  sleep 0.2
done

if [[ "$port_open" != "true" ]]; then
  echo "[envoy-init] ERROR: Envoy port 3128 failed to open after 10 seconds"
  exit 1
fi

# Test proxy functionality
echo "[envoy-init] Testing proxy functionality..."
proxy_working=false
for i in {1..50}; do
  if curl -s -f -x https://127.0.0.1:3128 --max-time 2 https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html >/dev/null 2>&1; then
    echo "[envoy-init] Confirmed a request is proxied"
    proxy_working=true
    break
  fi
  echo "[envoy-init] Check failed, trying again..."
  sleep 0.2
done

if [[ "$proxy_working" != "true" ]]; then
  echo "[envoy-init] ERROR: Envoy proxy test failed after 10 seconds"
  exit 1
fi
