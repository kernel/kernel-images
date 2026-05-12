// wrapper boots the chromium-headful and chromium-headless containers:
// prepares the environment, starts supervisord, brings services up in parallel
// where the dependency graph allows, and waits for CDP to be reachable through
// kernel-images-api.
//
// Replaces the legacy /wrapper.sh shipped in both images. Behavior parity is
// intentional — we still rely on supervisord, sysctl, dbus, etc. The only goal
// beyond parity is minimizing time-to-CDP-ready by removing serial dead time.
//
// The headful vs headless profile is detected at boot from supervisor's conf.d
// (xorg.conf → headful, xvfb.conf → headless), which keeps a single binary
// usable in both images without Dockerfile coordination.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	dbusSocket     = "/run/dbus/system_bus_socket"
	defaultDisplay = ":1"
	defaultIntPort = "9223"
)

type profile int

const (
	profileHeadful profile = iota
	profileHeadless
)

// detectProfile keys off whichever X server's supervisor conf is present.
// The image build is what writes these files, so this is deterministic.
func detectProfile() profile {
	if _, err := os.Stat(filepath.Join(supervisorConfD, "xvfb.conf")); err == nil {
		return profileHeadless
	}
	return profileHeadful
}

func profileName(p profile) string {
	if p == profileHeadless {
		return "headless"
	}
	return "headful"
}

func main() {
	t0 := time.Now()
	prof := detectProfile()
	stzManaged := scaleToZeroManaged()
	logf("starting wrapper (profile=%s stz=%s)", profileName(prof), stzMode(stzManaged))

	// Register signal handling early so a SIGTERM/SIGINT during the
	// seconds-long startup window queues into the channel instead of
	// triggering Go's default exit-immediately behavior. The handler
	// goroutine is installed below, once supervisord is running.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	// /dev/shm: only mount when not running under Docker (Docker manages it).
	if os.Getenv("WITHDOCKER") == "" {
		_ = os.MkdirAll("/dev/shm", 0o1777)
		_ = os.Chmod("/dev/shm", 0o1777)
		_ = exec.Command("mount", "-t", "tmpfs", "tmpfs", "/dev/shm").Run()
	}

	// Disable scale-to-zero for the duration of startup. When ENABLE_STZ is
	// false/0 the caller wants STZ off permanently, so we don't re-enable on
	// exit or once the hot path is up.
	disableScaleToZero()
	if stzManaged {
		defer enableScaleToZero()
	}

	// Headless ships a default CHROMIUM_FLAGS list (headless+stealth flags)
	// when callers don't set one. Headful's defaults are caller-supplied.
	if prof == profileHeadless {
		applyHeadlessDefaultFlags()
	}

	// Hostname: some envs boot with empty/(none); pick a friendly default.
	if h, err := os.ReadFile("/proc/sys/kernel/hostname"); err == nil {
		if v := strings.TrimSpace(string(h)); v == "" || v == "(none)" {
			_ = exec.Command("hostname", "kernel-vm").Run()
			_ = os.WriteFile("/proc/sys/kernel/hostname", []byte("kernel-vm"), 0o644)
		}
	}
	if os.Getenv("HOSTNAME") == "" {
		_ = os.Setenv("HOSTNAME", "kernel-vm")
	}

	// Disable IPv6 — Chromium DOH wastes connection slots on unreachable v6 endpoints.
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/disable_ipv6", []byte("1"), 0o644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/default/disable_ipv6", []byte("1"), 0o644)

	// Pre-create per-user dirs so chromium subsystems don't error.
	prepareUserDirs(os.Getenv("RUN_AS_ROOT") == "true")

	// Tail aggregator for service logs.
	startLogAggregator()

	// Default env that downstream services expect.
	_ = os.Setenv("DISPLAY", defaultDisplay)
	if os.Getenv("INTERNAL_PORT") == "" {
		_ = os.Setenv("INTERNAL_PORT", defaultIntPort)
	}
	if os.Getenv("CHROME_PORT") == "" {
		_ = os.Setenv("CHROME_PORT", "9222")
	}
	// Point dbus clients at the system bus socket. Set before supervisord
	// starts so it captures the env for child services (notably chromium,
	// which would otherwise spam autolaunch errors).
	_ = os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+dbusSocket)

	// Stale X locks from prior runs.
	_ = os.Remove("/tmp/.X1-lock")
	_ = os.Remove("/tmp/.X11-unix/X1")

	// supervisord — start in nodaemon mode so we own its lifecycle.
	// Without -n it forks and the parent exits with code 0, which would
	// drop us out of supCmd.Wait() and the container would stop.
	logf("starting supervisord")
	supCmd := exec.Command("supervisord", "-n", "-c", supervisorConf)
	supCmd.Stdout = os.Stdout
	supCmd.Stderr = os.Stderr
	if err := supCmd.Start(); err != nil {
		fatalf("supervisord start: %v", err)
	}
	// Install the shutdown goroutine now so it can clean up if a signal
	// arrives during the readiness window. Any signal queued in `sigs`
	// before this point gets picked up on the first iteration.
	go func() {
		<-sigs
		logf("shutdown: stopping services")
		_ = exec.Command("supervisorctl", "-c", supervisorConf, "stop", "all").Run()
		_ = supCmd.Process.Signal(syscall.SIGTERM)
	}()
	waitForSocket(supervisorSock, 10*time.Second)

	// Browser phase: identity-free services. Chromium itself doesn't read
	// any per-instance identity envs — it just needs the envoy CA cert
	// (baked into the image at build time, see shared/envoy/bake-certs.sh)
	// so it trusts the forward proxy on first start with no runtime cert
	// work to wait on. chromium-launcher internally waits for the X server
	// and (headful) for mutter before exec'ing chromium, so we start it in
	// parallel with the X server to overlap chromium-launcher's preamble
	// with display startup. chromedriver listens on 9225 immediately and
	// only attaches to chromium on session creation, so it can come up
	// alongside everything. mutter has no internal X-wait, so it's started
	// as soon as the X server is confirmed up — chromium-launcher gates on
	// it so chrome can negotiate CSD with the WM before mapping its window
	// (without it, mutter reparents the existing window with default SSD
	// and a titlebar appears). neko reads the active display mode at start,
	// so it's deferred until after the dbus wait on the WebRTC path.
	xServer := "xorg"
	if prof == profileHeadless {
		xServer = "xvfb"
	}
	webrtc := prof == profileHeadful && os.Getenv("ENABLE_WEBRTC") == "true"

	// Pre-touch chromium's supervisord log so kernel-images-api's `tail -f`
	// doesn't bail out and enter its 250ms retry backoff when started in
	// parallel with chromium.
	_ = os.WriteFile(filepath.Join(supervisordLogD, "chromium"), nil, 0o644)

	browserStart := time.Now()
	startAll(xServer, "dbus", "chromedriver", "chromium")
	waitForX(defaultDisplay, 20*time.Second)
	if prof == profileHeadful {
		startAll("mutter")
	}
	waitForSocket(dbusSocket, 10*time.Second)
	if prof == profileHeadful && webrtc {
		startAll("neko")
	}
	browserDone := time.Now()

	// FORK HOOK:
	//   When this binary runs as a forked snapshot restore, the per-fork
	//   identity envs (INST_NAME, METRO_NAME, XDS_SERVER, KERNEL_INSTANCE_JWT,
	//   plus any future per-tenant secrets) won't be set yet at this point —
	//   the snapshot was taken from a different instance. Insert the
	//   following sequence here once the env-delivery channel exists:
	//     1. Block on the host-pushed env bundle (vsock socket, virtio-fs
	//        drop file, or whatever transport the control plane settles on).
	//     2. Apply the bundle to this process's environ via os.Setenv so
	//        the identity phase below picks them up via the existing $VAR
	//        expansion in init-envoy.sh and the supervisorctl-spawned
	//        services inherit them.
	//     3. The identity phase uses `supervisorctl restart envoy`
	//        (idempotent — start on first boot, stop+start on a re-render
	//        after fork) so a restored snapshot drops its stale identity
	//        cleanly.
	//   Boot path keeps running through unchanged: the wait simply no-ops
	//   when there's no fork bundle to receive.

	// Identity phase: identity-bound services. Render envoy bootstrap with
	// INST_NAME/JWT/etc and (re)start envoy + kernel-images-api. Both
	// services use `restart` so the same code path works for boot (start a
	// stopped service) and post-fork (stop+start to force a re-read of
	// refreshed envs).
	identityStart := time.Now()
	if isExecutable("/usr/local/bin/init-envoy.sh") {
		runStreamFatal("envoy-init", "/usr/local/bin/init-envoy.sh")
	}
	restartAll("kernel-images-api")
	identityDone := time.Now()

	// Wait for the union of caller-visible ready signals. Each probe runs
	// concurrently and logs as soon as its target is reachable.
	probeDurations := waitAllReady(t0, webrtc)
	logf("ready in %s (browser=%s identity=%s; %s)",
		since(t0),
		browserDone.Sub(browserStart).Truncate(time.Millisecond),
		identityDone.Sub(identityStart).Truncate(time.Millisecond),
		formatProbeDurations(probeDurations))

	// Cosmetic + non-critical services come up off the hot path. Headless has
	// no audio stack.
	if prof == profileHeadful {
		go startAll("pulseaudio")
	}

	// Re-enable scale-to-zero now that the hot path is up — unless the caller
	// asked to keep it disabled via ENABLE_STZ=false/0.
	if stzManaged {
		enableScaleToZero()
	}

	// Block on supervisord; container exits when it does.
	if err := supCmd.Wait(); err != nil {
		logf("supervisord exited: %v", err)
	}
}

// waitAllReady gates on all caller-visible ready signals concurrently:
//   - cdp          : HTTP /json/version on the public CDP port (proves api proxy is
//                    wired through to chromium's DevTools server)
//   - chromedriver : TCP on chromedriver's internal port 9225 (api on 9224 is bound
//                    when api itself is up, which CDP readiness already implies)
//   - neko         : TCP on neko's HTTP port (8080), only when ENABLE_WEBRTC=true
//   - envoy        : TCP on envoy's listener (3128), only when envoy is enabled
func waitAllReady(t0 time.Time, webrtc bool) map[string]time.Duration {
	chromePort := os.Getenv("CHROME_PORT")
	probes := []probe{
		{"cdp", func() bool { return httpProbeOK("http://127.0.0.1:" + chromePort + "/json/version") }},
		{"chromedriver", func() bool { return tcpOK("127.0.0.1", "9225") }},
	}
	if webrtc {
		probes = append(probes, probe{"neko", func() bool { return tcpOK("127.0.0.1", "8080") }})
	}
	if envoyEnabled() {
		probes = append(probes, probe{"envoy", func() bool { return tcpOK("127.0.0.1", "3128") }})
	}

	type result struct {
		name string
		dur  time.Duration
		ok   bool
	}
	done := make(chan result, len(probes))
	for _, p := range probes {
		go func(name string, fn func() bool) {
			start := time.Now()
			deadline := start.Add(60 * time.Second)
			for time.Now().Before(deadline) {
				if fn() {
					d := since(t0)
					logf("[ready] %s in %s", name, d)
					done <- result{name, d, true}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			logf("[ready] WARNING: %s never became ready", name)
			done <- result{name, since(t0), false}
		}(p.name, p.fn)
	}
	durations := make(map[string]time.Duration, len(probes))
	for range probes {
		r := <-done
		if r.ok {
			durations[r.name] = r.dur
		}
	}
	return durations
}

type probe struct {
	name string
	fn   func() bool
}

// formatProbeDurations renders waitAllReady's per-probe ready times in a stable
// order so log lines diff cleanly across runs. Probes that never succeeded are
// omitted (they'd already have logged a WARNING separately).
func formatProbeDurations(d map[string]time.Duration) string {
	order := []string{"cdp", "chromedriver", "neko", "envoy"}
	parts := make([]string, 0, len(d))
	for _, name := range order {
		if v, ok := d[name]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", name, v.Truncate(time.Millisecond)))
		}
	}
	return strings.Join(parts, " ")
}

// timestamped wrapper log; prefix mirrors the bash script's [wrapper] tag.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "[wrapper] "+format+"\n", args...)
}

func since(t time.Time) time.Duration {
	return time.Since(t).Truncate(time.Millisecond)
}

func fatalf(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}
