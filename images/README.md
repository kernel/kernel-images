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

- Now new endpoint should be available for tests example curl command:
```sh
curl -X POST localhost:444/computer/cursor \
  -H "Content-Type: application/json" \
  -d '{"hidden": true}'
```

