# CDP standby experiment

Single-client proof of concept. No production proxy, browser image, or Hypeman code is changed. The Go executable lives in `server/experiments/cdpstandby`; it is not included in an image build.

## Findings

The smallest successful approach was **direct forwarding with upstream TCP keepalives disabled**. On same-host restore, the original socket and CDP session survived; no guest relay or image change was required.

Measured on a disposable 2-vCPU, 2-GiB Cloud Hypervisor browser running Chrome 153.0.8010.36 and Playwright 1.58.2. Detailed timings and limitations are in [`results.json`](results.json).

| Approach | Sleep | Result |
| --- | --- | --- |
| Direct, default TCP keepalives | 1s, 10s, 60s | Passed all state assertions |
| Direct, default TCP keepalives | 180s | Host TCP connection timed out while asleep |
| Direct, TCP keepalives disabled | 180s | Passed; first command completed in 1.128s |
| Guest relay, fresh HTTP connections | 1s, 180s | Passed all state assertions |
| Close/reopen browser WebSocket | 1s | Pre-sleep CDP session call timed out (negative control) |

The final code also passed one-second automatic-idle cycles for both successful approaches on a second instance. There were eight successful state-preservation cycles across these runs. This is not a latency comparison: restore varied substantially, and full snapshots took tens of seconds on this setup.

**Stability remains unproven.** After the relay assertions passed on the heavily reused first guest, Chrome became stuck in filesystem writeback (`folio_wait_writeback` through `ext4_evict_inode` and overlayfs teardown). New CDP connections and guest-agent exec timed out. The cause was not isolated. A fresh instance was used for the remaining controls; this observation must not be treated as evidence of production readiness.

Recommendation: first pursue an opt-in, same-host direct-proxy experiment with explicit activity holds. Retain the relay option if the external socket must be replaceable. Neither approach establishes that command silence is safe browser idleness.

## Alternatives

- **direct**: keep the original gateway → browser TCP/WebSocket connection open across standby. Answer client WebSocket pings locally. The next CDP message restores the instance before forwarding.
- **direct + no TCP keepalive**: the same path, with upstream TCP keepalives disabled. This isolates connection liveness probes from browser work.
- **reconnect**: negative control. Keep the client WebSocket open but close and recreate the browser WebSocket. This should lose session-scoped CDP state, even though page memory survives.
- **relay**: a guest process owns a persistent loopback connection to Chrome. The gateway sends commands and polls events over short-lived HTTP connections. No host-to-guest TCP connection needs to survive standby. Commands have sequence numbers; retrying the most recently accepted sequence does not execute it again. Events remain buffered until acknowledged by the next poll. The Chrome socket has process lifetime, not HTTP request lifetime.

HTTP polling is deliberately simple and not a proposed production transport. It makes the lifetime boundary explicit without building a resumable WebSocket protocol.

## Build and unit tests

```sh
cd server
CGO_ENABLED=0 go build -o /tmp/cdpstandby ./experiments/cdpstandby
go test -race -count=10 ./experiments/cdpstandby
go vet ./experiments/cdpstandby
```

The unit tests cover session-scoped command IDs, rejecting standby with a pending command, idle-triggered standby, ping without wake, command-triggered restore, relay authentication, duplicate-command suppression, event replay/acknowledgment, and failing rather than silently dropping buffered events.

## Run on a dedicated test instance

Use a Linux KVM development host with Hypeman. Use a disposable browser instance, not a shared browser or a production instance. Disable Hypeman's automatic standby for this experiment: the gateway explicitly invokes standby and restore. Do not change shared service configuration.

Create a browser from an already-built image, with 2 vCPUs and 2 GiB memory. Wait for `http://<guest-ip>:9222/json/version` to respond. Supply a Hypeman API token in a permissions-restricted file. Do not put tokens in arguments, source, or results.

```sh
cd bench/cdp-standby
bun install --frozen-lockfile
export BINARY=/tmp/cdpstandby
export API=http://127.0.0.1:4973
export API_TOKEN_FILE=/path/to/api-token
export INSTANCE=<dedicated-instance-id>
export UPSTREAM=ws://<guest-ip>:9222

# Default TCP keepalives; short sleeps, then a longer negative case.
MODE=direct SLEEPS_MS=1000,10000,60000 timeout 600 node run.mjs
MODE=direct IDLE_MS=500 SLEEPS_MS=180000 timeout 600 node run.mjs

# Restore the test instance manually if the previous run lost its connection.
# Reuse the original connection, but disable upstream TCP keepalives.
MODE=direct NO_TCP_KEEPALIVE=1 IDLE_MS=500 SLEEPS_MS=180000 timeout 600 node run.mjs

# A fresh browser WebSocket cannot preserve existing attached sessions.
MODE=reconnect SLEEPS_MS=1000 timeout 600 node run.mjs
```

Each invocation launches its own gateway, connects Playwright once, and stops the gateway afterward. Run one test at a time. Gateway status and manual standby are localhost-only by default. `IDLE_MS=0` uses explicit standby; a positive value uses command inactivity. Status checks do not wake the browser.

### Relay setup

Copy the static executable and a randomly generated, permissions-restricted relay token into the test instance. Start this process **inside the guest**, substituting Chrome's actual loopback WebSocket URL from `http://127.0.0.1:9223/json/version`:

```sh
/tmp/cdpstandby -mode guest -listen 0.0.0.0:9230 \
  -upstream ws://127.0.0.1:9223/devtools/browser/<browser-id> \
  -relay-token-file /tmp/relay-token
```

Run it under a supervisor or with detached stdio so the exec session can close before standby. Only the test host should be allowed to reach this port. It requires the shared bearer token, but does not provide TLS; keep it on the private test network. Do not expose Chrome or the relay publicly.

Then run on the host:

```sh
export RELAY_TOKEN_FILE=/path/to/the-same-relay-token
UPSTREAM=http://<guest-ip>:9230 MODE=relay IDLE_MS=500 \
  SLEEPS_MS=1000,180000 timeout 600 node run.mjs
```

Restart the guest relay before each new relay test invocation: it deliberately owns exactly one logical CDP connection and sequence space. Do not restart it between cycles within a test. Stop it after the test, including before switching to direct mode; otherwise its retained auto-attach/debugger state can interfere with other clients.

## What is asserted

Before sleeping, the harness establishes:

- a Playwright browser, page, and JSHandle;
- a raw attached CDP session, execution context, and RemoteObjectId;
- a Runtime binding and event listener;
- a debugger breakpoint.

After waking, it uses those same objects, increments counters exactly once, receives the binding event, hits the existing breakpoint, resumes execution, and checks that Playwright never disconnected. An outstanding `Runtime.evaluate(awaitPromise=true)` must block standby. Tests use a deterministic local fixture rather than external websites.

`wake_ms` measures the restore API call; `command_ms` measures the first post-sleep command through its response. Neither is a general platform latency benchmark.

## Limits and next experiments

- Command silence is not semantic idleness. A returned navigation command, an event waiter, a download, or page-side work can still be active. This prototype can freeze those. It is opt-in, not safe as a default policy.
- Direct mode depends on the original host socket and restored guest network identity. Disabling TCP keepalive does not protect against unacknowledged data timing out, network changes, host/proxy restarts, or external connection tracking expiry.
- Relay state survives a snapshot, not a relay or Chrome crash. No persistence, multi-client support, reconnection after gateway failure, or transparent recovery from arbitrary transport errors is implemented. Unknown failures close the client rather than silently replaying commands.
- Relay output is capped at 4 MiB. Overflow fails the session. The gateway uses backpressure and admits one client message at a time. Relay Chrome writes and lifecycle/relay HTTP requests have time limits.
- A lifecycle mutex prevents new commands from being forwarded during standby. Relay polling also stops before snapshot. This is not a complete quiescence protocol for browser-generated events or external network traffic.
- The idle timer ignores server events and pings. Non-CDP activity and production billing/activity tracking are not integrated.
- Add explicit activity holds, multiple attached clients, event-heavy pages, commands arriving during snapshot, randomized transport loss, and longer sleeps before proposing production integration.
- Restore and then delete the disposable instance; stop the relay/gateway and any dedicated test service. A failed test can leave the instance in standby. A timed-out snapshot can require explicit recovery; do not retry commands blindly.
