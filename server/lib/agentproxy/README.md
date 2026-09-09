# ACP agents

The browser images bundle pinned Pi and [Codex](CODEX.md) reference implementations. Kernel manages
configuration preparation and connection lifetime; ACP owns conversations.
There is no runtime resource, conversation REST API, prompt journal, automatic
prompt retry, or session-ID translation in the WebSocket proxy.

## API

| Endpoint | Behavior |
| --- | --- |
| `GET /agent/v1/harnesses` | Returns configured harness names, currently `{"configured":["codex","pi"]}` in the packaged images. This does not mean a model credential is configured. |
| `GET /agent/v1/harnesses/pi/config` | Returns desired/effective configuration, revisions, preparation status and an ETag. |
| `PUT /agent/v1/harnesses/pi/config` | Validates, installs and checks the requested configuration, then activates it. Requires `If-Match` from GET. |
| WebSocket `GET /agent/v1/acp?harness=pi` | Starts a connection-owned `acpremote expose` bridge and Pi adapter using the last ready launch definition. |

The routes use the existing browser API routing/authentication boundary, like
`/process/{process_id}/attach`. Do not expose the image server directly to an
untrusted network. Internal bridge listeners bind to loopback and use independent
random bearer tokens; client headers are not forwarded to them.

### Configuration example

Inject `OPENROUTER_API_KEY` into the image environment, then GET the configuration
and use its ETag in PUT. The initial ETag is `"0"`.

```http
PUT /agent/v1/harnesses/pi/config
Content-Type: application/json
If-Match: "0"

{
  "launch": {
    "provider": "openrouter",
    "model": "z-ai/glm-5.3",
    "thinking": "low",
    "credential": "openrouter"
  },
  "shared": {
    "extensions": [],
    "mcpServers": []
  }
}
```

The response, also returned by GET, has this shape:

```json
{
  "revision": "<desired-revision>",
  "status": "ready",
  "desired": {"launch": {}, "shared": {}},
  "effectiveRevision": "<last-ready-revision>",
  "effective": {"launch": {}, "shared": {}}
}
```

The configuration objects above are abbreviated. `status` is `unconfigured`,
`preparing`, `ready`, or `failed`. Before first preparation, desired/effective are
null. Failed preparation retains the desired request and a safe `error` message,
while effective configuration and existing connections retain the last ready
revision. GET remains available during preparation. Effective means the last
configuration prepared by this API, not an audit of out-of-band filesystem edits.

PUT is synchronous, with preparation bounded to three minutes. Concurrent or stale
writes return 409; missing If-Match returns 428; malformed JSON returns 400; invalid
configuration or failed preparation returns 422. After failure, GET obtains the
new desired revision for the next If-Match. Responses never contain credential
values. The request limit is 1 MiB.

A ready revision means software/configuration preparation succeeded, not that a
provider key has been authenticated or a remote MCP service is reachable.

### Launch settings and credential bindings

`provider` supports `openrouter`, `openai`, `anthropic`, and `google`. `model` is the
native Pi model ID. `thinking` is one of `off`, `minimal`, `low`, `medium`, `high`,
or `xhigh`; the pinned adapter does not expose `max`, and individual models may
support only a subset. ACP model/mode
controls can subsequently change a session's selection.

`credential` names an operator-defined environment binding, not a secret value.
The packaged catalog binds `openrouter` to `OPENROUTER_API_KEY`, `openai` to
`OPENAI_API_KEY`, `anthropic` to `ANTHROPIC_API_KEY`, and `google` to
`GEMINI_API_KEY`. Only credentials referenced by the configuration are inherited
by the launched agent. Missing/empty bindings reject preparation. GET returns the
binding names supplied by the caller, not their environment implementation.

### Extensions

`shared.extensions` accepts at most 32 exact-version npm references, for example
`npm:my-pi-extension@1.2.3`. Ranges, floating tags, git sources and local paths are
not supported in this first contract. Packages are installed with Bun and
lifecycle scripts disabled. Only extension resources are enabled from packages;
package skills, prompt templates and themes are excluded.

Preparation imports/initializes extensions in a separate preparation directory
without inheriting provider credentials. Managed MCP server startup is disabled. An extension import/startup
error prevents activation. Extensions requiring install scripts are unsupported.
Extensions are executable, trusted code with browser filesystem access: atomic
activation does not roll back arbitrary side effects performed by extension code.

Project-local executable settings/resources are not automatically trusted. The
managed runtime passes Pi's `--no-approve`; normal native context-file behavior
remains available.

### Shared MCP configuration

At most 32 uniquely named servers are accepted. A stdio definition uses an
absolute remote command:

```json
{
  "name": "docs",
  "command": "/usr/local/bin/docs-mcp",
  "args": ["--read-only"],
  "envBindings": {"DOCS_API_KEY": "docs-token"}
}
```

An HTTP definition can bind request headers:

```json
{
  "name": "docs",
  "url": "https://example.com/mcp",
  "transport": "http",
  "headerBindings": {
    "Authorization": {"credential": "docs-token", "prefix": "Bearer "}
  }
}
```

`docs-token` must be declared in the operator catalog's credential bindings.
`transport` supports `http` (streamable HTTP) and `sse`. Embedded URL credentials,
query parameters and fragments are rejected; use bindings for secrets. Interactive
MCP OAuth is disabled in the managed extension; use explicit credential bindings.

Standard ACP `mcpServers` on `session/new` and fresh-process `session/load` are
also supported. They **override shared defaults by server name** for that native
Pi session. They are kept in process memory rather than written into shared
configuration; clients must supply them again when loading. Other shared servers
remain available. Stdio MCP processes receive SDK platform-default environment
variables plus explicitly supplied bindings/values, not the entire provider
environment. The managed MCP extension uses an isolated explicit config, so
ambient `.mcp.json` and imported host MCP settings do not participate.

Pi-acp 0.0.33 otherwise accepts and stores MCP definitions without passing them to
Pi. `runtime/acp/pi/patch-adapter.mjs` adds per-session environment handoff for
create/load and advertises the implemented HTTP/SSE support. It is a guarded,
version-specific adapter patch, not an interception layer in the Go proxy.

## Lifetime and shared state

Each connection starts its own loopback expose listener and agent process tree.
Multiple connections and multiple native sessions per connection are supported.
Disconnect terminates the owned processes; it does not delete saved sessions.
Unresponsive clients are detected with a 20-second ping interval and pong timeout.
Shutdown first requests graceful bridge cleanup, then escalates to process-group
termination after six seconds. This is cleanup, not a sandbox against deliberately
daemonized processes. WebSocket `acp.v1` is optional and the message limit is 1 MiB.

Configuration activation atomically switches `current` to a prepared revision.
New connections use the new launch definition; existing connections are not killed.
All Pi processes in the browser share managed configuration and native session
storage. Native history and the adapter's ID mapping remain outside the revision
being replaced. Successful revisions are retained for existing processes;
failed preparation directories are removed. There is no automatic GC of successful
revisions yet, so repeated large extension installations consume disk space.

Shared files can be observed dynamically by existing processes; there is no promise
of complete revision isolation. New credentials require a fresh connection, and
extensions/MCP services may require a fresh native session to reload. Native writes
and direct filesystem edits are not reconciled back into the API's desired state.
Saved sessions survive connection and API-process restarts in the same browser,
not deletion/replacement of the browser itself.

The reconnect contract is `initialize`, authenticate if needed, `session/list`,
then `session/load` with the discovered exact ID. Load replays native history.
`session/resume` is optional; acpremote 1.7.0 mirror currently rejects that method
on its local router. Neither path guarantees interrupted-turn continuation,
lossless event replay or exactly-once tool side effects.

`cwd` and stdio MCP commands refer to the browser's filesystem. Client-local paths
and terminals do not become remotely available automatically. Use the process/file
APIs to provision an existing remote working directory.

## Pins and operator configuration

The runtime pins Pi **0.83.0**, pi-acp **0.0.33** with the MCP patch,
pi-mcp-adapter **2.32.1**, acpremote **1.7.0**, ACP Python SDK **0.11.0**,
websockets **15.0.1**, Python **3.12.11**, and Bun **1.4.0**. The runtime's Bun lockfile
pins its JavaScript dependency resolution. User extension dependencies are resolved
and recorded separately in each prepared revision.

Images set `AGENT_CONFIG_PATH=/opt/kernel-agent/catalog.json`. An empty variable
still disables the agent routes. Operators can replace the catalog to configure
`pi.stateDir`, `pi.runtimeDir`, absolute `pi.node`/`pi.bun` executable paths,
credential bindings and an optional npm
`registry`. The default state directory is `/home/kernel/.agents/pi`.

The original trusted `harnesses` launch catalog remains supported for separately
provisioned agents. Pi and Codex have independent packaged declarative preparers. The
`Preparer` interface and common revision manager are the implementation boundary
for subsequent harnesses; their native configuration support must be explicit.
Gemini's future integration excludes reconnect/discovery/load until its ACP
implementation satisfies that protocol gate.

## Validation

From `server/`:

```sh
cd runtime/acp/pi
bun install --frozen-lockfile --ignore-scripts
node patch-adapter.mjs
cd ../../..
AGENT_PI_TEST_RUNTIME="$PWD/runtime/acp/pi" \
AGENT_PROXY_TEST_ACPREMOTE=/path/to/acp-venv/bin/acpremote \
  go test -race ./lib/agentproxy ./lib/wsproxy
```

CI installs the pinned runtimes. Tests cover transactional activation, failure
retention, restart recovery, optimistic concurrency, private files and responses,
real npm-compatible fixture package installation/validation, plus the existing
bridge/official-client lifecycle and content gates. Without the corresponding
environment variables, external-runtime tests explicitly skip.

The opt-in real-provider test requires a **fresh disposable image**, injected
`OPENROUTER_API_KEY`, and a Python runner with `runtime/acp/requirements.txt`:

```sh
AGENT_API_URL=http://127.0.0.1:10001 \
  /path/to/acp-venv/bin/python lib/agentproxy/testdata/pi_gate.py
```

It refuses to overwrite an existing configuration. It exercises independent
connections, shared and session-overridden stdio MCP tools, updates while connected,
failed preparation, process cleanup, and fresh-process discovery/load/history plus
real-model recall. This paid-provider gate is not run automatically in CI.
HTTP/SSE MCP interoperability and media/model behavior beyond these tests require
additional validation; packaged headless-image testing is not a platform gateway
or TLS/authentication integration test.
