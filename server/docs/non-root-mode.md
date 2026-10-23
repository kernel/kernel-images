# Non-root browser images

`RUN_AS_ROOT=false` (also the default when unset) runs customer-facing services
as the `kernel` user. `RUN_AS_ROOT=true` retains root-mode behavior. This applies
to both headful and headless images.

This extends the old flag, which primarily selected Chromium's user. Deployments
that already set it to false, or leave it unset, now also lose root access through
the process and filesystem APIs. The launch scripts retain their existing
defaults: headful `run-unikernel.sh` supplies true, while headful `run-docker.sh`
and headless `run-unikernel.sh` supply false.

Use a fresh container or guest when switching modes. Changing the flag cannot
safely revoke root access from an existing filesystem that a caller has already
controlled as root, and the supervisor split is not reversed by setting true.

## Privilege boundary

The wrapper starts two supervisors in non-root mode:

- The user supervisor runs with `setpriv --no-new-privs` as `kernel`, as do its
  children: the API, Chromium launcher/browser, ChromeDriver, Neko, Openbox,
  PulseAudio and Xvfb. API-created processes, Playwright executors and the browser
  REPL inherit that identity. Setuid binaries and file capabilities cannot
  elevate these processes.
- The root supervisor manages Xorg, dbus and Envoy. Its configuration directory
  and control socket are inaccessible to the user. Each supervisor has its own
  crash-event listener.

The API can restart its own browser and live-view services, or reconfigure Xvfb,
without access to the root supervisor. Customer-writable supervisor configuration
is never loaded by the root supervisor, including on subsequent boots. User
service logs rotate separately from root service logs; the wrapper reads them as
`kernel` so log symlinks cannot expose root files.

Profiles, browser policies, recordings and timezone indirection remain
user-writable. Xorg's configuration, Neko's static configuration and executables
remain root-owned. Xorg creates its input socket with group access for Neko.

## Compatibility

- `as_root: true` and nonempty `as_user` are rejected with HTTP 400 by both exec
  and spawn, including `as_user: "kernel"`. Request environment overrides cannot
  change the server's policy.
- Filesystem APIs can only access files allowed by the `kernel` user's OS
  permissions. Root package installation and arbitrary system modifications are
  unavailable.
- Integrations that bootstrap SSH or other services through root process calls
  must be adapted, or use an explicitly root-mode image.
- Root-only API diagnostics such as reading `/dev/kmsg` are unavailable. Crash
  events from supervisor remain available.
- This is not a boundary against the host administrator or guest-kernel
  vulnerabilities.

## Validation

Run the Go tests and vet from `server/`:

```sh
go test -race $(go list ./... | grep -v /e2e$)
go vet ./...
```

On disposable headful and headless containers, exercise both privilege modes:

- Confirm the API, browser and exec/spawn effective identities and `NoNewPrivs`.
- Check exec/spawn identity overrides, request-environment overrides and
  filesystem permissions.
- Verify root supervisor configuration and its socket are inaccessible to the
  browser user, including after restarting with modified user service configs.
- Exercise browser restart, display resize, timezone changes, screenshots,
  recording with audio and headful live-view input/video.

The Go tests cover policy checks, the split of both images' service configurations,
configuration isolation across restarts and stale-socket readiness. They do not
replace image-level validation of the browser, display and media services.
