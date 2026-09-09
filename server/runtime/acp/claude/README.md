# Claude ACP reference

The packaged `claude` harness uses the same Preparer, revision manager and
connection-owned ACP transport as Pi. It does not adapt Pi extensions or parse
ACP conversation messages. Both browser images install this runtime.

## Configure

Inject `ANTHROPIC_API_KEY`, GET `/agent/v1/harnesses/claude/config`, then PUT
with the returned ETag in `If-Match`:

```json
{
  "launch": {
    "model": "claude-haiku-4-5-20251001",
    "credential": "anthropic"
  },
  "shared": {
    "settings": {"language": "English", "alwaysThinkingEnabled": false},
    "mcpServers": []
  }
}
```

Connect to `/agent/v1/acp?harness=claude`. ACP owns initialize/authenticate,
sessions, prompts, permissions, cancellation, history and model controls. The
pinned adapter exposes model selection through `configOptions` (Haiku's native
selection ID is `haiku`), not a legacy `models` response. Native adapter extensions
remain native; Kernel adds no conversation protocol or prompt retry.

`launch.credential` names an operator binding, never a key value. The packaged
catalog maps `anthropic` to `ANTHROPIC_API_KEY`; operators can replace this mapping.
The launcher copies only referenced credentials into its private environment and
sets the native `ANTHROPIC_API_KEY` and `ANTHROPIC_MODEL`. Ambient gateway URLs,
OAuth tokens and third-party-provider switches are not inherited. Direct Anthropic
API-key authentication is validated; OAuth, gateway auth, Bedrock, Vertex and other
providers are not managed configuration options.

Desired/effective revisions, failure retention, optimistic concurrency and response
status codes are the same as the [shared contract](../../../lib/agentproxy/README.md).
Unknown fields, including Pi `extensions`, `provider` and `thinking`, are rejected.
Configuration responses and revision files contain binding names, never resolved
credential values. Preparation checks the pinned native binary, native settings
resolution and executable shared MCP paths without provider credentials or MCP
startup. Ready does not mean provider authentication or MCP connectivity succeeded.

## Native shared settings and MCP

`shared.settings` supports native `language` and `alwaysThinkingEnabled`. These
settings are validated with the pinned SDK resolver and passed to the native CLI
as a revision-local `--settings` file. If the SDK already supplies `--settings`,
shared defaults are merged underneath those native session settings into one CLI
argument, preserving native model restrictions and provider-routing settings.
Arbitrary environment settings, hooks,
plugins, skills, commands, marketplaces and Pi npm extensions are **unsupported**
by this preparer. No extension installation is implied by accepting a revision.

Shared MCP supports stdio only, using the shared server schema:

```json
{
  "name": "docs",
  "command": "/usr/local/bin/docs-mcp",
  "args": ["--read-only"],
  "envBindings": {"DOCS_API_KEY": "docs-token"}
}
```

Declare `docs-token` in the operator catalog's `claude.credentials` map. Shared
stdio children receive platform environment variables and explicit bindings, not
provider keys or the entire adapter environment. Binding values never enter the
native CLI argument list or revision files. Shared HTTP/SSE and header bindings
are rejected pending interoperability validation.

`native-command` is a native executable launcher, selected through the upstream
adapter's `CLAUDE_CODE_EXECUTABLE` setting. It merges shared MCP defaults underneath
the SDK's ACP-supplied `--mcp-config` by server name and invokes the pinned native
binary. It never reads ACP messages. Shared settings and MCP are selected from the
connection's immutable revision. New native sessions within that connection retain
that revision; a new connection sees the new effective revision. Ambient MCP is
excluded with `--strict-mcp-config`, and ambient CLI settings sources are disabled.
The adapter's own native SettingsManager still reads native user/project metadata;
this launcher is not a sandbox or a replacement for native trust/permissions.
These revision guarantees describe the managed launch path, not a security boundary
against native client overrides: `_meta.claudeCode.options.env` can replace wrapper
paths, binding metadata or `CLAUDE_CONFIG_DIR`. Clients with access to this browser
can also modify its files or read process environments through the process API.
Such out-of-band/native overrides are not reconciled into desired/effective state.

ACP `session/new` and `session/load` stdio MCP overrides are validated. Supply them
again when loading; other shared server defaults remain. ACP-supplied stdio servers
retain upstream native environment inheritance, so they are trusted executable
code with access to the agent's credentials. The shared-binding isolation guarantee
does not extend to arbitrary native client configuration. Upstream advertises
HTTP/SSE and other extensions; those remain unvalidated here, not Kernel-managed
features.

## Native state and reconnect

`CLAUDE_CONFIG_DIR=<stateDir>/native` and `HOME=<stateDir>/home` are created on launch,
not during preparation. Native transcripts, auth and other native mutable state
stay outside replaceable revisions. They persist across connection/API restarts
inside the same browser, not browser deletion. Each connection gets independent
adapter/native/MCP processes. Activation does not restart existing connections.

Reconnect with initialize (authenticate if required), `session/list`, then
`session/load` using the exact discovered original ID. Consume `nextCursor` until
absent: 0.75.1 currently returns all sessions in a single terminal page and does not
implement multi-page cursor handling. Load replays history and restores model state;
subsequent prompts can recall prior content without the client supplying it again.
`session/resume` is optional and is not part of this gate. Interrupted-turn
continuation and exactly-once tool side effects are not promised.

Disconnect terminates connection-owned processes through the shared bridge cleanup.
Packaged-image validation observed terminated native children retained as zombies
by the image wrapper; no live owned processes remained. Child reaping is a shared
image lifecycle limitation, not solved in this adapter.

## Pins and verification

- `@agentclientprotocol/claude-agent-acp` **0.75.1** (unpatched).
- `@anthropic-ai/claude-agent-sdk` **0.3.257**, including its exact-version platform
  native binaries (CLI reports **2.1.257**); `@agentclientprotocol/sdk` **1.4.0**.
- `bun.lock` records transitive dependencies; installs use
  `--frozen-lockfile --ignore-scripts` and retain optional native dependencies.
- Uses the images' Node 22 (requires Node >=22.15 for Linux `process.execve`), Bun
  **1.4.0**, and shared ACP bridge/client pins documented in the shared README.

```sh
cd server/runtime/acp/claude
bun install --frozen-lockfile --ignore-scripts
node --test *.test.mjs
cd ../../..
AGENT_CLAUDE_TEST_RUNTIME="$PWD/runtime/acp/claude" \
  go test -race ./lib/agentproxy ./lib/wsproxy
```

The native runtime test skips explicitly without `AGENT_CLAUDE_TEST_RUNTIME`.
Unit coverage includes HTTP validation, credential-safe responses, stale writes,
failed preparation/restart retention, private revision files, no preparation-time
native-state mutation, native argument merging and shared MCP environment isolation.

The opt-in paid gate requires a **fresh disposable packaged image**, injected
`ANTHROPIC_API_KEY`, and a runner with `server/runtime/acp/requirements.txt` installed:

```sh
AGENT_API_URL=http://127.0.0.1:10001 \
  /path/to/acp-venv/bin/python server/lib/agentproxy/testdata/claude_gate.py
```

It uses Haiku 4.5 with a $0.15/session SDK budget, three-turn-per-prompt limit and
120-second request timeout; it never retries prompts. It checks a real initial
turn, shared/overridden MCP, independent processes, activation and failed-preparation
retention, disconnect termination, fresh exact-ID discovery/load/history, native
model state and model recall. Raw events stay in a private local evidence directory.

Locally validated: all non-e2e Go tests under race, `go vet ./...`, native JS tests,
both image runtime-stage builds, and the full headless image real-provider gate.
The full headful image, ARM64, interactive OAuth, media, remote HTTP/SSE MCP,
`session/resume`, cancellation during a tool side effect and platform gateway/TLS
authentication integration were not exercised. CI is filtered to PRs against main;
a stacked PR against another branch does not automatically run that workflow.
