# sysmon

VM-internal failure telemetry for the kernel-images browser VM. Surfaces two event types onto the existing `EventStream` → SSE / S2 pipeline:

| Event type         | Source                                     | Owned by              |
| ------------------ | ------------------------------------------ | --------------------- |
| `system_oom_kill`  | Linux kernel OOM-killer via `/dev/kmsg`    | `lib/sysmon` (in-process goroutine) |
| `service_crashed`  | supervisord eventlistener protocol         | `cmd/supervisord-shim` (separate binary, POSTs to `/telemetry/events`) |

Both paths funnel through the active `TelemetrySession`, which gates on the live telemetry config before forwarding to `events.EventStream`. Events fired while no telemetry session is active (or whose category is disabled) are dropped before reaching the stream.

## Why two binaries

| Concern | sysmon (in-process) | supervisord-shim (separate process) |
| --- | --- | --- |
| Why separate | n/a | supervisord's eventlistener protocol requires a separate process talking over stdin/stdout |
| Triggers | kernel OOM-killer writes to `/dev/kmsg` | supervised service exits unexpectedly or FATALs |
| Transport | in-process `PublishFunc` wired to `TelemetrySession.Publish` | `POST /telemetry/events` over localhost HTTP |
| Failure mode | open of `/dev/kmsg` may fail (no CAP_SYSLOG); API logs and continues without OOM telemetry | API may be down during shim's POST (shim logs and always ACKs supervisord; event lost); or API returns `204` because telemetry is unconfigured / `service_crashed` is filtered (shim treats as success; event dropped on purpose) |

## Event taxonomy

### `system_oom_kill`

Parsed from one kernel OOM dump in `/dev/kmsg`. Payload (see `BrowserSystemOomKillEventData` in `openapi.yaml` for the authoritative schema):

| Field | Meaning | Absent when |
| --- | --- | --- |
| `process_name` | comm of the killed process (max 15 chars, kernel TASK_COMM_LEN limit) | never |
| `pid` | PID of the killed process | never |
| `rss_kb` | sum of anon-rss + file-rss + shmem-rss in KiB | never |
| `constraint` | `none` / `memcg` / `cpuset` / `memory_policy` | pre-Linux-5.0 kernels (no structured `oom-kill:` line) |
| `mem_total_kb` | total RAM from `N pages RAM` × 4 KiB | kernel did not emit Mem-Info (e.g. memcg OOM) |
| `mem_free_kb` | free RAM from `free:N free_pcp:N` × 4 KiB | as above |
| `top_tasks` | up to 5 processes from `Tasks state` table, sorted by RSS desc | kernel did not emit the table |
| `trigger_process_name` | comm of the process whose allocation triggered the OOM-killer | sysrq-triggered OOMs (no opener line) |
| `trigger_pid` | PID of the trigger | as above; pre-CPU/PID header kernels |

### `service_crashed`

Mapped from supervisord `PROCESS_STATE_EXITED` (with `expected=0`) or `PROCESS_STATE_FATAL`. Schema in `BrowserServiceCrashedEventData`:

| Field | Meaning |
| --- | --- |
| `service_name` | supervisord program name (e.g. `chromium`, `mutter`, `kernel-images-api`) |
| `pid` | live PID at exit (omitted for `gave_up` since supervisord no longer tracks one) |
| `phase` | `startup` (died during STARTING) / `running` (crashed after reaching RUNNING) / `gave_up` (FATAL via exhausted startretries) |

Clean stops (`supervisorctl stop`, exit codes in the configured `exitcodes` list) do **not** produce events — supervisord marks them `expected=1` and the shim skips them.

## File layout

| File | Concern |
| --- | --- |
| `sysmon.go` | `Monitor` lifecycle (Start/Wait), goroutine wiring, `publishOomKill` |
| `kmsg.go` | OOM-dump text parser (regex + state machine) — see file header for format compatibility notes |
| `kmsg_linux.go` | Linux-only `/dev/kmsg` open via `euank/go-kmsg-parser`, SeekEnd on start so we don't replay history on API restart |
| `kmsg_other.go` | non-Linux stub so dev machines still compile |
| `kmsg_test.go` | parser fixtures + tests (both pre-5.14 and post-5.14 Tasks-state layouts) |
| `sysmon_test.go` | end-to-end test from stub kmsg source through EventStream |

The supervisord-shim lives at `cmd/supervisord-shim/`. Its configuration is duplicated as `supervisord-shim.conf` under both `images/chromium-headless/image/supervisor/services/` and `images/chromium-headful/supervisor/services/`.

## How to verify locally (Docker)

These steps reproduce the smoke matrix from PR #254. Container image is built with `cd images/chromium-headless && ./build-docker.sh`.

> **Heads up:** every test below assumes telemetry is configured. `TelemetrySession.Publish` drops every event when no session is active, so without the `PUT /telemetry` step you'll see nothing on the SSE stream and silently get false negatives. The shared setup block does this once.

```bash
# Start the container detached (the script's run-docker.sh hardcodes -it).
docker run -d --rm --name chromium-headless-test \
  --platform linux/amd64 --privileged --tmpfs /dev/shm:size=128m \
  -p 9222:9222 -p 444:10001 \
  onkernel/chromium-headless-test:latest

# Wait for the API.
sleep 10 && curl -sf http://localhost:444/spec.json >/dev/null && echo "API up"

# Configure telemetry so a session is active. The empty body captures the
# default set (every category except screenshot), which includes system, so
# OOM/crash events flow. (Setting all browser categories to enabled:false is
# treated as "clear the configuration" and tears the session down — don't do
# that here.)
curl -sf -X PUT http://localhost:444/telemetry \
  -H 'content-type: application/json' -d '{}'

# Open the SSE stream in another shell to watch events in real time.
curl -sN http://localhost:444/telemetry/stream
```

### service_crashed (phase=running)

```bash
# Kill the chromium browser process the launcher actually spawned.
docker exec chromium-headless-test supervisorctl signal KILL chromium
# Expect one service_crashed event with phase=running.
```

### service_crashed (phase=gave_up)

```bash
# Install a deliberately failing service.
docker exec chromium-headless-test bash -c 'cat > /etc/supervisor/conf.d/services/flaky.conf <<EOF
[program:flaky]
command=/bin/sh -c "echo flaky starting; sleep 0.1; exit 1"
autostart=false
autorestart=true
startsecs=1
startretries=3
stdout_logfile=/var/log/supervisord/flaky
redirect_stderr=true
EOF
supervisorctl reread && supervisorctl update'

docker exec chromium-headless-test supervisorctl start flaky
# Wait ~15 s for supervisord to exhaust the 3 retries and transition to FATAL.
# Expect one service_crashed event with phase=gave_up and no pid.
```

### Clean stop is suppressed (negative test)

```bash
docker exec chromium-headless-test supervisorctl stop chromium
# Expect NO new SSE event (only the 15 s keepalive ":" frame).
```

### system_oom_kill (synthetic kmsg injection)

```bash
docker exec chromium-headless-test bash -c '
for line in \
  "chromium invoked oom-killer: gfp_mask=0x100cca, order=0, oom_score_adj=0" \
  "CPU: 2 PID: 9999 Comm: chromium Not tainted 5.15.0-1-amd64 #1" \
  "Mem-Info:" \
  " free:4560 free_pcp:0 free_cma:0" \
  "524288 pages RAM" \
  "Tasks state (memory values in pages):" \
  "[   1234]  1000  1234  1308611  1205975  1205675   200   100   9678848   0   0 chromium" \
  "oom-kill:constraint=CONSTRAINT_NONE,task=chromium,pid=1234,uid=1000" \
  "Out of memory: Killed process 1234 (chromium) total-vm:5234572kB, anon-rss:4823900kB, file-rss:100kB, shmem-rss:200kB, UID:1000 pgtables:9678848kB oom_score_adj:0"
do
  echo "<6>$line" > /dev/kmsg
done'
# Expect one system_oom_kill event with constraint=none, mem_total_kb=2097152,
# top_tasks[0].name="chromium", trigger_process_name="chromium".
```

### system_oom_kill (real cgroup OOM)

```bash
docker rm -f chromium-headless-test
# 512 MB cap keeps the API itself alive while letting Chrome OOM.
docker run -d --rm --name chromium-headless-test \
  --platform linux/amd64 --privileged --tmpfs /dev/shm:size=128m \
  --memory 512m --memory-swap 512m \
  -p 9222:9222 -p 444:10001 \
  onkernel/chromium-headless-test:latest

# Re-apply the telemetry PUT — the respawn dropped the previous session.
sleep 10 && curl -sf -X PUT http://localhost:444/telemetry \
  -H 'content-type: application/json' -d '{}'

# Run a memory hog inside.
docker exec chromium-headless-test python3 -c '
import sys, time
chunks=[]
while True:
    chunks.append(b"x"*(60*1024*1024)); sys.stdout.write(f"{len(chunks)*60}MB\n"); sys.stdout.flush(); time.sleep(0.3)
'
# Expect system_oom_kill events with constraint=memcg, and mem_total_kb /
# mem_free_kb omitted (the kernel skips the global Mem-Info dump on memcg
# OOMs). Sanity-check top_tasks names: they should be single tokens
# (`chromium`, `python3`). If they include numbers or extra columns, the
# Tasks-state regex in kmsg.go needs updating for the current kernel.
```

## How to verify in production (real Linux 6.x VM)

This procedure asserts on sysmon's internal log lines, which fire regardless of telemetry config. To also see the event reach a downstream consumer, configure telemetry on the session first (same `PUT /telemetry` body as the local-Docker setup, against the session's metro-api telemetry endpoint).

```bash
# Spin up a browser session.
kernel browsers create
# Note the session ID, then exec into the VM.

# Confirm sysmon is running.
kernel browsers process exec <session_id> -- /bin/bash -c \
  'tail -50 /var/log/supervisord/kernel-images-api | grep sysmon'
# Look for: "sysmon: kmsg OOM reader started"

# Trigger an OOM (kills the highest-oom_score process; expect chromium).
kernel browsers process exec <session_id> -- /bin/bash -c \
  'echo 1 > /proc/sys/kernel/sysrq; echo f > /proc/sysrq-trigger'

# Verify sysmon parsed and emitted the event.
kernel browsers process exec <session_id> -- /bin/bash -c \
  'tail -50 /var/log/supervisord/kernel-images-api | grep "sysmon: emitted system_oom_kill"'

# Clean up.
kernel browsers delete <session_id>
```

## Known limitations

1. **API self-crash is invisible to sysmon.** If `kernel-images-api` itself dies, the shim's POST fails (connection refused) and that event is lost. The host platform's process/VM-level monitoring is the layer that catches it. Closing the gap inside this binary would require persistent shim-side buffering and is out of scope.
2. **`process_name` is truncated to 15 chars.** This is the kernel's `TASK_COMM_LEN-1` limit, not a parser bug. `kernel-images-api` shows up as `kernel-images-a`.
3. **Page size is hard-coded to 4 KiB.** Correct on x86_64; would be wrong on ARM 16K/64K page kernels.
4. **`mem_total_kb` / `mem_free_kb` are omitted on memcg OOMs.** The kernel does not emit the global `pages RAM` / `free:N` lines when the OOM is cgroup-scoped. This is correct behavior, documented in the openapi schema.
5. **`trigger_*` fields are absent on sysrq-triggered OOMs.** The `X invoked oom-killer:` opener line is only emitted on allocation-driven kills. Real allocation OOMs always populate these fields; only the synthetic `sysrq f` test path doesn't.
6. **No de-dup between kmsg and supervisord.** If a Chrome OOM both fires kmsg (`system_oom_kill`) and causes supervisord to notice the exit (`service_crashed`), both events fire. The overlap is itself a useful signal (RAM exhaustion vs. process bug).

## Where to look when things break

| Symptom | First place to check |
| --- | --- |
| No `system_oom_kill` events in prod | API logs for `sysmon: kmsg OOM monitor disabled` — indicates `/dev/kmsg` open failed |
| `system_oom_kill` events have corrupt `top_tasks` names | `oomTaskEntryRe` in `kmsg.go` — kernel changed the Tasks-state column layout again |
| `system_oom_kill` events missing fields after a kernel upgrade | each `oom*Re` regex in `kmsg.go` — sections may have been renamed in the kernel |
| No `service_crashed` events | `cat /var/log/supervisord/supervisord-shim` inside the container; check for `connection refused` to the API |
| Shim looping (supervisord shows repeated spawn) | the shim should never enter FATAL because `startretries=999999`; if it does, check `/var/log/supervisord.log` for spawn errors |
| sysmon log says "emitted system_oom_kill" but no SSE event | check `GET /telemetry` — without an active session both producers are dropped at `TelemetrySession.Publish`. Issue a `PUT /telemetry` and re-trigger. |
| Events admitted by `TelemetrySession` but don't reach downstream consumers | check the SSE / S2 pipeline (`POST /telemetry/events` → `TelemetrySession.Publish` → `EventStream.Publish` → SSE / S2) — that's `lib/events` territory, not sysmon |
