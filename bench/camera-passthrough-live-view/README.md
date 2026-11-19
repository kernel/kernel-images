# Camera passthrough for live view — reproduction & findings

Harness and findings behind the production code in this PR (`server/lib/camera`,
`server/cmd/camerad`, the wrapper/neko wiring, the client controller in
`images/chromium-headful/client/src/kernel/camera.ts`, and the camera kernel
modules in `images/chromium-headful/camera/`). Follows the guest-kernel /
loopback-module spike in kernel-images#515.

## What this enables

A live-view viewer shares their **real** camera into the browser so a page
inside it can use it. On demand: the viewer's camera turns on when a page calls
`getUserMedia()` and off when it stops. This is for a real person completing
their own camera step — **not** for feeding synthetic or recorded video to a
liveness check.

## Findings (validated in a real browser, 2026-11-16; device work 2026-11-19)

1. **Passthrough works.** A page's `getUserMedia()` auto-starts the viewer's
   camera; the browser receives live 1280×720; the camera stops ~3s after the
   page releases it. Verified on the WebRTC getUserMedia sample.

2. **The device presents as an ordinary integrated webcam.** The
   camera-persona v4l2loopback (see `images/chromium-headful/camera/`) plus
   the camerad mixer make the loopback look and behave like local hardware to
   a page, measured in Chromium 156 against the spike's fingerprint table:

   | Signal | Stock loopback (spike) | Now |
   | --- | --- | --- |
   | Label | `Integrated Camera` (no USB suffix) | `Integrated Camera` via the USB `interface` file, `uvcvideo`/`usb-…` QUERYCAP (vid:pid files in place; see note) |
   | Native modes | one (the producer's) | 640x480, 800x600, 1280x720, 1920x1080, all `resizeMode:"none"` |
   | `getUserMedia({video:true})` default | 1280x720 | 640x480 (like a real cam) |
   | `{width:{exact:1920}}` | `OverconstrainedError` | 1920x1080 native |
   | `{resizeMode:"none", width:640}` | 1280x720 | 640x480 |
   | ImageCapture photo caps | imageWidth/Height only | brightness, contrast, saturation, sharpness, exposure, white balance, color temperature |
   | Frame cadence | network-paced | paced 33.33ms ±1ms jitter (mediaTime sd ≈2.5ms) |
   | Flat-region pixels | codec-smooth | sensor-like grain (temporal + fixed-pattern), no two frames identical |
   | open → stop → open | `NotFoundError` (loopback timeout mode) | stable across rounds |

   Note on the label suffix: the sysfs `idVendor`/`idProduct` files are in
   place for clients that read them, but current Chromium does not append
   `(vid:pid)` to the JS label for every device, and integrated cameras
   attached over CSI (no USB) genuinely show no suffix, so this is a weak
   signal at worst.

3. **Liveness SDKs.** A widely used browser liveness demo was observed
   (2026-11-16) to refuse the *stock* loopback session with a "3rd-party app
   is modifying the video / suspicious webcam configuration" error. Whether
   the persona device passes is not re-tested here — check the vendor's
   terms before testing a relayed feed against a live liveness demo; the
   point of the persona is that flows with a real, present person are not
   false-flagged as *injected* video, not to carry recorded/synthetic video
   (which remains out of scope and is exactly what those checks exist for).

4. **Orientation (known limitation).** Capture is pinned to 1280×720 landscape
   end to end, so rotating a phone does not change the camera aspect. The client
   controller re-requests `getUserMedia` on `orientationchange`; native
   per-orientation device caps (reloading the loopback) remain a follow-up.

## Reproduce against a running browser (no image build)

Inside a browser VM (the modules are built for the guest kernel by
`images/chromium-headful/camera/build-modules.sh`):

```sh
# load the loopback pair, run the manager, enable neko webcam ingest
camerad &                      # loads /usr/lib/kernel-camera/*.ko itself
NEKO_CAPTURE_WEBCAM_ENABLED=true NEKO_CAPTURE_WEBCAM_DEVICE=/dev/kernel-camera/ingest \
  supervisorctl restart neko
```

`kernel-camera.injected.js` is the exact client controller used in the live
test, as a drop-in `<script>` for a browser where you can't rebuild the neko
client; `src/kernel/camera.ts` is the same logic as a real client module.

## Local validation without a browser VM

On any machine running the matching guest kernel (e.g. a Hypeship task VM):
build the modules with `images/chromium-headful/camera/` (docker, bookworm),
insmod them, run camerad, feed the ingest device a pattern with ffmpeg
(`-f v4l2 /dev/kernel-camera/ingest`), and point a local Chromium's CDP at the
probes (`bun install` once): `fp.ts` (fingerprint dump), `cycle.ts` (open/stop
rounds), `timing.ts` (cadence + noise), `grab.ts` (save a page frame as PNG).
That is how the table above was measured.
