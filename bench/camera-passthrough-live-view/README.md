# Camera passthrough for live view — reproduction & findings

Harness and findings behind the production code in this PR (`server/lib/camera`,
`server/cmd/camerad`, the wrapper/neko wiring, and the client controller in
`images/chromium-headful/client/src/kernel/camera.ts`). Follows the
guest-kernel / loopback-module spike in kernel-images#515.

## What this enables

A live-view viewer shares their **real** camera into the browser so a page
inside it can use it. On demand: the viewer's camera turns on when a page calls
`getUserMedia()` and off when it stops. This is for a real person completing
their own camera step — **not** for feeding synthetic or recorded video to a
liveness check.

## Findings (validated in a real browser, 2026-11-16)

1. **Passthrough works.** A page's `getUserMedia()` auto-starts the viewer's
   camera; the browser receives live 1280×720; the camera stops ~3s after the
   page releases it. Verified on the WebRTC getUserMedia sample.

2. **Liveness SDKs reject the virtual camera, by design.** A widely used
   browser liveness demo refuses the session with an "a 3rd-party app is
   modifying the video / suspicious webcam configuration" error — it detects the
   v4l2loopback device. **Camera passthrough alone will not carry a liveness
   step on a vendor that rejects virtual cameras.** We do not attempt to
   disguise the device to defeat that detection: the check exists to confirm a
   real person is physically present. Passthrough is for flows whose vendor
   permits a relayed camera, and for non-liveness camera use (video, capture).

3. **Orientation (known limitation).** Capture is pinned to 1280×720 landscape
   end to end, so rotating a phone does not change the camera aspect. The client
   controller re-requests `getUserMedia` on `orientationchange`; native
   per-orientation device caps (reloading the loopback) remain a follow-up.

## Reproduce against a running browser (no image build)

Inside a browser VM with the camera kernel module available (see
kernel-images#515 for the guest-kernel bits):

```sh
# load the loopback pair, run the manager, enable neko webcam ingest
camerad &                      # prebuilt binary from this PR
NEKO_CAPTURE_WEBCAM_ENABLED=true NEKO_CAPTURE_WEBCAM_DEVICE=/dev/video1 \
  supervisorctl restart neko
```

`kernel-camera.injected.js` is the exact client controller used in the live
test, as a drop-in `<script>` for a browser where you can't rebuild the neko
client; `src/kernel/camera.ts` is the same logic as a real client module.
