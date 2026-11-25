# Camera kernel modules

The V4L2 core modules (`mc.ko`, `videodev.ko`) plus a camera-persona build of
[v4l2loopback](https://github.com/umlaeute/v4l2loopback) that make a browser
VM's virtual camera present and behave like an ordinary integrated webcam.

`build-modules.sh` builds them for a given `kernel/linux` release tag (the
guest kernel the image will run on; keep it in sync with hypeman's
`DefaultKernelVersion`) and deposits the `.ko` files plus a `load-order`
manifest in the output dir. The Dockerfile stage `camera-modules` runs it and
ships the result at `/usr/lib/kernel-camera/`; `camerad` loads them with
`finit_module(2)` at session start (no kmod needed in the image).

## Why the patch

Stock v4l2loopback is easy to tell apart from a real camera, both to capture
clients (Chromium) and to the pages behind them:

- **Identity.** `VIDIOC_QUERYCAP` reports driver `v4l2 loopback` and a
  `platform:` bus, and the device has no USB identity, so Chromium's camera
  label lacks the usual `<interface> (<vid>:<pid>)` form. The patch reports a
  `uvcvideo` driver and `usb-…` bus, and registers a two-level platform-device
  hierarchy (`v4l2loopback-usb.N` with `idVendor`/`idProduct`, child
  `v4l2loopback-usbif.N` with `interface`) so that
  `/sys/class/video4linux/videoN/device/{interface,../idVendor,../idProduct}`
  resolve exactly like a UVC camera's. vid/pid/interface string are module
  params (`camera_usb_vid`/`camera_usb_pid`/`camera_usb_interface`).

- **Modes.** Stock enumerates one mode (the current format). The patch
  advertises a conventional integrated-cam table (640x480, 800x600, 1280x720
  @30..5fps, 1920x1080 @15..5fps, always discrete), snaps arbitrary capture
  requests to the nearest mode like a UVC driver, and lets the capture side
  negotiate a new size while a producer streams (adopting it without
  reallocating buffers, which the producer — camerad — then follows).

- **Buffers.** Stock reallocates capture buffers on every producer format
  change; a producer restarting at a smaller mode would shrink them and break
  later upward negotiation (`EBUSY`). The patch keeps the allocation whenever
  the new frames fit.

- **Controls.** Stock has no standard camera controls (so Chromium's
  ImageCapture photo capabilities are empty) and registers loopback-specific
  custom controls (`keep_format`, `sustain_framerate`, `timeout`,
  `timeout_image_io`). Chromium resets every visible control to default on
  open — pressing `timeout_image_io`, which poisons every later open into the
  loopback's timeout mode (the device then reports itself output-only and
  `getUserMedia` fails with `NotFoundError`). The patch drops the custom
  controls and adds the standard set an integrated webcam has (brightness,
  contrast, saturation, hue, auto white balance + temperature, gamma, power
  line frequency, sharpness, backlight compensation, auto exposure,
  exposure time, gain — no pan/tilt/zoom/focus, like a fixed-focus laptop
  camera).

- **Streaming tolerance.** Stock fails `STREAMON(CAPTURE)` with `EIO` while
  no producer streams. The patch allows it: a producer that is briefly away
  (mode switch) will be back; the client's DQBUF simply waits.

## Rebuilding

```sh
docker run --rm -v "$PWD":/src:ro -v /tmp/out:/out debian:bookworm-slim bash -c '
  apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    build-essential bc bison flex libssl-dev libelf-dev git curl ca-certificates \
    kmod cpio python3 pahole dwarves >/dev/null &&
  /src/build-modules.sh ch-6.12.8-kernel-3.3-202611131 /out'
```

amd64 only for now (the guest kernel headers and the compiler here are
x86_64); arm64 browsers need a cross build before the camera feature can
reach them.
