# Mouse Movement Demo — Before/After Video

Create a before/after video demonstrating human-like Bezier curve mouse movement vs instant teleport, inspired by [Camoufox's stealth overview](https://camoufox.com/stealth/) and [cursor movement docs](https://camoufox.com/fingerprint/cursor-movement).

## What You'll Record

- **BEFORE**: Instant movement (`smooth: false`) — cursor jumps in straight lines between targets
- **AFTER**: Human-like Bezier movement (`smooth: true`) — curved, natural trajectory

The cursor trail overlay makes the difference visually obvious.

## Prerequisites

- Kernel browser session running kernel-images-private (with Bezier support in `server/cmd/api/api/computer.go`)
- `KERNEL_BROWSER_ID` and `KERNEL_API_KEY` (or equivalent auth)
- Screen recorder (OBS, QuickTime, or `ffmpeg`)

## Steps

### 1. Start Screen Recording

Record the **browser live view** URL. Options:

- **OBS**: Add Browser source or window capture for the live view tab
- **QuickTime** (macOS): File → New Screen Recording, select the live view window
- **ffmpeg**:
  ```bash
  ffmpeg -f avfoundation -i "1" -c:v libx264 -crf 18 mouse-demo.mp4
  ```

### 2. Run the Demo Script

```bash
cd demo/mouse-movement
npm install
KERNEL_BROWSER_ID=<your-browser-id> KERNEL_API_KEY=<key> npm run demo
```

### 3. What Happens

1. The script loads the cursor trail demo page (`cursor-trail-demo.html`) into the browser
2. **BEFORE** phase: Moves the mouse along the path with `smooth: false` — straight lines
3. Pause and clear trail
4. **AFTER** phase: Same path with `smooth: true` — Bezier curves
5. The trail shows the curved vs straight paths

### 4. Edit the Video

- Trim to show BEFORE and AFTER clearly
- Optional: split screen or side-by-side comparison
- Add captions: "Instant movement" vs "Human-like Bezier movement"

## Files

| File | Purpose |
|------|---------|
| `cursor-trail-demo.html` | Page that draws the cursor path as the mouse moves |
| `demo-mouse-movement-video.ts` | Script that runs before/after moveMouse with smooth on/off |

## Implementation

The Bezier trajectory and `smooth` movement are implemented in `server/cmd/api/api/computer.go` and `server/lib/mousetrajectory/`. When `smooth: true` is sent in the move_mouse request body, the instance uses Bernstein Bezier curves for human-like movement.
