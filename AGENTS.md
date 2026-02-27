# AGENTS.md

## Cursor Cloud specific instructions

### Project overview

Kernel Images is a sandboxed cloud browser infrastructure platform. The Go server (under `server/`) provides a REST API for screen recording, process execution, file management, and a CDP (Chrome DevTools Protocol) WebSocket proxy. Everything runs inside a single Docker container orchestrated by supervisord.

### Development commands

- **Lint**: `cd server && go vet ./...`
- **Unit tests** (skip e2e): `cd server && go test -v -race $(go list ./... | grep -v /e2e$)`
- **Full tests** (requires Docker + pre-built images): `cd server && make test`
- **Build server**: `cd server && make build`
- **Build headless image**: `cd /workspace && DOCKER_BUILDKIT=1 docker build -f images/chromium-headless/image/Dockerfile -t kernel-headless-test .`
- **Run headless container**: `docker run -d --name kernel-headless -p 10001:10001 -p 9222:9222 --shm-size=2g kernel-headless-test`
- See `server/README.md` and `server/Makefile` for additional commands and configuration.

### Docker in Cloud VM

The Cloud VM runs inside a Firecracker microVM. Docker requires:
- `fuse-overlayfs` storage driver (configured in `/etc/docker/daemon.json`)
- `iptables-legacy` (not nftables)
- Start daemon with `sudo dockerd &>/tmp/dockerd.log &` then `sudo chmod 666 /var/run/docker.sock`

### Key gotchas

1. **`UKC_METRO` must be the full API URL** (e.g., `https://api.<region>.<domain>/v1`), not just the metro short name. The kraft CLI defaults to `*.kraft.cloud` but this org uses a custom domain — check the `UKC_METRO` environment variable for the correct value.

2. **Some kraft cloud subcommands need `--metro "<full-url>"` explicitly** even when the `UKC_METRO` env var is set.

3. **CDP proxy on port 9222 routes ALL WebSocket connections to the browser-level endpoint** (ignores request path). Use `Target.createTarget` + `Target.attachToTarget` with `flatten: true` for page-level interaction. Playwright/Puppeteer handle this automatically.

4. **The default recorder cannot be restarted after stop+delete within the same process lifetime.** Restart the container or use a custom `recorder_id`.

5. **The server (`make dev`) only runs inside the Docker container** — it exits on bare host because it waits for Chromium devtools upstream on port 9223.

6. **Image naming convention**: Cursor Cloud agents use `onkernel/cursor-agent-<type>:latest` for test images pushed to KraftCloud. Always check quota with `kraft cloud quota` before pushing. Never auto-delete images — present them to the user for approval.

7. **Image storage quota is tight** (~80 GiB limit). Old `kernel-cu` and `chromium-headless` versions consume most of it.

8. **E2e tests** use `testcontainers-go` and require Docker + pre-built images. Set `E2E_CHROMIUM_HEADFUL_IMAGE` and `E2E_CHROMIUM_HEADLESS_IMAGE` env vars to point to the correct image tags.

9. **Go version**: The project requires Go 1.25.0 (per `server/go.mod`). The system Go may be older — ensure `/usr/local/go/bin` is on PATH.
