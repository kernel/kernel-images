# ACP-first service boundary (proposal)

This is a proposed production contract, not an implemented HTTP API. The current
`/commands`, `/permissions`, `/cancel`, and `/snapshot` routes are private acceptance
fixtures. They must not become a second public conversation protocol. Harness
fan-out remains on hold while the lifecycle and configuration contract is settled.

## Reuse ACP for interaction

Keep a small runtime control surface separate from ACP:

| Proposed endpoint | Responsibility |
| --- | --- |
| `POST /agent/v1/runtimes` | Prepare a pinned launch configuration and start its agent/adapter process. |
| `GET /agent/v1/runtimes/{id}` | Process generation, readiness, resume identity, effective configuration (redacted), outstanding uncertain operations. |
| `POST /agent/v1/runtimes/{id}/acp` | Accept an ACP JSON-RPC request, notification, or response without changing its payload schema. |
| `GET /agent/v1/runtimes/{id}/acp` | Sequenced, resumable SSE containing original ACP server messages; distinguish runtime lifecycle events from ACP messages. |
| `POST /agent/v1/runtimes/{id}/reset` | Explicit, idempotent process replacement and restoration of the same conversation. |

Explicit runtime termination is also needed; closing an HTTP connection is never
termination. Start with one managed conversation per runtime. Multi-session ACP
processes and multiple concurrent writers are not part of the initial contract.

Use standard ACP methods for `initialize`, `authenticate`, `session/new`,
`session/prompt`, `session/cancel`, `session/load`, `session/resume`, and supported
configuration methods. Advertise only client capabilities that the service can
actually fulfill. The current driver does not provide filesystem/terminal
callbacks; this is not a transparent implementation of every ACP capability.

The fixture mappings are:

- `/commands` -> `session/prompt`; `prompt` is already an ACP ContentBlock array.
- `/cancel` -> `session/cancel`, which is a notification, not an RPC request.
- `/permissions` -> a JSON-RPC response to `session/request_permission`, with the
  offered ACP outcome/option ID. There is no ACP `session/permissions` method.
- `/events` -> agent `session/update` notifications, requests, and RPC results/errors.
- `/snapshot` -> Kernel transport/recovery state; there is no corresponding ACP method.

The additions to ACP are narrowly scoped: durable HTTP acceptance, operation
idempotency, replay cursors, and runtime generation/lifecycle state. A stable
HTTP idempotency key must apply to notifications and permission responses too;
JSON-RPC IDs alone are not durable operation identities. Reject stale-generation
controls, namespace native request IDs by generation, and check duplicate requests
before generation/admission checks so lost acknowledgements remain retryable.

An HTTP 202 acknowledges journal persistence. The actual ACP result/error arrives
on SSE with its JSON-RPC ID. A prompt result means turn completion, not acceptance.
SSE IDs support exclusive replay after the last applied event. Reconnect is not
`session/load`: the still-running process should not be restarted or re-prompted.

ACP's remote HTTP transport is still an active proposal and explicitly defers
reliable delivery replay. This binding must be named/versioned as a Kernel
extension, not advertised as conformance to a finalized ACP HTTP standard.

## Content, not a text wrapper

The fixture now journals and forwards ACP content arrays, preserving block order,
annotations, `_meta`, media data, resource links, and embedded text/blob resources.
It also journals unmodified prompt responses/errors, not just streamed updates.

The shared service performs structural validation and bounds encoded prompts to
4 MiB. It does not duplicate the entire ACP content schema. The driver rejects
optional content unless the agent advertises the relevant prompt capability:
`image`, `audio`, or `embeddedContext`. Text and resource links are baseline ACP
content. A model may still reject content despite an agent-level capability;
that must surface as an error, never silent conversion or omission.

Deterministic content tests cover HTTP -> journal -> ACP subprocess -> SSE ->
reopened journal, content above the old 64 KiB HTTP limit, metadata/large integer
preservation, equivalent-JSON retries, changed-content conflicts, buffer ownership,
and unsupported capability rejection before dispatch. Media in the echo peer is
opaque fixture data; this proves transport fidelity, not model media comprehension.
Real harness tests must separately check valid supported media and resources.

This changes the experimental prompt wire shape from a string to an array. Use a
new evidence/journal directory; no legacy journal migration is implemented.

## Restore a conversation, not an interrupted side effect

ACP v1 already defines:

- `session/load`, gated by `agentCapabilities.loadSession`: restore context and
  replay conversation history with `session/update` before returning.
- `session/resume`, gated by `agentCapabilities.sessionCapabilities.resume`:
  restore context without replaying history. This is stable, not just an RFD.

Neither method is an exactly-once checkpoint for arbitrary tools. Neither promises
that a half-finished prompt, shell command, or remote MCP side effect is resumed
safely. Native persisted history can also lag the transport journal after a crash.

Persist the ACP session ID, adapter-specific native resume handle when different,
configuration revision, workspace, and runtime generation. ACP IDs need not equal
CLI-native IDs; a native session file may not exist yet immediately after creation.
Never infer a resume target using a CLI's "latest session" flag.

Proposed reset procedure:

1. Journal the reset's stable operation ID and expected generation. Require explicit
   acknowledgement of the interrupted/uncertain operation when resetting a busy
   or crashed runtime; do not guess on behalf of a background reconnect.
2. Stop admission, invalidate old approvals, and stop/reap the old process group.
   Record any unconfirmed old turn as `uncertain`; do not relabel it successful.
3. Start a new generation using the same configuration revision and persistent
   session storage. Reinitialize and authenticate as needed.
4. Use advertised `session/resume`, otherwise `session/load`. If neither is supported
   or the exact native session is missing, fail explicitly; never silently create
   a new conversation. Capture fallback load history separately from delivery replay
   so restarting does not append duplicate historical messages to the live log.
5. Persist readiness after successful restoration. Permit a new, explicitly submitted
   prompt while retaining the old operation's uncertain outcome and deduplication.

The implementation currently fences uncertain sessions permanently because it has
no reset/reconciliation operation. That is an experimental safety limit, not the
intended product behavior. Reset is not implemented by this change.

Before enabling reset for a harness, test a fresh process with the exact saved ID,
context continuity, same-ID retry after reset, stale permission rejection, and
no model/tool auto-continuation during restore. Kill during tool and approval
boundaries, and during reset itself. Local process termination cannot cancel an
external side effect already accepted by a remote service.

## Configuration may write files; make the scope explicit

Do not require callers to reverse-engineer CLI flags, installation commands, or
MCP config files. Accept declarative startup configuration and materialize it
before the runtime becomes ready. Keep ACP's `mcpServers` schema and configuration
methods instead of inventing alternative MCP or model-setting protocols.

Separate three scopes:

- **Runtime/session-owned (default):** generated settings, credentials, MCP overrides,
  extension selection, writable caches, and native session storage. Persist these
  across reset. Use harness-specific directory overrides and explicit config APIs.
- **Shared pinned artifacts:** downloaded harness/adapter/extension packages. Resolve
  versions and integrity once; concurrent preparation uses locks and atomic publish.
  Sharing package bytes must not imply sharing mutable activation settings.
- **Explicit workspace/shared configuration:** opt-in writes affecting other sessions.
  Record a revision and serialize preparation. Existing processes keep their resolved
  configuration until explicitly restarted; do not promise arbitrary extensions can
  be transactionally rolled back.

For Pi, `PI_CODING_AGENT_DIR`, explicit extension paths, and explicit session storage
allow managed state to be scoped. Project discovery also needs a policy: changing
HOME alone does not prevent `.pi/settings.json` or project MCP config from loading.
The current `pi-mcp-adapter` additionally supports `createMcpAdapter({config})` and
runtime MCP registration; its in-memory configuration avoids ambient file merging.
The integration should use that boundary or an equally explicit pinned mechanism.

Configuration isolation is not a security sandbox. Extensions and installer scripts
can execute code with the process's permissions; the system must not promise that
arbitrary packages only write managed directories. Keep native setup directories
available after a process exits, and distinguish terminate from destructive purge.
Secrets and transcripts require private storage. Do not put secret values in public
lifecycle events or effective-configuration responses; ACP payloads/logs themselves
can contain sensitive data and need access controls and an at-rest policy.

## Remaining pre-production gates

Content transport is executable; the public ACP HTTP binding, reset lifecycle,
configuration materializer, and their shared acceptance tests are not. Existing
crash/replay tests must stay strict when adding recovery. Also still required:
auth/session authorization, writer ownership, bounded retention/backpressure,
expired-cursor recovery, and real proxy/suspend testing.

## Sources

- [ACP session setup, including load and resume](https://agentclientprotocol.com/protocol/v1/session-setup)
- [Resume stabilization](https://agentclientprotocol.com/rfds/session-resume)
- [ACP content blocks](https://agentclientprotocol.com/protocol/v1/content)
- [Remote transport proposal and deferred replay guarantees](https://agentclientprotocol.com/rfds/streamable-http-websocket-transport)
- [Pi adapter restores a specific session file](https://github.com/svkozak/pi-acp/blob/d1cffc047ab37a096ee70ca39cfc1de463db8d12/src/acp/agent.ts#L180-L226)
- [Codex adapter uses threadResume for resume/load](https://github.com/agentclientprotocol/codex-acp/blob/061f9a4a2e463a220d7a3ab2ae5e9732837085ef/src/CodexAcpClient.ts#L530-L605)
- [Claude adapter distinguishes resume and history load](https://github.com/zed-industries/claude-agent-acp/blob/3e23c5b960b66a6d2c892e7524c952e731c076a7/src/acp-agent.ts#L2139-L2166)
- [Pi MCP programmatic configuration and runtime registration](https://github.com/nicobailon/pi-mcp-adapter/blob/8243eba3421e301c88c047444f34ab7d5d57163e/index.ts#L1343-L1378)
- [Gemini native session persistence and exact-ID resume](https://geminicli.com/docs/cli/session-management/)
