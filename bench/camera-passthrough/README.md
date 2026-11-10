# Camera passthrough spike

**Status: SPIKE, do not merge.** Throwaway tooling and findings from checking
whether a browser VM can expose a real-time camera to Chromium, fed by a live
remote stream.

## Findings

| Question | Result |
| --- | --- |
| V4L2 in the guest kernel today | No. `ch_defconfig` in `kernel/linux` has `# CONFIG_MEDIA_SUPPORT is not set`. `CONFIG_MODULES=y`, no module signing. |
| v4l2loopback on the **stock** guest kernel | Yes, if `mc.ko` + `videodev.ko` are built out of tree from the same tag/config/compiler (`kernel/build-modules.sh`). All exported symbol CRCs match. Tested on `ch-6.12.8-kernel-3.2-202610301`. |
| Kernel with V4L2 built in | Builds cleanly with `kernel/media.config`. bzImage +131 KB (9.62 MB → 9.75 MB), vmlinux +118 KB. Boot to root mount unchanged (~0.29 s on cloud-hypervisor, 2 vCPU); media core init < 1 ms. |
| Chromium sees the device | Yes. `getUserMedia({video:true})` returns live 1280x720 frames from ffmpeg in both upstream Chromium 156 and the `chromium-headful` image (Chrome for Testing 155). Permission granted via CDP `Browser.grantPermissions(["videoCapture"])`. |
| Live remote WebRTC source (WHIP → pion → ffmpeg → v4l2) | Works. Glass-to-`getUserMedia` latency, sender on another host over a 14 ms RTT link: p50 112 ms / p90 144 ms / p99 159 ms (sender canvas timestamp → frame decoded in page). |
| Live view upstream (neko) | Works with no server code change. neko already has `capture.webcam.*` (VP8 → GStreamer `v4l2sink`); the image ships `libgstvideo4linux2`. Viewer `addTrack()` renegotiates over the existing peer. Same-host p50 72 ms / p90 83 ms. |
| v4l2 → `getUserMedia` leg alone | p50 62 ms / p90 77 ms (headful container on a 10 vCPU VM, ~25 fps compositor). |

### How the device looks to a page

| Signal | v4l2loopback | Typical laptop webcam in Chrome |
| --- | --- | --- |
| `enumerateDevices()` label | `card_label` verbatim, e.g. `Integrated Camera` | Linux appends USB `(vid:pid)` for UVC devices; a loopback device has no USB IDs, so no suffix |
| Native modes | One: whatever the producer writes (1280x720) | Several (640x480, 1280x720, often 1920x1080) |
| `getUserMedia({video:true})` default | 1280x720 (only mode) | 640x480 native |
| `{width:{exact:1920}}` | `OverconstrainedError` | Succeeds on 1080p cameras |
| `{resizeMode:"none", width:640}` | 1280x720 | 640x480 |
| ImageCapture `getPhotoCapabilities()` | Only `imageWidth/Height` fixed at 1280x720, `redEyeReduction: never` | Usually exposes brightness, contrast, saturation, sharpness, exposure / white balance modes |
| `getCapabilities()` | width/height 1–max, frameRate 0–30, `facingMode: []` | Same shape; values depend on the camera |

The left column is measured. The right column comes from how Chrome reports
UVC devices and hasn't been checked side by side against a real laptop yet.
The native-mode and ImageCapture differences show up without any
fingerprinting effort. Liveness vendors that target "virtual camera" or
"camera feed takeover" attacks should be expected to flag this device.

## Layout

- `kernel/media.config`: config fragment to build V4L2 core into the guest kernel.
- `kernel/build-modules.sh`: build `mc.ko`, `videodev.ko`, `v4l2loopback.ko` for an existing guest kernel release.
- `whip-bridge/`: minimal WHIP endpoint (pion) that pipes the first VP8 track through ffmpeg into `/dev/video0`. `/send` serves a synthetic sender that stamps the bridge-clock time into each frame.
- `probes/`: Playwright scripts run against a browser's CDP endpoint.
  - `probe.ts`: open the WebRTC gUM sample, report label, settings, capabilities, observed fps.
  - `latency.ts`: decode the frame timestamps in-page and report latency percentiles.
  - `fp.ts`: dump page-visible camera properties and supported resolutions.
  - `liveview-cam.ts`: join neko live view and send a stamped camera track upstream.
  - `stampwriter.py`: raw stamped frames for measuring the v4l2 → gUM leg alone.

## Reproduce

Run inside a browser VM, or in a VM on the same guest kernel with the image under docker:

```sh
kernel/build-modules.sh ch-6.12.8-kernel-3.2-202610301 ./out
sudo insmod out/mc.ko && sudo insmod out/videodev.ko
sudo insmod out/v4l2loopback.ko devices=1 video_nr=0 card_label="Integrated Camera" exclusive_caps=1

# test pattern
ffmpeg -re -f lavfi -i testsrc2=size=1280x720:rate=30 -pix_fmt yuv420p -f v4l2 /dev/video0 &

# headful image with the device and neko webcam ingest enabled
docker run -d --name kcam --privileged --tmpfs /dev/shm:size=2g \
  -p 9222:9222 -p 8080:8080 -e RUN_AS_ROOT=true -e ENABLE_WEBRTC=true \
  -e NEKO_CAPTURE_WEBCAM_ENABLED=true -e NEKO_CAPTURE_WEBCAM_DEVICE=/dev/video0 \
  onkernel/chromium-headful:<tag>

cd probes && bun install
bun probe.ts http://127.0.0.1:9222
bun fp.ts http://127.0.0.1:9222

# remote WHIP source: run the bridge (stop the test-pattern ffmpeg first), open /send on another host
(cd ../whip-bridge && go run . -listen :8090)
bun latency.ts http://127.0.0.1:9222 20

# live view upstream (stop the bridge first; neko writes the device)
bun liveview-cam.ts "http://127.0.0.1:8080/?usr=viewer&pwd=admin" 120 &
bun latency.ts http://127.0.0.1:9222 15
```

Only one producer can write the device at a time. With `exclusive_caps=1` the
device advertises capture only while a producer is attached, so Chrome lists
no camera until a stream starts.
