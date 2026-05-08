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
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	supervisorConf  = "/etc/supervisor/supervisord.conf"
	supervisorConfD = "/etc/supervisor/conf.d/services"
	supervisorSock  = "/var/run/supervisor.sock"
	supervisordLogD = "/var/log/supervisord"
	scaleToZeroFile = "/uk/libukp/scale_to_zero_disable"
	dbusSocket      = "/run/dbus/system_bus_socket"
	defaultDisplay  = ":1"
	defaultIntPort  = "9223"
	defaultAPIPort  = "10001"
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

func main() {
	t0 := time.Now()
	prof := detectProfile()
	logf("starting wrapper (profile=%s)", profileName(prof))

	// /dev/shm: only mount when not running under Docker (Docker manages it).
	if os.Getenv("WITHDOCKER") == "" {
		_ = os.MkdirAll("/dev/shm", 0o1777)
		_ = os.Chmod("/dev/shm", 0o1777)
		_ = exec.Command("mount", "-t", "tmpfs", "tmpfs", "/dev/shm").Run()
	}

	// Disable scale-to-zero for the duration of startup; restored on exit.
	disableScaleToZero()
	defer enableScaleToZero()

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
	waitForSocket(supervisorSock, 10*time.Second)

	// Phase A: identity-free services with no X/dbus dependency. chromedriver
	// listens on 9225 immediately and only attaches to chromium on session
	// creation, so it can come up alongside the display stack. The envoy
	// forward-proxy CA cert is baked into the image at build time (see
	// shared/envoy/bake-certs.sh), so chromium trusts it on first start with
	// no runtime cert work to wait on.
	xServer := "xorg"
	if prof == profileHeadless {
		xServer = "xvfb"
	}
	startAll(xServer, "dbus", "chromedriver")
	waitForX(defaultDisplay, 20*time.Second)
	waitForSocket(dbusSocket, 10*time.Second)

	// Pre-touch chromium's supervisord log so kernel-images-api's `tail -f`
	// doesn't bail out and enter its 250ms retry backoff when started in
	// parallel with chromium.
	_ = os.WriteFile(filepath.Join(supervisordLogD, "chromium"), nil, 0o644)

	// Phase B: identity-free X/dbus consumers. Chromium itself doesn't read
	// any per-instance identity envs — it just needs the envoy cert (baked
	// into the image) in trust. mutter is the compositor on headful; neko is
	// the WebRTC streamer when ENABLE_WEBRTC=true.
	webrtc := prof == profileHeadful && os.Getenv("ENABLE_WEBRTC") == "true"
	var phaseB []string
	if prof == profileHeadful {
		phaseB = []string{"mutter", "chromium"}
		if webrtc {
			phaseB = append(phaseB, "neko")
		}
	} else {
		phaseB = []string{"chromium"}
	}
	startAll(phaseB...)

	// FORK HOOK:
	//   When this binary runs as a forked snapshot restore, the per-fork
	//   identity envs (INST_NAME, METRO_NAME, XDS_SERVER, KERNEL_INSTANCE_JWT,
	//   plus any future per-tenant secrets) won't be set yet at this point —
	//   the snapshot was taken from a different instance. Insert the
	//   following sequence here once the env-delivery channel exists:
	//     1. Block on the host-pushed env bundle (vsock socket, virtio-fs
	//        drop file, or whatever transport the control plane settles on).
	//     2. Apply the bundle to this process's environ via os.Setenv so
	//        Phase C below picks them up via the existing $VAR expansion in
	//        init-envoy.sh and the supervisorctl-spawned services inherit
	//        them.
	//     3. Phase C uses `supervisorctl restart envoy` (idempotent — start
	//        on first boot, stop+start on a re-render after fork) so a
	//        restored snapshot drops its stale identity cleanly.
	//   Boot path keeps running through unchanged: the wait simply no-ops
	//   when there's no fork bundle to receive.

	// Phase C: identity-bound. Render envoy bootstrap with INST_NAME/JWT/etc
	// and (re)start envoy + kernel-images-api. Both services use `restart`
	// so the same code path works for boot (start a stopped service) and
	// post-fork (stop+start to force a re-read of refreshed envs).
	if isExecutable("/usr/local/bin/init-envoy.sh") {
		runStream("envoy-init", "/usr/local/bin/init-envoy.sh")
	}
	restartAll("kernel-images-api")

	// Wait for the union of caller-visible ready signals. Each probe runs
	// concurrently and logs as soon as its target is reachable.
	waitAllReady(t0, webrtc)
	logf("ready in %s", since(t0))

	// Cosmetic + non-critical services come up off the hot path. Headless has
	// no audio stack and no UI to dismiss.
	if prof == profileHeadful {
		go func() {
			startAll("pulseaudio")
			if os.Getenv("RUN_AS_ROOT") == "true" {
				dismissNoSandboxWarning()
			}
		}()
	}

	// Re-enable scale-to-zero now that the hot path is up.
	enableScaleToZero()

	// Forward signals so cleanup runs and supervisord is taken down cleanly.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		logf("shutdown: stopping services")
		_ = exec.Command("supervisorctl", "-c", supervisorConf, "stop", "all").Run()
		_ = supCmd.Process.Signal(syscall.SIGTERM)
	}()

	// Block on supervisord; container exits when it does.
	if err := supCmd.Wait(); err != nil {
		logf("supervisord exited: %v", err)
	}
}

// startAll asks supervisord to start the given programs. We invoke
// supervisorctl once (it accepts multiple args) so we don't pay python
// cold-start costs per service.
func startAll(progs ...string) {
	supervisorctl("start", progs...)
}

// restartAll is the start-or-stop+start variant. It's used for services
// that may already be running from a snapshot restore (post-fork, see the
// FORK HOOK in main) so they pick up refreshed envs cleanly. supervisorctl
// `restart` is a no-op stop on cold programs followed by a normal start.
func restartAll(progs ...string) {
	supervisorctl("restart", progs...)
}

func supervisorctl(verb string, progs ...string) {
	if len(progs) == 0 {
		return
	}
	args := append([]string{"-c", supervisorConf, verb}, progs...)
	cmd := exec.Command("supervisorctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run() // a service that fails to come up will surface via readiness checks
}

func waitForSocket(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	logf("WARNING: socket %s not ready after %s", path, timeout)
}

// waitAllReady gates on all caller-visible ready signals concurrently:
//   - CDP   : HTTP /json/version on the public CDP port (proves api proxy is wired
//             through to chromium's DevTools server)
//   - cd    : TCP on chromedriver's internal port 9225 (api on 9224 is bound when
//             api itself is up, which CDP readiness already implies)
//   - proxy : TCP on chromium's --forward-proxy-port (8888)
//   - neko  : TCP on neko's HTTP port (8080), only when ENABLE_WEBRTC=true
//   - envoy : TCP on envoy's listener (3128), only when envoy is enabled
func waitAllReady(t0 time.Time, webrtc bool) {
	chromePort := os.Getenv("CHROME_PORT")
	if chromePort == "" {
		chromePort = "9222"
	}
	probes := []struct {
		name string
		fn   func() bool
	}{
		{"cdp", func() bool { return httpProbeOK("http://127.0.0.1:" + chromePort + "/json/version") }},
		{"chromedriver", func() bool { return tcpOK("127.0.0.1", "9225") }},
		{"forward-proxy", func() bool { return tcpOK("127.0.0.1", "8888") }},
	}
	if webrtc {
		probes = append(probes, struct {
			name string
			fn   func() bool
		}{"neko", func() bool { return tcpOK("127.0.0.1", "8080") }})
	}
	if envoyEnabled() {
		probes = append(probes, struct {
			name string
			fn   func() bool
		}{"envoy", func() bool { return tcpOK("127.0.0.1", "3128") }})
	}

	done := make(chan string, len(probes))
	for _, p := range probes {
		go func(name string, fn func() bool) {
			deadline := time.Now().Add(60 * time.Second)
			for time.Now().Before(deadline) {
				if fn() {
					logf("[ready] %s in %s", name, since(t0))
					done <- name
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			logf("[ready] WARNING: %s never became ready", name)
			done <- name
		}(p.name, p.fn)
	}
	for range probes {
		<-done
	}
}

func tcpOK(host, port string) bool {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(host, port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

var probeClient = &http.Client{Timeout: time.Second}

func httpProbeOK(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.Contains(string(body), `"Browser"`)
}

// waitForX waits until the X server is reachable on display :N. We try both
// the named unix socket (Xorg, headful) and the abstract namespace socket
// (Xvfb runs with -nolisten unix, which disables the named socket but leaves
// the abstract one). Cheaper than spawning xdpyinfo in a loop.
func waitForX(display string, timeout time.Duration) {
	num := strings.TrimPrefix(display, ":")
	named := "/tmp/.X11-unix/X" + num
	abstract := "@/tmp/.X11-unix/X" + num // Linux abstract namespace
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", named, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		if c, err := net.DialTimeout("unix", abstract, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	logf("WARNING: X display %s not responsive after %s", display, timeout)
}

func disableScaleToZero() { writeScaleToZero("+") }
func enableScaleToZero()  { writeScaleToZero("-") }

func writeScaleToZero(c string) {
	if _, err := os.Stat(scaleToZeroFile); err != nil {
		return // not running on Unikraft Cloud
	}
	_ = os.WriteFile(scaleToZeroFile, []byte(c), 0o644)
}

// applyHeadlessDefaultFlags mirrors the legacy headless wrapper.sh: when
// CHROMIUM_FLAGS is unset, fill in a curated headless+stealth flag list.
// --disable-background-networking is intentionally omitted: it prevents
// Chrome from fetching ExtensionInstallForcelist managed extensions.
func applyHeadlessDefaultFlags() {
	if strings.TrimSpace(os.Getenv("CHROMIUM_FLAGS")) != "" {
		return
	}
	flags := strings.Join([]string{
		"--accept-lang=en-US,en",
		"--allow-pre-commit-input",
		"--blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4",
		"--crash-dumps-dir=/tmp/chromium-dumps",
		"--disable-back-forward-cache",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-blink-features=AutomationControlled",
		"--disable-breakpad",
		"--disable-client-side-phishing-detection",
		"--disable-component-extensions-with-background-pages",
		"--disable-crash-reporter",
		"--disable-crashpad",
		"--disable-dev-shm-usage",
		"--disable-features=AcceptCHFrame,AutoExpandDetailsElement,AvoidUnnecessaryBeforeUnloadCheckSync,CertificateTransparencyComponentUpdater,DeferRendererTasksAfterInput,DestroyProfileOnBrowserClose,DialMediaRouteProvider,ExtensionManifestV2Disabled,GlobalMediaControls,HttpsUpgrades,ImprovedCookieControls,LazyFrameLoading,LensOverlay,MediaRouter,PaintHolding,ThirdPartyStoragePartitioning,Translate",
		"--disable-field-trial-config",
		"--disable-gcm-registration",
		"--disable-gpu",
		"--disable-gpu-compositing",
		"--disable-hang-monitor",
		"--disable-ipc-flooding-protection",
		"--disable-notifications",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--disable-renderer-backgrounding",
		"--disable-search-engine-choice-screen",
		"--disable-software-rasterizer",
		"--enable-use-zoom-for-dsf=false",
		"--export-tagged-pdf",
		"--force-color-profile=srgb",
		"--hide-crash-restore-bubble",
		"--hide-scrollbars",
		"--metrics-recording-only",
		"--mute-audio",
		"--no-default-browser-check",
		"--no-first-run",
		"--no-sandbox",
		"--no-service-autorun",
		"--ozone-platform=headless",
		"--password-store=basic",
		"--unsafely-disable-devtools-self-xss-warnings",
		"--use-angle=swiftshader",
		"--use-gl=angle",
		"--use-mock-keychain",
	}, " ")
	_ = os.Setenv("CHROMIUM_FLAGS", flags)
}

func profileName(p profile) string {
	if p == profileHeadless {
		return "headless"
	}
	return "headful"
}

// envoyEnabled mirrors init-envoy.sh's gate: when any of these are unset
// the script exits early without starting envoy, so we should skip the
// readiness probe too (otherwise it would just time out at 60s).
func envoyEnabled() bool {
	return os.Getenv("INST_NAME") != "" &&
		os.Getenv("METRO_NAME") != "" &&
		os.Getenv("XDS_SERVER") != "" &&
		os.Getenv("KERNEL_INSTANCE_JWT") != ""
}

func prepareUserDirs(asRoot bool) {
	if asRoot {
		for _, d := range []string{"/tmp", "/var/log", supervisordLogD, "/home/kernel", "/home/kernel/user-data"} {
			_ = os.MkdirAll(d, 0o755)
		}
		return
	}
	dirs := []string{
		"/home/kernel/user-data",
		"/home/kernel/.config/chromium",
		"/home/kernel/.pki/nssdb",
		"/home/kernel/.cache/dconf",
		"/tmp",
		"/var/log",
		supervisordLogD,
	}
	for _, d := range dirs {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = exec.Command("chown", "-R", "kernel:kernel",
		"/home/kernel", "/home/kernel/user-data", "/home/kernel/.config",
		"/home/kernel/.pki", "/home/kernel/.cache").Run()
	_ = exec.Command("chown", "-R", "kernel:kernel", "/etc/chromium/policies").Run()
}

// startLogAggregator tails any file under /var/log/supervisord, prefixing
// each line with the relative path so the container log stream remains
// readable.
func startLogAggregator() {
	_ = os.MkdirAll(supervisordLogD, 0o755)
	go func() {
		seen := map[string]bool{}
		for {
			entries, _ := os.ReadDir(supervisordLogD)
			for _, e := range entries {
				path := filepath.Join(supervisordLogD, e.Name())
				if seen[path] {
					continue
				}
				if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
					seen[path] = true
					go tailFile(path)
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
}

func tailFile(path string) {
	cmd := exec.Command("tail", "-n", "+1", "-F", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return
	}
	label := filepath.Base(path)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fmt.Printf("[%s] %s\n", label, scanner.Text())
	}
}

// dismissNoSandboxWarning replicates the wrapper.sh behaviour of clicking the
// "X" on the --no-sandbox infobar. Cosmetic; runs off the hot path.
func dismissNoSandboxWarning() {
	out, err := exec.Command("xdotool", "getdisplaygeometry").Output()
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return
	}
	width := parts[0]
	x := width
	if w := atoi(width); w > 30 {
		x = fmt.Sprintf("%d", w-30)
	}
	target := "New Tab - Chromium"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("xdotool", "search", "--name", target).Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			id := strings.Fields(string(out))[0]
			if exec.Command("xdotool", "windowactivate", "--sync", id).Run() == nil {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Without a settle delay the click can land before the --no-sandbox infobar
	// has finished painting, leaving the warning on screen. The legacy
	// wrapper.sh slept 5s here for the same reason. Runs off the hot path
	// (goroutine fired post-readiness) so this doesn't extend time-to-CDP.
	time.Sleep(5 * time.Second)
	port := os.Getenv("KERNEL_IMAGES_API_PORT")
	if port == "" {
		port = defaultAPIPort
	}
	body := fmt.Sprintf(`{"x":%s,"y":115}`, x)
	_ = exec.Command("curl", "-s", "-o", "/dev/null", "-X", "POST",
		"http://localhost:"+port+"/computer/click_mouse",
		"-H", "Content-Type: application/json",
		"-d", body).Run()
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func runStream(label, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = prefixWriter{label: label, w: os.Stdout}
	cmd.Stderr = prefixWriter{label: label, w: os.Stderr}
	_ = cmd.Run()
}

type prefixWriter struct {
	label string
	w     *os.File
}

func (p prefixWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(p.w, "[%s] %s\n", p.label, line)
	}
	return len(b), nil
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
