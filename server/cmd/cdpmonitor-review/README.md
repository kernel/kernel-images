# CDP Monitor Review Harness

This command runs the headful Chromium image from the current checkout, starts the CDP monitor capture session, exposes the WebRTC live view through ngrok, and streams captured events from `/var/log/kernel/*.log`.

## Usage

```bash
cd server
go run ./cmd/cdpmonitor-review
```

By default it uses the reserved ngrok domain `raf-kernel-images.ngrok.app`.

To use another reserved domain:

```bash
cd server
go run ./cmd/cdpmonitor-review --ngrok-domain your-domain.ngrok.app
```

Useful flags:

- `--skip-build`: run an already-built image.
- `--image <tag>`: override the image tag. Defaults to `kernel-cdpmonitor-review:latest`.
- `--container <name>`: override the container name. Defaults to `kernel-cdpmonitor-review`.
- `--log-file <path>`: overwrite a file with a copy of the run output. Defaults to `cdpmonitor-review.log` at the repo root. Use `--log-file ""` to disable.
- `--raw`: print redacted raw event envelopes instead of compact summaries.
- `--data-limit <n>`: adjust printed event data length.
- `--keep-container`: leave the container running after exit.

Once it prints the live view URL, open it, log in as `admin` / `admin` if prompted, and navigate/click/type/scroll manually. Press `Ctrl-C` to stop capture, stop ngrok, and remove the container. Share `cdpmonitor-review.log` when you want someone else to inspect the captured event stream.
