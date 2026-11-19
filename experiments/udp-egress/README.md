# Isolated browser UDP experiment

This opt-in headful image experiment sends a browser's HTTP(S) and UDP through
one egress-proxy process: HTTP(S) as an ordinary HTTP proxy (CONNECT, with
egress-proxy's MITM) and UDP as connect-udp
([RFC 9298](https://www.rfc-editor.org/rfc/rfc9298)) on the same listener. It
needs an egress-proxy built with connect-udp support and started with
`EGRESS_PROXY_UDP_ENABLED=true`. It does not change the normal images. Use only
a disposable, locally run container.

The container holds no proxy credentials. egress-proxy authorizes both kinds of
traffic by the container's source address, selects the session's upstream, and
relays UDP through that upstream's SOCKS5 UDP association.

## Isolation

Chromium runs as the unprivileged `kernel` user in the `browser-udp` network
namespace. The desktop, live-view server, DevTools bridge, TCP relay and UDP
shim stay in the container's original namespace.

Chromium's only proxy is `192.0.2.1:18080`, a plain TCP relay to egress-proxy.
The UDP DNAT rules capture only the browser's veth. Each explicit test
destination has its own local listener; the shim opens one connect-udp request
per browser flow and carries the unchanged payload as DATAGRAM capsules.
Returned payloads pass through normal reverse NAT, without changing STUN
contents or browser candidates. Unlisted UDP destinations are denied.

This intentionally uses explicit per-test routes rather than general transparent
UDP interception. On some kernels, ancillary destination information contains the
post-DNAT address, not the original destination. General interception needs a
separately validated original-destination mechanism; this experiment does not
assume that mechanism works.

An independent FORWARD deny rule is installed before the browser gets its default
route. If the shim stops, packets reach a closed local port. If the DNAT rule
disappears, forwarding remains denied. If egress-proxy refuses or drops a flow,
nothing else carries it. The browser namespace has IPv6 disabled when the kernel
supports it; this is not a dual-stack forwarding test. DevTools uses TCP loopback
bridges; live-view media is not intercepted.

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

Create a directory outside the checkout containing `egress.json`, using
`egress.example.json` as the schema, and egress-proxy's MITM CA certificate as
`mitm-ca.crt` (certificate only, never the key). `egress_host` must be the
egress-proxy address the container reaches; egress-proxy sees the container's
address as the source. Set `udp_targets` to the controlled STUN receivers'
public IPv4 addresses and ports, plus the public DNS resolver used by the
browser namespace (`8.8.8.8`). Configure it before starting the container.

egress-proxy must authorize the container's address as a claimed instance whose
route resolves to a Byteful ISP upstream. Give the container a fixed address on
a dedicated network so that mapping stays valid across restarts. Keep HTTP, API
and DevTools ports on loopback and publish only the live-view media range:

```sh
docker network create --subnet 172.30.0.0/24 udp-egress-net
docker run --rm --privileged --tmpfs /dev/shm:size=2g \
  --name udp-egress-test --network udp-egress-net --ip 172.30.0.10 \
  -p 127.0.0.1:18081:8080 -p 127.0.0.1:19222:9222 \
  -p 127.0.0.1:10001:10001 -p 59000-59100:59000-59100/udp \
  -v "$CONFIG_DIR:/run/udp-egress:ro" \
  -e WITHDOCKER=false -e ENABLE_WEBRTC=true \
  -e NEKO_WEBRTC_EPR=59000-59100 -e NEKO_WEBRTC_NAT1TO1="$VIEWER_REACHABLE_IP" \
  -e CHROMIUM_FLAGS="--no-sandbox --no-zygote --disable-dev-shm-usage --disable-quic --proxy-server=http://192.0.2.1:18080 --webrtc-ip-handling-policy=disable_non_proxied_udp" \
  kernel-images:udp-experiment
```

The normal launcher still supplies the display and DevTools settings. To enable
WebRTC UDP, set the policy in `/chromium/flags` and restart only the Chromium
supervisor program. Keep the shim and deny rule in place first, and do not relax
the policy in a normal browser image. `PATCH /chromium/flags` appends a second
policy rather than replacing the first, so write the file directly:

```sh
docker exec udp-egress-test sh -c \
  'mkdir -p /chromium &&
   printf "{\"flags\":[\"--webrtc-ip-handling-policy=default\"]}" > /chromium/flags &&
   supervisorctl restart chromium'
```

## Validation

Use HTTPS and STUN receivers you control, or equivalent independently verifiable
receiver-side observations. The HTTPS echo endpoint used by `probe.cjs` must
return JSON containing the observed source address as `ip`. Add a unique query
string so a cached response cannot stand in for a request.

```sh
python3 -m unittest discover -s experiments/udp-egress -p 'test_*.py'
docker exec udp-egress-test node /opt/udp-egress/probe.cjs \
  "https://receiver.example.test/request?nonce=$RANDOM" stun:203.0.113.20:3478 baseline
```

Repeat with WebRTC UDP enabled. Require the HTTPS receiver source IP, the STUN
receiver's XOR-MAPPED-ADDRESS, and the browser's server-reflexive candidate to
agree, and the HTTPS issuer to be the MITM CA. Correlate the receiver response
transaction IDs in `/var/log/supervisord/udp-relay` with outgoing requests.
Candidates alone are not proof of the forwarding path.

Capture the container's external interface and the egress host's while testing.
No browser UDP should leave the container, and the egress host should exchange
UDP only with the upstream's relay, never with the STUN receivers. Test all of:

- fresh peer connections and associations;
- a Chromium restart;
- `supervisorctl stop udp-relay`, with HTTPS and live view still working;
- egress-proxy with UDP disabled, then stopped, checking there is no direct path;
- an egress-proxy restart while an association is open;
- instance invalidation while an association is open;
- deletion of only this experiment's DNAT rule, checking FORWARD deny counters;
- whole-container restart, re-enabling UDP only after the guard is installed;
- IPv6 behavior, distinguishing an IPv6-disabled kernel from an IPv6-capable test.

Verify live-view video frames advance and the service records a connected ICE
state. A healthy HTTP endpoint does not establish media health.

## Limits

The shim is intentionally IPv4-only, permits only the explicit public test
destinations, and caps itself at 32 flows with a 60-second idle timeout. It is
not a production design for capturing browser UDP. Results for an ISP binding do
not establish behavior for rotating residential bindings, other providers,
different kernels, or the deployed stealth browser.

No production credentials belong in captures. Detach the private exposure before
stopping the container, then remove the runtime directory. Do not leave an
enabled experiment running unattended.
