# Agent transport acceptance suite

An isolated **custom POST + SSE** session-service experiment and a reusable ACP v1
stdio harness driver. No production route is registered and no coding agent is
installed automatically. Deterministic tests passing is not a real-harness pass.

## Run the shared gates

From `server/`:

```sh
go test -race -count=30 ./experiments/agenttransport/...
go vet ./experiments/agenttransport/...
```

The existing server unit-test target includes these packages. Tests use actual
HTTP/TCP connections, a separate ACP subprocess, and on-disk journals. Recovery
tests kill a separate process instead of gracefully shutting it down. No model
calls or provider credentials are needed for the deterministic gates.

## Executable coverage

| Gate | Test | Required result |
| --- | --- | --- |
| Disconnect | `TestDeterministicACP/disconnect-mid-tool` | Detach at a counted tool barrier, retry, finish while no stream is attached, replay the complete missing suffix. Same process, one dispatch, one tool invocation. |
| Lost acknowledgement | `TestDeterministicACP/lost-prompt-acknowledgement` | Send over TCP without reading its response; retry without executing twice. |
| Permission | `TestDeterministicACP/permission-detach-and-retry` | Pending request survives detach; only an offered `allow_once` option is selected; duplicate answers have one effect and conflicting answers fail. |
| Cancellation | `TestDeterministicACP/cancel-pending-permission` | Cancel while approval is pending, retry cancellation, execute no tool, clear the pending request. |
| Agent crash | `TestDeterministicACP/agent-crash-fences-session` | Kill the ACP process mid-tool; expose `uncertain`, do not resend the prompt, reject new work in that session. |
| Service restart | `TestJournalRecoveryAfterProcessKill` | Reopen after kill at accepted, tool-start, permission-pending, and completed boundaries. Completed stays completed; unfinished becomes uncertain. No automatic dispatch. |
| Write failure | `TestPersistenceFailurePreventsDispatch`, `TestPermissionPersistenceFailureNeverApproves` | A failed journal write cannot acknowledge/dispatch a prompt or release an approval. The runtime becomes unavailable. |
| Journal integrity | `TestJournalLockAndTornTail` | One owner, repair incomplete final records, reject complete corrupt records. |
| Retry contention | `TestConcurrentRetriesAndPayloadConflict` | Twenty concurrent duplicate submissions execute once; changed payload with the same ID is rejected. |
| Cursor validation | `TestInvalidReplayCursor` | Malformed/negative cursors return 400; future cursors return 409. |
| MCP fixture | `TestMCPToolBarrier` | MCP initialization, tool discovery, invocation, counted barrier, and release work independently of the agent. |

The first five cases are exported through `acceptance.Run`, `RunPermissions`,
and `RunCrash`. The **same assertions** run against real harnesses through
`TestRealHarness`; each subtest gets a new agent and workspace.

## Run a real harness

1. Install pinned harness and ACP adapter versions. Authenticate separately using
   an approved provider credential or existing CLI login. Do not embed secrets
   in JSON, prompts, branches, or committed files.
2. Create a private configuration, for example `/tmp/harness.json`:

```json
{
  "harness": "replace-with-harness-name",
  "harnessVersion": "replace-with-verified-version",
  "adapterVersion": "replace-with-verified-version",
  "acp": {
    "command": "/absolute/path/to/pinned-acp-adapter",
    "args": [],
    "env": [],
    "inheritEnv": [],
    "setup": []
  }
}
```

3. Run:

```sh
AGENT_ACCEPTANCE_CONFIG=/tmp/harness.json \
AGENT_ACCEPTANCE_EVIDENCE_DIR=/tmp/harness-evidence \
go test -race -v -count=1 ./experiments/agenttransport/harness \
  -run '^TestRealHarness$' -timeout=20m
```

The harness suite builds `cmd/agent-probe` and injects the `acceptance-probe` stdio
MCP server through `session/new.mcpServers`. Its `checkpoint` tool records each
invocation, blocks until the test releases it, then returns a completion message.
The runner must actually invoke the tool; missing MCP plumbing times out and fails
rather than being counted as support.

The default `acp` configuration must permit that test tool without interaction.
Add a full **separate** `permissionACP` configuration object to test approval
behavior; it must require approval for the checkpoint, not bypass it. If omitted,
permission tests report **SKIP / NOT VALIDATED**, not a compatibility pass.

Supported configuration details:

- `authMethod` optionally invokes ACP `authenticate` after initialization. This
  driver does not implement interactive terminal login. Pre-authentication or a
  supported noninteractive agent auth method is required.
- `setup` contains `{ "method": "session/set_config_option", "params": {...} }`
  calls; `session/set_mode` and `session/set_model` are also accepted. The driver
  injects `sessionId`. Unsupported setup methods fail; agent errors are surfaced.
- `{workspace}`, `{probeDir}`, and `{probeCommand}` expand in command, arguments,
  and explicit environment entries. A small wrapper can prepare harness-specific
  configuration before exec, without modifying the shared suite. Each workspace
  is a child directory separate from journal/probe evidence.
- Only basic process environment (PATH, HOME, USER, LANG, TMPDIR, TERM and certificate
  paths) is inherited by default. `inheritEnv` explicitly names additional variables,
  such as a provider key; values are looked up at runtime and never printed by the
  driver. `env` is for explicit nonsecret configuration. Existing HOME-based CLI
  credentials remain accessible; this is a test driver, not an agent sandbox.
- Each startup and test body has a two-minute bound. Unknown configuration fields
  are rejected. Versions in the config are declarations: attach actual version
  command output separately rather than treating these strings as verification.

Evidence directories contain `capabilities.json`, `events.jsonl`, `agent.stderr`,
and the tool invocation counter/release marker. Treat all evidence as private:
agent output and logs can contain sensitive information. Do not commit it. If no
evidence directory is configured, temporary evidence is deleted after the test.

## Contract owned by the shared session service

| Endpoint | Meaning |
| --- | --- |
| `POST /commands` | `{id,prompt}`; durable acceptance before local dispatch. Reusing an ID with a different payload returns 409. Returns `{id,state}` with 202, including for identical retries. |
| `POST /permissions` | `{id,operationId,requestId,optionId}`; offered choices only, durable decision before releasing the blocked agent. Retry the same control ID/body. |
| `POST /cancel` | `{id,operationId}`; durable cancellation request before signalling the turn. Retry the same control ID/body. Cancellation does not undo prior side effects. |
| `GET /events` | SSE `id` equals event `sequence`. `Last-Event-ID` is the last **applied** event. Replay is exclusive of that cursor and seamlessly continues with live events. |
| `GET /snapshot` | Current sequence, operation states, and unresolved permissions from one synchronized snapshot. It is not a replacement for conversation replay. |

Operation IDs and control IDs are stable application identities, not ACP JSON-RPC
request IDs. There is one active turn per session. Admission checks happen after
idempotency checks. HTTP detach never cancels the turn or restarts ACP. Original
ACP `session/update` messages are retained as JSON payloads, not flattened text.

ACP v1's prompt response means completion, not acceptance. The service's
`accepted` event means the session service has journaled the command; it does
**not** claim the agent has already accepted it. On cancellation the ACP driver
sends `session/cancel` and waits up to five seconds for completion before killing
the agent process group. A forced kill fails the harness cancellation gate; it
is not silently counted as native cancellation support. It does not reuse an
agent that failed to stop. It does not advertise client filesystem or terminal delegation; execution remains local
to the agent. Unsupported server requests receive an explicit protocol error.

`FileStore` appends and fsyncs before publishing events or releasing work. On
restart, a nonterminal operation becomes `uncertain`, its old permission requests
cease to be actionable, and new commands are fenced off. Retry returns the
existing state. This is deliberately conservative: a crash between dispatch and
recording its result cannot promise exactly-once external shell effects. This
experiment has no reconciliation/unfence endpoint; inspect the outcome and create
a fresh session explicitly instead of retrying effects automatically.

## Parallel assignment contract

The common test driver is ready for per-harness compatibility work. Each agent
owns one harness adapter/configuration and its tests, **not** a new transport.

| Harness | Starting integration | Additional acceptance work |
| --- | --- | --- |
| Pi | `pi-acp` wrapping `pi --mode rpc` | Make session MCP configuration effective (the upstream adapter currently documents that it does not forward it). Install a pinned tool extension and prove an invocation. Report unsupported extension UI/commands. |
| Codex | `codex-acp` wrapping Codex App Server | Pin both versions; test model/approval/sandbox configuration, MCP, and the distinction between native history restoration and interrupted work. |
| Claude | `claude-agent-acp` using Claude Agent SDK | Pin adapter/SDK; test MCP and approval settings. Identify SDK behavior separately from the interactive CLI. |
| Gemini | Gemini CLI ACP mode | Pin release; verify advertised capabilities, MCP, permission mode, and native session-restoration support. |

Self-contained assignment brief (substitute one harness):

> Use `kernel/kernel-images`, branch `hypeship/agent-reconnect-acceptance`, as the
> shared baseline. Read `server/experiments/agenttransport/README.md`. Own only the
> named harness's configuration/wrapper/tests, on a separate branch. Record actual
> binary/package versions and initialization capabilities. Use approved existing
> credentials; if missing, report BLOCKED rather than creating accounts or storing
> credentials. Run the five shared real-harness cases, including a separate
> permission-required configuration. Do not weaken assertions, auto-approve
> permissions in the permission suite, change common wire semantics, or silently
> accept ignored MCP/settings. Fix harness-specific plumbing or report the precise
> incompatibility. Separately validate configuration changes, native session
> history restoration, and (for Pi) one installed extension tool. Keep evidence
> private, scrub logs before sharing, and report each case PASS / FAIL /
> UNSUPPORTED / BLOCKED with evidence paths and operation IDs. SKIP is not PASS.
> Return a PR, configuration instructions, capability matrix, and explicit
> recovery limits. Escalate shared-driver defects instead of forking its protocol.

## Still not a production service

Ready for harness fan-out does **not** mean ready to expose `/agent`:

- The journal and in-memory replay index are unbounded. Retention, compaction,
  expired-cursor recovery, storage quotas, and load/slow-consumer tests remain.
- No endpoint authentication, tenant routing, credential refresh, multi-client
  writer ownership, or hardened package/extension installation is implemented.
- SSE has one-second keepalives and five-second write deadlines, but no remote
  proxy, HTTP/2, half-open network, or suspend/restore validation has been run.
- Durability covers surviving local storage and process kill, not disk/host loss
  or power-loss guarantees on every filesystem. Shared-disk ownership uses a local
  file lock; this is not distributed fencing.
- The ACP driver is intentionally v1-only and single-session. Background work,
  native agent resume, subscription auth UX, richer client capabilities, and
  agent-specific extensions require separate evidence. The fixture's permission
  and MCP implementations are test-only, not complete SDKs.

No real harness is marked supported until its opt-in tests and additional
capability checks have run. The remote ACP binding remains replaceable; this
suite does not claim conformance to the unfinished ACP HTTP transport proposal.

References: [ACP v1 transports](https://agentclientprotocol.com/protocol/v1/transports),
[HTTP/WebSocket proposal](https://agentclientprotocol.com/rfds/streamable-http-websocket-transport),
[session setup](https://agentclientprotocol.com/protocol/v1/session-setup).
