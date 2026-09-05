# ACP WebSocket proxy

This is the first integration milestone for an ACP-first agent API: a real
kernel-images route and an existing-client compatibility gate. It deliberately
contains no conversation REST protocol, runtime resource, prompt journal,
idempotency layer, session-ID translation, or delivery replay.

## Implemented surface

- `GET /agent/v1/harnesses` returns configured harness names. This is configuration
  discovery, not a claim that a harness/model or every ACP capability is validated.
- WebSocket `GET /agent/v1/acp?harness=pi` starts `acpremote expose` with the trusted
  launch definition and proxies ACP messages unchanged.
- Names are `pi`, `codex`, `claude`, and `gemini`. Only provisioned names are enabled.
- `AGENT_CONFIG_PATH` enables the routes. Empty or invalid configuration leaves
  them unavailable without breaking the browser's other APIs.

These routes use the existing browser API authentication/routing boundary, like
`/process/{process_id}/attach`; they do not introduce another public token scheme.
Do not expose the image server directly to an untrusted network. Each internal
bridge binds only to loopback and requires a generated bearer token; browser
credentials and client headers are not forwarded to that bridge.

## Lifecycle

For each client connection the server starts a dedicated `acpremote expose`
listener on an OS-assigned loopback port. Opening its upstream WebSocket starts
one harness/adapter subprocess. The listener and subprocess are scoped to that
attachment and are cleaned up when the connection ends. Multiple concurrent
attachments are allowed within `maxConnections`; each may contain multiple ACP
sessions if the selected harness supports them.

A disconnect is termination, not detach. On reconnect, a fresh subprocess may load
an exact persisted native conversation with ACP `session/load` or `session/resume`.
Native storage must survive connection cleanup. No output is retained by the proxy,
and an interrupted tool's side effect is not automatically reconciled or repeated.

The proxy retains no ACP state. It does not intercept initialize, permissions,
model selection, MCP definitions, session IDs, or results/errors. Subprotocol
`acp.v1` is accepted when offered; no subprotocol is required. Frame size is limited
to 1 MiB to match acpremote 1.7.0's default. Downstream ping/pong uses a 20-second
interval and timeout so a dead network connection cannot leave the agent running
indefinitely while the proxy answers upstream pings.

Server shutdown cancels startup and active connections. Bridge cleanup first asks
the CLI to shut down gracefully, then kills the owned process group after at most
six seconds so adapters cannot leave ordinary descendant processes behind. This
is process cleanup, not a security sandbox for programs that deliberately daemonize
outside their process group.

## Trusted launch catalog (bootstrap, not the declarative public API)

Install Python 3.11+ and the pinned transport dependencies in a private environment:

```sh
python3 -m venv /opt/kernel-agent/venv
/opt/kernel-agent/venv/bin/python -m pip install -r server/runtime/acp/requirements.txt
```

The image does not install these dependencies or any harness automatically in this
milestone. An operator must provision the environment and pinned harness/adapter
binaries before enabling the feature. The bridge executable is expected to be
acpremote 1.7.0; startup reads that CLI's loopback readiness banner, not agent output.

Example catalog, assuming the executable and working directory are provisioned:

```json
{
  "acpremote": "/opt/kernel-agent/venv/bin/acpremote",
  "maxConnections": 8,
  "harnesses": {
    "gemini": {
      "command": "/opt/kernel-agent/gemini/node_modules/.bin/gemini",
      "args": ["--acp"],
      "cwd": "/workspace",
      "env": {"HOME": "/home/kernel"},
      "inheritEnv": ["GEMINI_API_KEY"]
    }
  }
}
```

Set `AGENT_CONFIG_PATH` to this private file and restart the image server. Launch
configuration is read once; editing the file does not hot-reload running processes.
The catalog is limited to 64 KiB. Commands and working directories must be absolute.
It is trusted operator input, not a mechanism for a WebSocket caller to supply a
command, arguments, or environment. Agent stderr is discarded by this first
integration; no transcript or secret-bearing log stream is exposed.

Only PATH, HOME, USER, LANG, TMPDIR, TERM and certificate paths are inherited by
default. Provider credentials must be explicitly named by `inheritEnv`; a missing
or empty required value rejects connection startup. `env` overrides inherited
values and should contain nonsecret settings only. Neither is returned by discovery.

All connections to a harness use the same configured environment and working
directory. Shared configuration, extensions, MCP files and native history are not
copied or deleted per connection. For Pi, explicitly setting `PI_CODING_AGENT_DIR`
selects shared Pi state; project-local discovery remains Pi/adapter behavior.
Clients must supply a working directory meaningful on the remote host. Local IDE
files, terminal callbacks and local stdio MCP servers do not become remotely
available automatically.

## Acceptance checks

The integration suite uses real acpremote processes and TCP WebSockets. Its
stdlib-only ACP peer is deterministic: no provider credentials or model calls.

```sh
python3 -m venv /tmp/acp-test
/tmp/acp-test/bin/python -m pip install -r runtime/acp/requirements.txt
AGENT_PROXY_TEST_ACPREMOTE=/tmp/acp-test/bin/acpremote \
  go test -race -v ./lib/agentproxy ./lib/wsproxy
```

Run from `server/`. CI installs the pinned bridge and sets the environment variable;
without it, the external-process cases explicitly skip rather than claim a pass.

The gates cover independent concurrent processes, multiple native sessions per
connection, connection admission, termination during an in-flight prompt, exact-ID
history load after reconnect, permissions, opaque content/metadata, server shutdown,
message limits, and unresponsive clients. A separate case drives the unmodified
`acpremote mirror` CLI using the official ACP Python SDK. No Kernel-specific client
messages or headers are needed.

ACP UI 0.1.16 at commit `cd9c3cb464a4b321bff652101953a64c07473e31` was also tested
locally without source changes: initialization, session creation, streamed output,
permission approval, disconnect, and exact-session restoration in a fresh process.
This was a local WebSocket test with the deterministic peer, not a TLS/gateway or
real-model test. The UI check is not yet part of CI.

## Remaining work

The public declarative configuration GET/PUT API, transactional preparation,
harness/extension installers, and model-provider secret bindings remain a separate
milestone. The bootstrap catalog is not their replacement. They should produce
trusted launch definitions while making shared filesystem effects explicit.

Each harness still needs pinned native installation, capability/MCP/media tests,
permission and native restoration checks, and shared-configuration verification.
Successful fixture/client interoperability is not a blanket harness support claim.
Platform-level routing/authentication and packaged-image tests with the feature
enabled are also required before public availability.
