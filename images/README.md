## How to test kernel-images changes locally with docker

- Make relevant changes to kernel-images example adding a new endpoint at `kernel-images/server/cmd/api/api/computer.go`, example I added `SetCursor()` endpoint.
- Run openApi to generate the boilerplate for the new endpoints with make oapi-generate
- Check changes at `kernel-images/server/lib/oapi/oapi.go`
- `cd kernel-images/images/chromium-headful`
-  Build and run the docker image with `./build-docker.sh && ENABLE_WEBRTC=true ./run-docker.sh`
- Open http://localhost:8080/ in your browser

### Experimental native Wayland Chromium

Set `KERNEL_WAYLAND_PURE=true` when running the headful image to launch
Chromium with the native Wayland Ozone backend on a headless Weston output.
This mode does not start Xorg, Mutter, Neko, or the X11 capture/input path;
use CDP for browser interaction and screenshots while benchmarking it.

```sh
KERNEL_WAYLAND_PURE=true ./run-docker.sh
```

`KERNEL_WAYLAND_NESTED=true` remains available for comparing native Wayland
Chromium while retaining the existing X11 capture and input path.

Set `ENABLE_WEBRTC=true ENABLE_WAYLAND_WEBRTC=true` in pure mode to run Neko
against the Wayland capture and `/dev/uinput` input backends. This requires
the Neko build with Wayland support and a wlroots compositor exposing
screencopy and output-management.

#### Browser-only benchmark

Ten fresh-container trials per mode, using the same Chromium flags and a
1920x1080 configuration. The screenshot metric is CDP
`Page.captureScreenshot`; it does not measure product capture or live view.

| metric | X11 | pure Wayland |
| --- | ---: | ---: |
| wrapper readiness (mean) | 2.84s | 2.19s |
| Chromium startup (mean) | 602ms | 565ms |
| CDP evaluation p50 (mean) | 1.06ms | 0.96ms |
| CDP screenshot p50 (mean) | 257ms | 116ms |
| container memory (mean) | 709MiB | 414MiB |

The memory reduction primarily comes from not starting Xorg and Mutter. Pure
mode remains experimental and does not provide the X11 screenshot, input,
recording, or live-view paths.

- Now new endpoint should be available for tests example curl command:
```sh
curl -X POST localhost:444/computer/cursor \
  -H "Content-Type: application/json" \
  -d '{"hidden": true}'
```

