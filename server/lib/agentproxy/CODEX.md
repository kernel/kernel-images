# Packaged Codex ACP reference

Codex is an independent declarative harness alongside Pi. It uses the existing
configuration/revision manager and connection-owned WebSocket bridge unchanged.
ACP owns initialization, authentication, sessions, history, prompts, cancellation,
permissions and model/mode controls. There is no Kernel conversation protocol,
session registry, saved proxy output or automatic prompt retry.

## Configure

Inject `OPENAI_API_KEY` into the image environment. GET
`/agent/v1/harnesses/codex/config` and PUT with its ETag in `If-Match`:

```json
{
  "launch": {
    "provider": "openai",
    "model": "gpt-5.4-mini",
    "thinking": "low",
    "mode": "read-only",
    "credential": "openai"
  },
  "shared": {
    "instructions": "Keep replies brief.",
    "webSearch": "disabled",
    "mcpServers": []
  }
}
```

Connect to `/agent/v1/acp?harness=codex`. Call ACP `initialize`, then
`authenticate` with `{"methodId":"api-key"}`. No key value is needed in ACP;
the adapter reads the selected environment binding. `NO_BROWSER=1` disables
browser-based ChatGPT login. The managed profile uses native ephemeral auth
storage, so API-key authentication does not persist a key in `auth.json`.
Reauthenticate on each fresh connection. The catalog's `codex.credentials` can
add operator-defined binding names; requests contain names, never secret values.
Only referenced bindings reach the launch wrapper. It passes the provider key
under `CODEX_API_KEY` and removes binding source names and ambient adapter
configuration/logging variables from the child environment. Native shell snapshots
are disabled (`features.shell_snapshot` and `shell_snapshot_v2`): their environment
exports can persist provider/MCP keys. These flags and ephemeral auth storage are
also supplied as native session overrides so project config cannot re-enable them.
`features.plugins=false` disables native plugins and their automatic unpinned
catalog clone/recommendations; native project skills remain available.

### Supported configuration

- `provider`: `openai` only in this preparer. Custom gateways, ChatGPT OAuth,
  other providers and Bedrock are not managed configuration options.
- `model`: native Codex model ID. `thinking`: `minimal`, `low`, `medium`, `high`,
  `xhigh`; the selected model may accept only a subset. ACP config/model controls
  can change a session's selection.
- `mode`: `read-only`, `agent`, `agent-full-access`. In this pinned adapter,
  the misleading `read-only` ID means **workspace-write with user approvals**
  ("Ask for approval"), not a read-only filesystem. `agent` uses workspace-write
  with native automatic review; `agent-full-access` disables sandbox approvals.
  These are native adapter modes, not an additional Kernel sandbox. ACP owns
  permissions and subsequent mode changes.
- `shared.instructions`: native `developer_instructions` (up to 64 KiB).
  `shared.webSearch`: native `web_search`, `disabled`, `cached` or `live`.
- `shared.mcpServers`: native stdio and streamable HTTP (up to 32 servers).
  Pi extensions, package installation, arbitrary native config passthrough,
  managed skills/plugins and SSE MCP are explicitly unsupported. Unknown fields
  are rejected, not silently ignored.

## Native MCP

Use the [shared MCP definition](README.md#shared-mcp-configuration) with
`command`/`args`/`envBindings`, or `url`/`transport: "http"`/`headerBindings`.
Server names must use letters, digits, underscores or hyphens (1–64 characters).
Commands are absolute remote executable paths; preparation checks executability,
not the MCP handshake. URLs cannot contain credentials, queries or fragments.

Native `config.toml` contains no credential values. HTTP headers use native
`env_http_headers` with per-definition hashed environment aliases. Stdio uses
native `env_vars` and a small command wrapper to remap those aliases into the
requested variable names. Different servers can bind the same destination name
to different credentials. MCP children receive platform-default environment plus
only their own bindings, not the provider environment. A changed server definition
gets different aliases so an old connection cannot send an old token to a new
URL. Reconnect after credential or bound-server changes; old processes may lack
the new aliases. HTTP services with required auth should reject missing headers.

ACP `session/new` and `session/load` accept native session MCP definitions.
**The pinned adapter preserves configured shared servers on name conflicts**;
client definitions with the same name are filtered out. Unlike Pi, these do not
override shared defaults. Use distinct names for session additions. Session MCP
configuration is not written into the managed shared file; supply it again on
load. No adapter patch or protocol interception is used.

Native project `.codex/config.toml`, `AGENTS.md`, skills and other native resources
can participate according to Codex's own rules. The adapter trusts the remote
session `cwd`; choose it deliberately. This is not a sandbox against code with
browser filesystem access. Interactive MCP OAuth and client gateway auth are
upstream ACP capabilities, not managed/binding-based features tested here.

## Revisions and lifetime

The common GET/PUT semantics apply: desired/effective revisions, safe failed
preparation retention, required optimistic `If-Match`, 409 for stale or concurrent
writes, private revision files and last-ready recovery after restart. Preparation
is capped at 30 seconds, validates exact installed runtime versions and native
TOML loading using `codex mcp list --json`, and does not authenticate, start MCP
servers or call a provider. The native command is not strict schema/model
validation; Go validates the supported fields and enums first. A ready revision
is not proof of remote provider/MCP availability or model support.

`/home/kernel/.agents/codex/native` is the stable `CODEX_HOME`; native sessions,
SQLite state and history remain outside replaceable revisions. Its `config.toml`
symlink follows the atomic `current` revision. It is established at launch, not
during preparation. An unexpected existing native config file is not overwritten;
launch fails instead. The operator may change `stateDir`, `runtimeDir`, `node`
and credential bindings in the catalog.

Each connection owns an independent adapter + Codex app-server process tree.
Disconnect cleans up processes, not saved sessions. Updates do not kill existing
connections. Native shared settings can be observed dynamically; full revision
isolation is not promised. Credentials and initial mode are captured at launch.
Successful revisions are retained for old processes; there is no automatic GC.
Direct native/file edits are not reconciled into the API's desired state.

Reconnect with fresh `initialize` → `authenticate` → paginated `session/list`
(follow `nextCursor` even when a filtered page is empty) → `session/load` with the
exact original ID, remote `cwd` and session MCP definitions. Load replays native
history and permits a fresh prompt using that history. `session/resume` is
optional and is rejected by acpremote 1.7.0's local router. No interrupted-turn
continuation, exactly-once side effects or lossless event replay is promised.

## Pins and tests

Both images install **@agentclientprotocol/codex-acp 1.10.0** and
**@openai/codex 0.153.3** (including its matching native platform dependency).
The override prevents the adapter's upstream `^0.153.3` range drifting.
**smol-toml 1.4.2** serializes native config; `runtime/acp/codex/bun.lock` pins the
complete npm resolution. Installation uses Bun **1.4.0**, frozen lockfile and no
lifecycle scripts. Bridge/client versions are unchanged from the Pi base:
acpremote **1.7.0**, Python ACP SDK **0.11.0**, websockets **15.0.1**, packaged
Python **3.12.11**. Native source reviewed at adapter commit
`061f9a4a2e463a220d7a3ab2ae5e9732837085ef` and Codex commit
`b1a547b1f73ce86205d9222ac19cff334b3b7a2e`.

```sh
cd server/runtime/acp/codex
bun install --frozen-lockfile --ignore-scripts
node --test *.test.mjs
cd ../../..
AGENT_CODEX_TEST_RUNTIME="$PWD/runtime/acp/codex" \
AGENT_PROXY_TEST_ACPREMOTE=/path/to/acp-venv/bin/acpremote \
  go test -race ./lib/agentproxy ./lib/wsproxy
```

Unit tests cover configuration validation, HTTP concurrency/failure/recovery,
native preparation, private files, no persisted credentials, environment
isolation, native settings and safe credential-alias changes. A real native
adapter/app-server test exercises streamable HTTP MCP initialize/tools-list with
a bound fixture header, without a model prompt. It checks plugin sync is disabled
and scans the resulting state for fixture keys. This handshake-only test does not
reproduce native shell snapshot creation: the paid gate checks absence of the
snapshot directory and scans all managed files after real turns. Node tests assert
the safety flags in native config and session overrides. Native tests skip
explicitly without the corresponding runtime variables. CI installs both runtimes.

Opt-in paid gate, against a **fresh disposable image** with `OPENAI_API_KEY`:

```sh
AGENT_API_URL=http://127.0.0.1:10001 \
  /path/to/acp-venv/bin/python lib/agentproxy/testdata/codex_gate.py
```

This refuses to overwrite an existing configuration and uses small bounded
`gpt-5.4-mini` turns. It tests independent connections, shared/session stdio MCP,
updates and failed preparation while connected, disconnect cleanup, absence of
persisted provider credentials and fresh paginated list/load/history/model recall.
To force two native discovery pages without 26 extra paid turns, the gate creates
26 native forks and seeds only those pagination fixtures with a user event and
preview in the pinned native storage format. The original real session and its
history are never modified by fixture seeding. Test transcripts are local private
evidence, not proxy output persistence. HTTP MCP tool invocation, media, native
extensions, cancellation mid-tool and provider/model combinations beyond the
stated gates need separate validation. Packaged headless testing does not validate
the platform gateway, TLS/authentication boundary, arm64 or headful desktop behavior.
