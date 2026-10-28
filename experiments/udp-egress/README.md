# Isolated browser UDP experiment

This opt-in headful image experiment keeps HTTPS on an HTTP CONNECT upstream and
forwards actual browser UDP through a SOCKS5 UDP association on the same upstream
binding. It does not change the normal images or implement a production egress
service. Use only a disposable, locally run container.

## Isolation

Chromium runs as the unprivileged `kernel` user in the `browser-udp` network
namespace. The desktop, live-view server, HTTP forwarder and UDP relay remain in
the container's original namespace.

The browser's veth is the only interface captured by the UDP DNAT rules. Each
explicit test destination has its own local listener, which wraps the unchanged
payload in a SOCKS5 UDP header and forwards it to that destination through the
upstream. Returned payloads pass through normal reverse NAT, without changing
STUN contents or browser candidates. Each flow holds its SOCKS control connection
open. Unlisted UDP destinations are denied.

This intentionally uses explicit per-test routes rather than general transparent
UDP interception. On some kernels, ancillary destination information contains the
post-DNAT address, not the original destination. General interception needs a
separately validated original-destination mechanism; this experiment does not
assume that mechanism works.

An independent FORWARD deny rule is installed before the browser gets its default
route. If the UDP listener stops, packets reach a closed local port. If the DNAT
rule disappears, IPv4 forwarding remains denied. The browser namespace has IPv6
disabled when the kernel supports it; this is not a dual-stack forwarding test.
This design does not require the netfilter owner match, a TUN device, or policy
routing. DevTools uses TCP loopback bridges; live-view media is not intercepted.

The HTTP forwarder adds upstream authentication and otherwise copies bytes. It is
not a reproduction of the host-side TLS MITM or request-mutation stack.

## Build and run

Build the base image from the repository root using the normal headful Dockerfile,
then build this experiment:

```sh
GITHUB_TOKEN="$(gh auth token)" DOCKER_BUILDKIT=1 \
  docker build --secret id=github_token,env=GITHUB_TOKEN \
  -t kernel-images:udp-base -f images/chromium-headful/Dockerfile .
docker build --build-arg BASE_IMAGE=kernel-images:udp-base \
  -t kernel-images:udp-experiment -f experiments/udp-egress/Dockerfile .
```

Create a root-owned `0600` upstream JSON file outside the checkout using
`upstream.example.json` as the schema. Supply an existing IPv4 ISP endpoint,
its actual HTTP and SOCKS ports, and the same credentials for both protocols.
Set `udp_targets` to the controlled STUN receivers' actual public IPv4 addresses
and ports, plus the public DNS resolver used by the browser namespace. DNS is
configured to use `8.8.8.8` in this experiment. The documentation addresses in the
example must be replaced. Configure the file before starting the container; do
not change it while running. Do not bake credentials into an image or commit them.

Run the container through the repository's local-service workflow. It needs
privilege for its _container-local_ namespace and netfilter setup. Mount the JSON
read-only at `/run/udp-egress/upstream.json`. Keep HTTP/API/CDP ports bound to
loopback, publish only the live-view media range needed by authorized viewers,
and configure the live-view NAT address for the local environment. Do not attach
the container to the host network.

For example, use this foreground command as the local service, substituting the
root-owned configuration path and the address authorized viewers can reach:

```sh
docker run --rm --privileged --tmpfs /dev/shm:size=2g \
  --name udp-egress-test \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:19222:9222 \
  -p 127.0.0.1:10001:10001 -p 59000-59100:59000-59100/udp \
  --mount "type=bind,source=$CONFIG,target=/run/udp-egress/upstream.json,readonly" \
  -e WITHDOCKER=false -e ENABLE_WEBRTC=true \
  -e NEKO_WEBRTC_EPR=59000-59100 -e NEKO_WEBRTC_NAT1TO1="$VIEWER_REACHABLE_IP" \
  -e CHROMIUM_FLAGS="--no-sandbox --no-zygote --disable-dev-shm-usage --disable-quic --proxy-server=http://192.0.2.1:18080 --webrtc-ip-handling-policy=disable_non_proxied_udp" \
  kernel-images:udp-experiment
```

The normal launcher still supplies the display and DevTools settings. To enable
the experiment, merge `--webrtc-ip-handling-policy=default` into `/chromium/flags`
and restart only the Chromium supervisor program. Keep the relay and deny rule
in place before enabling UDP. Do not relax the policy in a normal browser image.
The local API provides a merge-and-restart operation:

```sh
curl --fail -X PATCH http://127.0.0.1:10001/chromium/flags \
  -H 'Content-Type: application/json' \
  -d '{"flags":["--webrtc-ip-handling-policy=default"]}'
```

## Validation

Use HTTPS and STUN receivers you control, or equivalent independently verifiable
receiver-side observations. The HTTPS echo endpoint used by `probe.cjs` must
return JSON containing the observed source address as `ip`.

```sh
python3 -m unittest discover -s experiments/udp-egress -p 'test_*.py'
docker exec CONTAINER node /opt/udp-egress/probe.cjs \
  https://receiver.example.test/request stun:receiver.example.test:3478 baseline
```

Repeat with WebRTC UDP enabled. Require the HTTPS receiver source IP, the STUN
receiver's XOR-MAPPED-ADDRESS, and the browser's server-reflexive candidate to
agree. Correlate the receiver response transaction IDs in
`/var/log/supervisord/udp-relay` with outgoing requests. Candidates alone are not
proof of the forwarding path.

Capture the container's external interface while testing. Check that no direct
browser UDP reaches the STUN receivers; encapsulated packets should instead go
to the upstream relay. Test all of:

- fresh peer connections and SOCKS associations;
- a Chromium restart;
- `supervisorctl stop udp-relay`, with HTTPS and live view still working;
- relay restart and successful return traffic;
- deletion of only this experiment's DNAT rule, checking FORWARD deny counters;
- whole-container restart, re-enabling UDP only after the guard is installed;
- IPv6 behavior, distinguishing an IPv6-disabled kernel from an IPv6-capable test.

Verify live-view video frames advance and the service records a connected ICE
state. A healthy HTTP endpoint does not establish media health.

## Limits

The relay is intentionally IPv4-only, permits only the explicit public test
destinations, rejects SOCKS fragments, and caps itself at 32 flows with a
60-second idle timeout. Credentials and relay endpoints are static. This is not
a production-ready open proxy.
Receiver observations must establish provider parity before testing the browser.
Results for an ISP binding do not establish behavior for rotating residential
bindings, other providers, different kernels, or the deployed stealth browser.

No production credentials belong in captures. Detach the private exposure before
stopping the container, then remove the runtime JSON file. Do not leave an
enabled experiment running unattended.
