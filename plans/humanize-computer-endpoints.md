# Humanize All Computer Interaction Endpoints

> Add human-like behavior to all computer interaction API endpoints using fast, pre-computed algorithms that add zero additional xdotool process spawns.

## Performance-First Design Principle

**The bottleneck is xdotool process spawns** (fork+exec per call), not Go-side computation. Every algorithm below is designed around two rules:

1. **One xdotool call per API request** -- pre-compute all timing in Go and bake it into a single chained xdotool command with inline `sleep` directives. This is the same pattern already used by `doDragMouse` (see lines 911-951 of `computer.go`).
2. **O(1) or O(n) math only** -- uniform random (`rand.Intn`), simple easing polynomials (2-3 multiplies), no lookup tables, no transcendental functions beyond what `mousetrajectory.go` already uses.

```mermaid
flowchart LR
    Go["Go: pre-compute timing array O(n)"] --> Args["Build xdotool arg slice"]
    Args --> OneExec["Single fork+exec"]
    OneExec --> Done["Done"]
```



### Existing proof this works

`doDragMouse` already chains `mousemove_relative dx dy sleep 0.050 mousemove_relative dx dy sleep 0.050 ...` in a single xdotool invocation. Every strategy below follows this exact pattern.

---

## 0. Move Mouse -- Bezier Curve Trajectory (Already Implemented)

**Status:** Complete. This is the reference implementation that all other endpoints follow.

**Cost:** N xdotool calls (one `mousemove_relative` per trajectory point) with Go-side sleeps. Typically 5-80 steps depending on distance.

**Algorithm:** Bezier curve with randomized control points, distortion, and easing. Ported from Camoufox/HumanCursor.

- **Bezier curve**: 2 random internal knots within an 80px-padded bounding box around start/end. Bernstein polynomial evaluation produces smooth curved path. O(n) computation.
- **Distortion**: 50% chance per interior point to apply Gaussian jitter (mean=1, stdev=1 via Box-Muller transform). Adds micro-imperfections.
- **Easing**: `easeOutQuad(t) = -t*(t-2)` -- cursor decelerates as it approaches the target, matching natural human behavior.
- **Point count**: Auto-computed from path length (`pathLength^0.25 * 20`), clamped to [5, 80]. Override via `Options.MaxPoints`.
- **Per-step timing**: ~10ms default step delay with +/-2ms uniform jitter. When `duration_ms` is specified, delay is computed as `duration_ms / numSteps`.
- **Screen clamping**: Trajectory points clamped to screen bounds to prevent X11 delta accumulation errors.

**Key files:**

- `[server/lib/mousetrajectory/mousetrajectory.go](kernel-images/server/lib/mousetrajectory/mousetrajectory.go)` -- Bezier curve generation (~230 lines)
- `[server/cmd/api/api/computer.go](kernel-images/server/cmd/api/api/computer.go)` lines 104-206 -- `doMoveMouseSmooth` integration

**API (existing):** `MoveMouseRequest` has `smooth: boolean` (default `true`) and optional `duration_ms` (50-5000ms).

**Implementation in `doMoveMouseSmooth`:**

1. Get current mouse position via `xdotool getmouselocation`
2. Generate Bezier trajectory: `mousetrajectory.NewHumanizeMouseTrajectoryWithOptions(fromX, fromY, toX, toY, opts)`
3. Clamp points to screen bounds
4. For each point: `xdotool mousemove_relative -- dx dy`, then `sleepWithContext` with jittered delay
5. Modifier keys held via `keydown`/`keyup` wrapper

**Note:** This endpoint uses per-step Go-side sleeps (not xdotool inline `sleep`) because the trajectory includes screen-clamping logic that adjusts deltas at runtime. The other endpoints below use inline `sleep` since their timing can be fully pre-computed.

---

## Shared Library: `server/lib/humanize/humanize.go`

Tiny utility package (no external deps, no data structures) providing:

```go
// UniformJitter returns a random duration in [base-jitter, base+jitter], clamped to min.
func UniformJitter(rng *rand.Rand, baseMs, jitterMs, minMs int) time.Duration

// EaseOutQuad computes t*(2-t) for t in [0,1]. Two multiplies.
func EaseOutQuad(t float64) float64

// SmoothStepDelay maps position i/n through a smoothstep curve to produce
// a delay in [fastMs, slowMs]. Used for scroll and drag easing.
// smoothstep(t) = 3t^2 - 2t^3. Three multiplies.
func SmoothStepDelay(i, n, slowMs, fastMs int) time.Duration

// FormatSleepArg formats a duration as a string suitable for xdotool's
// inline sleep command (e.g. "0.085"). Avoids fmt.Sprintf per call.
func FormatSleepArg(d time.Duration) string
```

All functions are pure, allocate nothing, and cost a few arithmetic ops each. Tested with table-driven tests and deterministic seeds.

---

## 1. Click Mouse -- Single-Call Down/Sleep/Up

**Cost:** 1 xdotool call (same as current). Pre-computation: 1-2 `rand.Intn` calls.

**Algorithm:** Replace `click` with `mousedown <btn> sleep <dwell> mouseup <btn>` in the same xdotool arg slice. No separate process spawns.

- **Dwell time**: `UniformJitter(rng, 90, 30, 50)` -> range [60, 120]ms. This matches measured human click dwell without needing lognormal sampling.
- **Micro-drift**: Append `mousemove_relative <dx> <dy>` between mousedown and mouseup, where dx/dy are `rand.Intn(3)-1` (range [-1, 1] pixels). Trivially cheap.
- **Multi-click**: For `num_clicks > 1`, loop and insert inter-click gaps via `UniformJitter(rng, 100, 30, 60)` -> [70, 130]ms.

**Single xdotool call example:**

```
xdotool mousemove 500 300 mousedown 1 sleep 0.085 mousemove_relative -- 1 0 mouseup 1
```

**API change:** Add `smooth: boolean` (default `true`) to `ClickMouseRequest`.

---

## 2. Type Text -- Chunked Type with Inter-Word Pauses

**Cost:** 1 xdotool call (same as current). Pre-computation: O(words) random samples.

**Algorithm:** Instead of per-character keysym mapping (which is complex and fragile for Unicode), split text by whitespace/punctuation into chunks and chain `xdotool type --delay <intra> "chunk" sleep <inter>` commands.

- **Intra-word delay**: Per-chunk, pick `rand.Intn(70) + 50` -> [50, 120]ms. Varies per chunk to simulate burst-pause rhythm.
- **Inter-word pause**: Between chunks, insert `sleep` with `UniformJitter(rng, 140, 60, 60)` -> [80, 200]ms. Longer pauses at sentence boundaries (after `.!?`): multiply by 1.5x.
- **No bigram tables**: The per-word delay variation is sufficient for convincing humanization. Bigram-level precision adds complexity with diminishing returns for bot detection evasion.

**Single xdotool call example:**

```
xdotool type --delay 80 -- "Hello" sleep 0.150 type --delay 65 -- " world" sleep 0.300 type --delay 95 -- ". How" sleep 0.120 type --delay 70 -- " are" sleep 0.140 type --delay 85 -- " you?"
```

**API change:** Add `smooth: boolean` (default `false`) to `TypeTextRequest`. When `smooth=true`, the existing `delay` field is ignored.

**Why this is fast:** We never leave the `xdotool type` mechanism (which handles Unicode, XKB keymaps, etc. internally). We just break it into chunks with sleeps between them. One fork+exec total.

---

## 3. Press Key -- Dwell via Inline Sleep

**Cost:** 1 xdotool call (same as current). Pre-computation: 1 `rand.Intn` call.

**Algorithm:** Replace `key <keysym>` with `keydown <keysym> sleep <dwell> keyup <keysym>`.

- **Tap dwell**: `UniformJitter(rng, 95, 30, 50)` -> [65, 125]ms.
- **Modifier stagger**: When `hold_keys` are present, insert a small `sleep 0.025` between each `keydown` for modifiers, then the primary key sequence. Release in reverse order with the same stagger. This costs zero extra xdotool calls -- it's all in the same arg slice.

**Single xdotool call example (Ctrl+C):**

```
xdotool keydown ctrl sleep 0.030 keydown c sleep 0.095 keyup c sleep 0.025 keyup ctrl
```

**API change:** Add `smooth: boolean` (default `false`) to `PressKeyRequest`.

---

## 4. Scroll -- Eased Tick Intervals in One Call

**Cost:** 1 xdotool call (same as current). Pre-computation: O(ticks) easing function evaluations (3 multiplies each).

**Algorithm:** Replace `click --repeat N --delay 0 <btn>` with N individual `click <btn>` commands separated by pre-computed `sleep` values following a **smoothstep easing curve**.

- **Easing**: `SmoothStepDelay(i, N, slowMs=80, fastMs=15)` for each tick i. The smoothstep `3t^2 - 2t^3` creates natural momentum: slow start, fast middle, slow end.
- **Jitter**: Add `rand.Intn(10) - 5` ms to each delay. Trivially cheap.
- **Small scrolls (1-3 ticks)**: Skip easing, use uniform delay of `rand.Intn(40) + 30` ms.

**Single xdotool call example (5 ticks down):**

```
xdotool mousemove 500 300 click 5 sleep 0.075 click 5 sleep 0.035 click 5 sleep 0.018 click 5 sleep 0.040 click 5
```

**API change:** Add `smooth: boolean` (default `false`) to `ScrollRequest`.

**Why not per-tick Go-side sleeps?** That would require N separate xdotool calls (N fork+execs). Inline `sleep` achieves the same timing in one process.

---

## 5. Drag Mouse -- Bezier Path + Eased Delays

**Cost:** Same as current (1-3 xdotool calls for the 3 phases). Pre-computation: Bezier generation (already proven fast in `mousetrajectory.go`).

**Algorithm:** When `smooth=true`, auto-generate the drag path using the existing `mousetrajectory.HumanizeMouseTrajectory` Bezier library, then apply eased step delays (instead of the current fixed `step_delay_ms`).

- **Path generation**: `mousetrajectory.NewHumanizeMouseTrajectoryWithOptions(startX, startY, endX, endY, opts)` -- already O(n) with Bernstein polynomial evaluation. Proven fast.
- **Eased step delays**: Replace the fixed `stepDelaySeconds` in the Phase 2 xdotool chain with per-step delays from `SmoothStepDelay`. Slow at start (pickup) and end (placement), fast in middle. These are already baked into the single xdotool arg slice, so zero extra process spawns.
- **Jitter**: Same `rand.Intn(5) - 2` ms pattern already used by `doMoveMouseSmooth`.

**API change:** Add `smooth: boolean` (default `false`) to `DragMouseRequest`. When `smooth=true` and `path` has exactly 2 points (start + end), the server generates a Bezier curve between them and replaces `path` with the generated waypoints.

**No new `start`/`end` fields needed** -- the caller simply provides `path: [[startX, startY], [endX, endY]]` and the server expands it.

---

## Computational Cost Summary


| Endpoint      | xdotool calls    | Pre-computation                   | Algorithm                            |
| ------------- | ---------------- | --------------------------------- | ------------------------------------ |
| `move_mouse`  | O(points) (done) | O(points) Bezier + Box-Muller     | Bezier curve + easeOutQuad + jitter  |
| `click_mouse` | 1 (same)         | 1-2x `rand.Intn`                  | Uniform random dwell                 |
| `type_text`   | 1 (same)         | O(words) `rand.Intn`              | Chunked type + inter-word sleep      |
| `press_key`   | 1 (same)         | 1x `rand.Intn`                    | Inline keydown/sleep/keyup           |
| `scroll`      | 1 (same)         | O(ticks) smoothstep (3 muls each) | Eased inter-tick sleep               |
| `drag_mouse`  | 1-3 (same)       | O(points) Bezier (existing)       | Bezier path + smoothstep step delays |


No additional process spawns. No heap allocations beyond the existing xdotool arg slice. No lookup tables. Every random sample is a single `rand.Intn` or `rand.Float64` call.

---

## Files to Create/Modify

- **Modify:** `[server/openapi.yaml](kernel-images/server/openapi.yaml)` -- Add `smooth` boolean to 5 request schemas
- **Modify:** `[server/cmd/api/api/computer.go](kernel-images/server/cmd/api/api/computer.go)` -- Add humanized code paths (branching on `smooth` flag)
- **Create:** `server/lib/humanize/humanize.go` -- Shared primitives (~50 lines)
- **Create:** `server/lib/humanize/humanize_test.go` -- Table-driven tests
- **Regenerate:** OpenAPI-generated types (run code generation after schema changes)

No separate per-endpoint library packages needed. The shared `humanize` package plus the existing `mousetrajectory` package cover everything.