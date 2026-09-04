# Agent transport acceptance experiment

This is a runnable proof of a **custom POST + SSE reconnect contract**, not an
ACP HTTP implementation or a production API. Nothing is registered in the image
server, and no agent or extension is installed by this experiment.

## Run

From `server/`:

```sh
go test -race -count=100 ./experiments/agenttransport
go vet ./experiments/agenttransport
```

Tests use actual loopback HTTP/TCP connections, not only recorder-based handler
calls. A deterministic runner emits output around a synchronization barrier and
counts executions. No provider credentials or model calls are required.

## What this establishes

| ID | Executable test | Assertion |
| --- | --- | --- |
| T01 | `TestDisconnectRetryAndReplay` | Disconnect mid-turn, retry the prompt, reconnect from the last applied event: one execution, ordered missing output, terminal completion. Retry after completion remains harmless. |
| T02 | `TestLostSubmitResponseDoesNotRepeatExecution` | Send a prompt over TCP, discard its HTTP acknowledgement, close the connection, retry: one execution and complete replay. |
| T03 | `TestConcurrentRetriesAndPayloadConflict` | Twenty concurrent submissions with one ID execute once; the same ID with a different prompt returns 409. |
| T04 | `TestInvalidReplayCursor` | Negative/malformed cursors return 400; a cursor ahead of the log returns 409 instead of silently losing data. |

The experiment validates **client connection loss while the runtime remains
alive**. It does not validate restart durability, real ACP adapters, remote
proxies, image suspension, authentication, or loss of the underlying machine.
It does not benchmark SSE against WebSocket; both can implement this contract.

## Contract under test

A reference runtime represents exactly one session. Its lifetime is independent
of HTTP requests. The test shuts it down explicitly.

- `POST /commands`: `{ "id": "client-generated-stable-id", "prompt": "..." }`.
  Returns 202 with `{ "id": "...", "state": "running|completed|failed" }`.
  Reuse an ID only for an identical command. The reference retains IDs for its
  entire lifetime. Operation identity is separate from an ACP JSON-RPC request ID.
- `GET /events`: SSE with `Last-Event-ID` set to the last **applied** sequence
  number (0 starts from the beginning). Each event has an SSE `id` matching its
  JSON `sequence`, plus `operationId`, `kind`, and optional `text`.
- Replay is exclusive of the supplied sequence. The log is the source for both
  replay and live delivery; subscription registration and log snapshots are
  synchronized so an event cannot disappear between replay and live delivery.
- `accepted` precedes execution. Exactly one terminal event follows a runner
  invocation. A broken HTTP response or stream does not cancel the invocation.
- The receiver must apply events in order and ignore already-applied sequences.
  Updating a durable cursor before applying an event is unsafe.

The runner boundary is intentionally smaller than ACP. A production adapter must
translate ACP prompt/update/completion messages into the session service, while
retaining the original ACP payloads rather than flattening them to text as this
fixture does. ACP v1 prompt responses mean completion, not immediate acceptance;
the service must track submission separately without misreporting agent acceptance.

## Required gates before shipping

Unchecked gates are **not implemented**, not implicitly passing.

### Transport/session service (shared by all harnesses)

| ID | Pending acceptance case | Required result |
| --- | --- | --- |
| T05 | Reconnect repeatedly during high-volume output; slow consumer; replay cursor expires | No silent gaps or unbounded memory growth; explicit expired-cursor response and snapshot/reconciliation path. |
| T06 | Disconnect while permission is pending; retry its answer; answer from two clients | Same unresolved request on reconnect; one decision applied; conflicting answers rejected; never auto-approve. |
| T07 | Cancel during disconnection; lose cancel acknowledgement; retry | Cancellation has a stable operation ID and observable outcome; disconnect alone never cancels. |
| T08 | Restart service at each command acceptance/dispatch/result boundary | Persist before acknowledging; completed operations deduplicate; ambiguous dispatch is reported, never blindly retried. |
| T09 | Restart during event append/delivery; reconnect with stale cursor | Durable ordered replay or explicit recovery failure; no claim that conversation history is a complete event log. |
| T10 | Expired credentials, another session's IDs/cursors, superseded writer | Reauthenticate and authorize every attach/command; no cross-session data or competing command ownership. |
| T11 | Half-open connection, proxy timeout, suspend/resume | Bounded detection and backoff; keepalive; attach to the same live runtime without resetting ACP. |

T08 cannot promise exactly-once arbitrary shell effects. There is a crash window
between handing a command to an external agent and durably recording that fact.
The service must reconcile using agent evidence or expose an uncertain outcome.
A deduplication map alone does not close that window.

### Harness compatibility (run separately for every pinned harness version)

| ID | Case | Required result |
| --- | --- | --- |
| H01 | Initialize, create session, prompt, output/tool updates, completion | Correct ACP capabilities and event mapping; original payloads retained. |
| H02 | Repeat T01/T02 with a real harness and an instrumented tool | One adapter dispatch; record tool execution counts and ordered replay. Control tool retries explicitly so model behavior is not confused with transport duplication. |
| H03 | Detach/reattach during tool execution and permission wait | Same live agent process and ACP conversation; no new prompt or hidden auto-approval. |
| H04 | Agent crash and session restoration | Clear distinction between history restoration and continuation of in-flight work; unsupported recovery is explicit. |
| H05 | MCP tool, configuration, and supported extensions | An actual tool invocation works; unsupported settings are rejected rather than silently ignored. |

## Bounded harness assignments

Each assignment should implement an adapter and run the shared tests, not invent
another network transport, cursor format, or retry policy. Do not mark a harness
supported based only on the deterministic runner passing.

| Harness | Starting point | Additional requirement |
| --- | --- | --- |
| Pi | `pi-acp` wrapping `pi --mode rpc` | Verify MCP configuration is actually forwarded; install a pinned tool extension and invoke it. Report extension UI/command limitations explicitly. |
| Codex | `codex-acp` wrapping Codex App Server | Verify approvals, sandbox settings, MCP, and native session recovery with pinned adapter/CLI versions. |
| Claude | `claude-agent-acp` using Claude Agent SDK | Verify permissions, MCP, and recovery; identify SDK behavior separately from interactive CLI behavior. |
| Gemini | Gemini CLI ACP mode | Verify advertised capabilities, permission handling, MCP, and session restoration against the pinned release. |

Common assignment brief:

> Implement only the named harness adapter against the shared session-service
> contract. Start by recording exact harness/adapter versions and initialization
> capabilities. Use a temporary workspace and scoped test credentials. Keep ACP
> connected locally while the network client is detached. Run H01-H05 and the
> shared transport tests; report each as PASS, FAIL, or UNSUPPORTED with logs and
> operation/event IDs. Do not log credentials, run destructive commands, or
> silently convert unsupported behavior into success. Deliver tests, adapter
> code, capability matrix, and recovery limitations. Do not change the shared
> protocol without raising the mismatch first.

Before dispatching those assignments, implement the shared session service and
its test-driver boundary for controlled tool barriers, permission decisions,
process identity, and crash injection. The current runner fixture is not that
real-agent driver yet.

## Deliberate prototype limits

The reference uses an unbounded in-memory log and operation map, accepts only
text, and has no authentication, session routing, heartbeat, write deadline,
permission flow, cancellation API, or admission limit. **Do not expose it to
untrusted callers or register it in the production router.** Shutdown assumes
runners honor context cancellation. These limits keep the experiment focused on
network loss rather than presenting an incomplete service as production-ready.

## Protocol references

- [ACP v1 transports](https://agentclientprotocol.com/protocol/v1/transports)
- [HTTP/WebSocket proposal](https://agentclientprotocol.com/rfds/streamable-http-websocket-transport)
- [ACP v1 session setup](https://agentclientprotocol.com/protocol/v1/session-setup)

The remote transport proposal defers event replay and resumability; this
experiment does not claim conformance to that draft. Keep the network binding
versioned and separate from ACP adapter behavior.
