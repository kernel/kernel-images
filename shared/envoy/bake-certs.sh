#!/bin/bash
set -eux

# Generate the self-signed cert envoy presents on the localhost forward proxy,
# install it into the system CA trust store, and seed the NSS DBs for both the
# root and `kernel` users so chromium trusts it regardless of which user the
# wrapper runs as. Runs once at image build time so container startup pays
# zero cost — no openssl invocation, no certutil shell-outs.
#
# Safety of a shared (per-image-tag) cert across customer instances:
#   - The cert's only Subject Alternative Names are `DNS:localhost` and
#     `IP:127.0.0.1`. A TLS client only accepts it for connections to
#     localhost, so the cert (and its private key) are useless for MITMing
#     any traffic the cert holder doesn't already control.
#   - The cert is trusted only by this image's system CA store and chromium
#     NSS DB. It is not trusted by customer machines, the host, or anything
#     outside this container.
#   - The forward proxy listens on 127.0.0.1 inside a network-isolated
#     container. One customer's container has no path to another customer's
#     localhost. Even an attacker holding the private key would need code
#     execution inside a sibling container to use it, at which point they
#     have everything anyway.
#   - The cert never leaves the container — no customer SDK, no browser
#     extension, no host service ever sees it.
# Bottom line: this CA is an in-container trust anchor for a localhost-only
# TLS listener. Sharing the key across containers built from the same image
# does not widen the threat model.

mkdir -p /etc/envoy/certs
openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
  -keyout /etc/envoy/certs/proxy.key \
  -out /etc/envoy/certs/proxy.crt \
  -subj "/C=US/ST=CA/O=Kernel/CN=localhost" \
  -addext "subjectAltName = DNS:localhost,IP:127.0.0.1"

# System trust store — picked up by curl, openssl, Go's net/http, etc.
cp /etc/envoy/certs/proxy.crt /usr/local/share/ca-certificates/kernel-envoy-proxy.crt
cp /etc/envoy/certs/proxy.crt /kernel-envoy-proxy.crt

# Seed both NSS DBs so chromium trusts the cert under either user. The
# wrapper's RUN_AS_ROOT branch chooses which DB chromium reads from at
# runtime; seeding both at build time means we don't need to know yet.
mkdir -p /root/.pki/nssdb /home/kernel/.pki/nssdb
certutil -d /root/.pki/nssdb -N --empty-password 2>/dev/null || true
certutil -d /home/kernel/.pki/nssdb -N --empty-password 2>/dev/null || true
certutil -d /root/.pki/nssdb -A -t "C,," -n "Kernel Envoy Proxy" -i /etc/envoy/certs/proxy.crt
certutil -d /home/kernel/.pki/nssdb -A -t "C,," -n "Kernel Envoy Proxy" -i /etc/envoy/certs/proxy.crt

# Install any pre-baked CA certs (BrightData certs are downloaded into
# /etc/envoy/brightdata by install-proxy.sh in private images). Same
# identity-free trust-store work as the self-signed cert above — moving it
# here means runtime sees an already-populated trust store.
if [ -d /etc/envoy/brightdata ]; then
  for cert in /etc/envoy/brightdata/*.crt; do
    [ -f "$cert" ] || continue
    cert_name=$(basename "$cert" .crt)
    cp "$cert" "/usr/local/share/ca-certificates/brightdata-${cert_name}.crt"
    certutil -d /root/.pki/nssdb -A -t "C,," -n "BrightData $cert_name" -i "$cert"
    certutil -d /home/kernel/.pki/nssdb -A -t "C,," -n "BrightData $cert_name" -i "$cert"
  done
fi

chown -R kernel:kernel /home/kernel/.pki

update-ca-certificates
