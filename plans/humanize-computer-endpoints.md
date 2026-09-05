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

**Cost:** O(words) xdotool calls with Go-side sleeps. Pre-computation: O(words) random samples.

**Algorithm:** Split text into word chunks (keeping trailing whitespace/punctuation with each chunk), then issue one `xdotool type --delay <intra> -- "<chunk>"` call per chunk with Go-side `sleepWithContext` pauses between them. This follows the same pattern as `doMoveMouseSmooth` (O(n) calls with Go-side sleeps).

- **Chunking**: Split at word boundaries, keeping trailing delimiters (spaces, punctuation) attached to the preceding chunk. `"Hello world. How are you?"` becomes `["Hello ", "world. ", "How ", "are ", "you?"]`. This ensures pauses happen *after* typing the delimiter, matching natural rhythm.
- **Intra-word delay**: Per-chunk, pick `rand.Intn(70) + 50` -> [50, 120]ms. Varies per chunk to simulate burst-pause rhythm.
- **Inter-word pause**: Between chunks, Go-side sleep with `UniformJitter(rng, 140, 60, 60)` -> [80, 200]ms. Longer pauses at sentence boundaries (chunk ends with `.!?`): multiply by 1.5x.
- **No bigram tables**: The per-word delay variation is sufficient for convincing humanization. Bigram-level precision adds complexity with diminishing returns for bot detection evasion.

**Execution sequence example (`"Hello world. How are you?"`):**

```
xdotool type --delay 80 -- "Hello "        # fork+exec 1
  [Go sleep 150ms]
xdotool type --delay 65 -- "world. "       # fork+exec 2
  [Go sleep 300ms]                         # sentence boundary: 1.5x pause
xdotool type --delay 95 -- "How "          # fork+exec 3
  [Go sleep 120ms]
xdotool type --delay 70 -- "are "          # fork+exec 4
  [Go sleep 140ms]
xdotool type --delay 85 -- "you?"          # fork+exec 5
```

**API change:** Add `smooth: boolean` (default `false`) to `TypeTextRequest`. When `smooth=true`, the existing `delay` field is ignored.

**Why O(words) calls, not 1?** `xdotool type` consumes the rest of argv as text to type, so it cannot be chained with `sleep` or other commands in a single invocation. O(words) fork+execs (typically 5-15 for a sentence) is acceptable -- the inter-word pauses (80-300ms) dwarf the ~1-2ms fork+exec overhead.

### 2a. Typo Injection (Optional)

Optionally introduce realistic typos that are immediately corrected with backspace, simulating natural human typing errors.

**Research basis:** The [136 million keystrokes study (Aalto/CHI 2018)](https://userinterfaces.aalto.fi/136Mkeystrokes/) and [Logan (1999)](https://www.unm.edu/~quadl/empiricaldata/Typographical-Errors-by-E.P.) show average typists produce errors at ~2-5% of keystrokes. The dominant error type (~40%) is adjacent-key substitution on the QWERTY layout.

**API change:** Add `typo_chance: number` (0.0-0.10, default 0) to `TypeTextRequest`. Requires `smooth=true`. Typical human range is 0.02-0.05.

**Typo position selection -- geometric gap, O(typos) not O(chars):**

Instead of per-character Bernoulli trials (expensive), compute gaps between typo positions directly. For a 3% rate the average gap is ~33 characters. One `rng.Intn` per typo:

```go
avgGap := int(1.0 / typoRate) // e.g., 33 for 3%
pos := avgGap/2 + rng.Intn(avgGap)  // first typo position
for pos < len(runes) {
    typoPositions = append(typoPositions, pos)
    pos += avgGap/2 + rng.Intn(avgGap) // next gap: [avgGap/2, avgGap*3/2)
}
```

For a 200-char text at 3%: ~6 typos = ~6 `rng.Intn` calls for positions + ~6 for type selection = **12 total random calls** vs 200 with per-character Bernoulli. No `math.Log` or transcendental functions.

**Typo type selection (one `rng.Intn(100)` per typo):**

| Roll | Type | Weight | Mechanism |
|---|---|---|---|
| 0-59 | Adjacent key | 60% | Look up QWERTY neighbor from static `[26][]byte` array, O(1) |
| 60-79 | Doubling | 20% | Type the character twice |
| 80-94 | Transposition | 15% | Swap current and next character |
| 95-99 | Extra character | 5% | Insert a random adjacent key before the correct one |

**Correction sequence:** At a typo position, the chunk is split and the correction is injected:

```
xdotool type --delay 80 -- "hel"      # type up to typo point
xdotool type --delay 80 -- "p"        # type wrong char (adjacent to 'l')
  [Go sleep 350ms]                    # "oh no" realization pause
xdotool key BackSpace                 # delete wrong char
  [Go sleep 80ms]                     # brief recovery
xdotool type --delay 80 -- "lo "      # continue with correct text
```

**Correction timing:**
- Realization pause before backspace: `UniformJitter(rng, 350, 150, 150)` -> [200, 500]ms
- Backspace is rapid: no extra delay
- Recovery pause after correction: `UniformJitter(rng, 80, 30, 40)` -> [50, 110]ms

**QWERTY adjacency data:** Static array, ~26 entries, initialized at package level. No maps, no heap allocation:

```go
var qwertyAdj = [26][]byte{
    {'q', 'w', 's', 'z'},       // a
    {'v', 'g', 'h', 'n'},       // b
    {'x', 'd', 'f', 'v'},       // c
    // ... etc
}
```

**Extra xdotool calls per typo:** ~3-4 (type wrong, backspace, type correct). At 3% rate on a 50-char sentence, that's ~1-2 typos adding ~4-8 extra calls. The realization pauses (200-500ms) dominate, so fork+exec overhead is negligible.

**Cost summary:** O(typos) random calls + O(typos) extra xdotool calls. No per-character work. For typical text, typos add 0-3 extra correction sequences.

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

- **Bounded total duration**: Target a fixed total scroll time regardless of tick count. Default `totalMs = 200` (capped so large scrolls don't block input). Per-tick delay = `totalMs / N`, then shaped by the easing curve.
- **Easing**: `SmoothStepDelay(i, N, slowMs, fastMs)` where `slowMs` and `fastMs` are derived from `totalMs / N`. The smoothstep `3t^2 - 2t^3` creates natural momentum: slow start, fast middle, slow end. Edge delays are ~2x the center delay.
- **Jitter**: Add `rand.Intn(6) - 3` ms to each delay. Trivially cheap.
- **Small scrolls (1-3 ticks)**: Skip easing, use uniform delay of `rand.Intn(20) + 10` ms.

**Single xdotool call example (5 ticks down, totalMs=200, avg 40ms/tick):**

```
xdotool mousemove 500 300 click 5 sleep 0.055 click 5 sleep 0.030 click 5 sleep 0.025 click 5 sleep 0.032 click 5
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
| `type_text`   | O(words+typos)   | O(words+typos) `rand.Intn`        | Per-word type + Go-side sleep + optional typo injection |
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