# Gemini ACP reference — new sessions only

Both browser images install **@google/gemini-cli 0.58.0**. Its native
`--experimental-acp` implementation runs unpatched behind the existing ACP bridge.
The Gemini runtime has its own frozen Bun lockfile, including optional native
package versions; install scripts are disabled. It uses the reference's Node 22,
Bun 1.4.0, acpremote 1.7.0 and Python/ACP client pins (see [README](README.md)).
Keychain/OAuth authentication is not part of this managed configuration.

**Reconnect, discovery, load, resume and history replay are unsupported.** Native
`session/list` returns -32601; known-ID load has not been reliable. Gemini still
advertises `loadSession: true` upstream: clients must not interpret that as a
supported Kernel reconnect contract. No capability rewriting, load shim or Kernel
session registry is added. Every new connection must create fresh native sessions.

## Configure

Inject `GEMINI_API_KEY` into the browser environment. The packaged `google`
credential binding selects that variable. An operator can instead bind `google`
to `GOOGLE_API_KEY` in the catalog if that is where a Gemini Developer API key is
provisioned. This does **not** select Vertex AI. Keys never go in the PUT body.
This binding contract applies to the configuration API: native ACP `authenticate`
can separately accept client-supplied API keys or gateway metadata in `_meta`,
which the proxy does not inspect or rewrite.

1. GET `/agent/v1/harnesses/gemini/config` and read its ETag.
2. PUT the configuration with that ETag in `If-Match`.
3. Connect `/agent/v1/acp?harness=gemini`, initialize, authenticate using method
   `gemini-api-key`, and call `session/new` with an existing remote `cwd`.

```json
{
  "launch": {
    "model": "gemini-2.5-flash",
    "credential": "google",
    "trustWorkspace": true
  },
  "shared": {
    "settings": {"maxSessionTurns": 20},
    "mcpServers": []
  }
}
```

`model` is a native Gemini model ID, not a provider-qualified Pi model.
`maxSessionTurns` defaults to 20 when omitted/zero, accepts 1–100, and maps to
native `model.maxSessionTurns`. This is a cumulative lifetime budget of model
round-trips, including tool-continuation turns across all prompts; it never resets.
After exhaustion, later prompts return `max_turn_requests`: create a fresh session.
It is not a wall-clock prompt timeout. ACP owns prompts, permissions, cancellation and session
model/mode controls; the proxy does not transform or retry prompts.

`trustWorkspace` defaults to false. Native MCP (all transports) requires it to be
true; preparation rejects shared MCP servers otherwise. **Opting in trusts the remote
working directories supplied on this connection**, including their native
`.gemini/settings.json`, context and policy files. Provision only trusted remote
workspaces. Native settings can merge additional workspace MCP definitions with
managed definitions. This is not a sandbox or an isolated-settings guarantee.
Hooks and extensions are disabled by the managed launch/settings, and generic
project `.env` loading is disabled; native `.gemini/.env` behavior in trusted
workspaces still applies. With trust off, native workspace executable settings
are excluded and all MCP is unavailable. Native folder trust can also prevent
operations; this configuration does not bypass that policy silently.

## Native shared settings and MCP

The preparer writes a native **system settings** file per revision, selected by
`GEMINI_CLI_SYSTEM_SETTINGS_PATH`. Native user state is under
`/home/kernel/.agents/gemini/home`, outside revisions. System settings take
precedence for managed keys. User settings, native project metadata and native
session files are retained, but retaining files does not imply ACP restoration
support. Existing processes keep their own revision path; new connections use the
last ready revision. Native settings caching and out-of-band filesystem writes
are not reconciled into the desired configuration API.

`shared.mcpServers` uses the reference's declarative schema, with at most 32 unique
names. Supported native transports: stdio, streamable HTTP (`transport: "http"`)
and SSE (`transport: "sse"`). Example:

```json
{
  "name": "docs",
  "command": "/usr/local/bin/docs-mcp",
  "args": ["--read-only"],
  "envBindings": {"DOCS_TOKEN": "docs-token"}
}
```

For an HTTP server, use `url`, `transport` and optional
`headerBindings: {"Authorization": {"credential": "docs-token", "prefix": "Bearer "}}`.
The operator must declare `docs-token` in the Gemini catalog credentials map.
URLs cannot contain credentials, query parameters or fragments. `$` interpolation
in caller-supplied MCP commands, arguments, URLs and header prefixes is rejected;
use bindings instead. Managed MCP discovery has a 15-second timeout per server.
Preparation does not start MCP servers or authenticate to remote services.

Only selected provider/MCP source variables are inherited. The launcher copies
bound values into native environment aliases, removes source variable names and
the internal bridge token, then starts Gemini. Persisted configuration/settings
contain only binding names or environment references, never values. Gemini's
native environment redaction blocks the provider key and binding aliases from
implicit subprocess inheritance; explicitly configured MCP destination variables
receive their bound values. MCP servers and native tools are trusted executable
code with browser filesystem access, not a credential-isolation security boundary.
Native settings expansion semantics still apply to bound values.

Standard ACP `session/new.mcpServers` is passed directly to Gemini. Native Gemini
merges it with shared MCP defaults, overriding by name in that session, including
explicit ACP environment/header values. Nothing is copied into shared settings.
Managed MCP OAuth is not provisioned; use explicit credential bindings instead.
Native authentication errors and permission requests stay on ACP.

## Supported and unsupported

| Feature | Contract |
| --- | --- |
| Fresh sessions, prompts, authenticate, tool permissions, cancellation, model/mode controls | Native ACP; no proxy conversation protocol |
| Declarative model, bounded session turns, explicit workspace trust, shared MCP | Supported as above |
| Optimistic revisions, failed preparation retention, existing connections | Existing Preparer/revision manager, unchanged |
| Reconnect, discovery, load, resume, history replay | **Unsupported**, including known-ID restoration |
| Pi extensions, arbitrary settings, hooks, extension installation, OAuth/Vertex/gateway configuration | Unsupported by this preparer; unknown request fields are rejected |

Ready means schema/materialization and exact installed CLI version checks passed,
not a valid provider key, working remote MCP service or successful future session.
The version probe runs with no provider/MCP credentials in an isolated preparation
directory and a 30-second timeout. Invalid requests return 400/422 without changing
revisions. Failed preparation retains desired and last-ready effective state with
a safe generic error; failed revision directories are removed. Missing If-Match
returns 428, stale/concurrent writes return 409. GET is available during preparation.
Successful revisions remain on disk for existing connections; no automatic GC.

Each connection owns an independent bridge/Gemini/MCP process tree. Native CLI
relaunch is disabled. Disconnect cleanup uses the unchanged shared bridge teardown
(including process-group escalation), without deleting native state. No runtime
resource, durable proxy output, automatic prompt retry or interrupted-turn recovery.

## Validation

```sh
cd server/runtime/acp/gemini
bun install --frozen-lockfile --ignore-scripts
cd ../../..
AGENT_GEMINI_TEST_RUNTIME="$PWD/runtime/acp/gemini" \
AGENT_PROXY_TEST_ACPREMOTE=/path/to/acp-venv/bin/acpremote \
  go test -race ./lib/agentproxy ./lib/wsproxy
```

Unit tests cover configuration validation, secret-free private revision files,
credential mapping and launch environment isolation, failed preparation retention,
stale/concurrent writes, GET during preparation, restart recovery, preservation of
native state and prior revision paths. Optional installed-runtime tests verify the
pin, native ACP initialization/authentication/fresh sessions, model/mode controls,
idle cancellation and HTTP MCP initialization with a bound header, without sending
provider prompts. They cover both trusted and untrusted fresh sessions; CI enables
these tests.

The opt-in real-provider gate uses a **fresh disposable image** with `GEMINI_API_KEY`
and `GEMINI_GATE_MCP_TOKEN=fixture-token` injected. Add a test-only catalog binding
`gemini.credentials["gate-mcp"] = "GEMINI_GATE_MCP_TOKEN"`. Run:

```sh
AGENT_API_URL=http://127.0.0.1:10001 \
  /path/to/acp-venv/bin/python lib/agentproxy/testdata/gemini_gate.py
```

This gate refuses existing configuration, makes at most four small
`gemini-2.5-flash` prompts (90 seconds per ACP call, no prompt retry), exercises
native authentication, shared and session-overridden stdio MCP, native permissions,
MCP credential redaction, independent connections, configuration updates while
connected, failed preparation retention, fresh sessions and disconnect cleanup.
It temporarily renames the packaged CLI entrypoint to exercise failed preparation,
restoring it in `finally`. **Never run it against a shared or non-disposable image.**
It outputs a boolean summary, not raw ACP messages or credentials.

Validated on Linux amd64: headless image build and enabled packaged-image gate;
local race tests including native session controls and HTTP MCP initialization.
The headful runtime stage was also built and verified as 0.58.0, but full headful
image/provider behavior is not independently verified. HTTP MCP tool calls, SSE
MCP interoperability, multimodal input, inference after model switching,
cancellation during active inference, OAuth and platform gateway/TLS integration
are not covered by these tests. Restoration is intentionally not exercised.
