# Process execution and output

`POST /process/exec` waits for completion. `POST /process/spawn` returns a process ID immediately. Both accept `timeout_sec`; null or zero disables the deadline. A negative value is rejected.

Non-TTY commands continuously capture stdout and stderr into temporary files, whether or not a client is streaming. Output capture does not wait for stream readers. TTY output continues to use the WebSocket attach endpoint.

## Limits and retention

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `PROCESS_OUTPUT_DIR` | OS temporary directory | Existing writable directory for output files |
| `PROCESS_OUTPUT_MAX_BYTES` | 67108864 (64 MiB) | Maximum stored output per process |
| `PROCESS_OUTPUT_TOTAL_MAX_BYTES` | 268435456 (256 MiB) | Maximum stored output across running and retained processes |

Limits count stdout, stderr, and record headers. Zero selects the default. The per-process limit must exceed 13 bytes and the aggregate limit must be at least the per-process limit. These limits bound stored file bytes, not filesystem block allocation or the memory needed to serialize an exec response.

Exceeding either limit stops the producing process group and reports `output_limit` with `output_truncated: true`. Already captured output remains available. A storage failure reports `output_error`.

Spawned process status and output remain available for 10 seconds after capture completes. Expiration releases the storage budget and closes active streams, even if a reader has stalled. Exec releases its stored output when its result has been assembled. Output is temporary and does not survive a server restart.

## Streaming and reconnecting

`GET /process/{process_id}/stdout/stream` without a cursor consumes the process's unread output, including output captured before the first connection. Reconnects continue at the shared consumed position and do not repeat a consumed exit event. Concurrent cursorless connections divide the output, as with the original queue. Consumption happens when a record is read for delivery; a disconnect can lose an in-flight record, so use explicit cursors when delivery must be resumable.

Supplying `?offset=0` replays all retained output, then follows new output. Data events include an opaque `offset`, also emitted as the SSE `id`. To resume after a data event, send its cursor as `?offset=...` or the `Last-Event-ID` header. These explicit cursors read independently and do not advance cursorless consumption. When both are supplied, `Last-Event-ID` takes precedence so an `EventSource` opened with `?offset=0` resumes on automatic reconnect. To replay from the beginning again, open a new connection with `?offset=0` and no `Last-Event-ID`. Invalid cursors return 400; expired processes return 404.

An `exit` event follows all captured output and includes `exit_code`, `output_truncated`, and, when execution was stopped by the server, `termination_reason`. Clients must check the terminal event to distinguish complete capture from partial output. Reconnection does not extend retention.

## Timeouts and cancellation

A timeout or exec request cancellation sends SIGKILL to the command's process group. Ordinary child processes in that group are included. A descendant that deliberately creates its own session or process group can survive. After the direct child exits, capture drains for at most one additional second; pipes still held open at that point are closed, remaining processes in the original group are killed, and the result is marked `output_truncated`.

An exec timeout returns HTTP 422 with `code: process_timeout`, `message`, `exit_code`, `duration_ms`, and partial `stdout_b64` / `stderr_b64`. Output quota, capture failure, and cancellation also use 422, with `output_limit`, `output_error`, or `process_canceled`. These execution failures must not be retried automatically because the command may already have performed side effects. An ordinary nonzero command exit remains HTTP 200 with its exit code.

Manual `/kill` requests continue to signal only the requested process, using the specified signal.

Spawn keeps running after its HTTP request ends, but still honors its execution timeout and server shutdown. Status and the stream's exit event report why it stopped.

These are the image server's HTTP contracts. Callers that proxy this API must forward `timeout_sec`, preserve 422 responses and their fields, and forward replay cursors to expose the same behavior.
