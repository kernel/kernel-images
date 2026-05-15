package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/kernel-images/server/lib/chromiumflags"
	"github.com/kernel/kernel-images/server/lib/x11"
)

func main() {
	launcherStart := time.Now()

	headless := flag.Bool("headless", false, "Run Chromium with headless flags")
	chromiumPath := flag.String("chromium", "chromium", "Chromium binary path (default: chromium)")
	runtimeFlagsPath := flag.String("runtime-flags", "/chromium/flags", "Path to runtime flags overlay file")
	flag.Parse()

	// Clean up stale lock file from previous SIGKILL termination
	// Chromium creates this lock and doesn't clean it up when killed
	_ = os.Remove("/home/kernel/user-data/SingletonLock")
	_ = os.Remove("/home/kernel/user-data/SingletonSocket")
	_ = os.Remove("/home/kernel/user-data/SingletonCookie")

	// Kill any existing chromium processes to ensure clean restart.
	// This is necessary because supervisord's stopwaitsecs=0 doesn't wait for
	// the old process to fully die before starting the new one, which can cause
	// the new process to fall back to IPv6 while the old one holds IPv4.
	killStats := killExistingChromium()

	// Inputs
	internalPort := strings.TrimSpace(os.Getenv("INTERNAL_PORT"))
	if internalPort == "" {
		internalPort = "9223"
	}

	// Parse devtools port for /proc/net/tcp lookups; default to 9223 on parse failure.
	internalPortNum := uint16(9223)
	if n, err := strconv.ParseUint(internalPort, 10, 16); err == nil {
		internalPortNum = uint16(n)
	}

	// Wait for devtools port to be available (handles SIGKILL socket cleanup delay)
	waitportStats := waitForPort(internalPort, 5*time.Second)

	// Wait for the X server. The wrapper starts chromium in parallel with
	// xorg/xvfb, so the display socket may not be ready yet — without this
	// gate chromium would fail on connect and supervisord would restart us.
	if d := x11.WaitForDisplay(":1", 20*time.Second); d >= 20*time.Second {
		fmt.Fprintf(os.Stderr, "warning: X display :1 not responsive after %s\n", d)
	}

	// Headful: wait for mutter to register before exec'ing chromium. If
	// chromium maps its window with no WM present, the CSD hint it sends has
	// no listener; mutter starts later, reparents the existing window, and
	// applies default SSD — i.e., the titlebar with the close X. Headless
	// has no WM, so skip.
	if !*headless {
		if d := x11.WaitForMutter(20 * time.Second); d >= 20*time.Second {
			fmt.Fprintf(os.Stderr, "warning: mutter not registered after %s\n", d)
		}
	}

	baseFlags := os.Getenv("CHROMIUM_FLAGS")
	runtimeTokens, err := chromiumflags.ReadOptionalFlagFile(*runtimeFlagsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed reading runtime flags: %v\n", err)
		os.Exit(1)
	}
	final := chromiumflags.MergeFlagsWithRuntimeTokens(baseFlags, runtimeTokens)

	// Diagnostics for parity with previous scripts
	fmt.Printf("BASE_FLAGS: %s\n", baseFlags)
	fmt.Printf("RUNTIME_FLAGS: %s\n", strings.Join(runtimeTokens, " "))
	fmt.Printf("FINAL_FLAGS: %s\n", strings.Join(final, " "))

	// flags we send no matter what
	chromiumArgs := []string{
		fmt.Sprintf("--remote-debugging-port=%s", internalPort),
		"--remote-allow-origins=*",
		"--user-data-dir=/home/kernel/user-data",
		"--password-store=basic",
		"--no-first-run",
	}
	if *headless {
		chromiumArgs = append([]string{"--headless=new"}, chromiumArgs...)
	}
	chromiumArgs = append(chromiumArgs, final...)

	runAsRoot := strings.EqualFold(strings.TrimSpace(os.Getenv("RUN_AS_ROOT")), "true")

	// Prepare environment
	env := os.Environ()
	env = append(env,
		"DISPLAY=:1",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket",
	)

	emitPhaseCompleteEvent(launcherStart, *headless, internalPortNum, killStats, waitportStats)

	if runAsRoot {
		// Replace current process with Chromium
		if p, err := execLookPath(*chromiumPath); err == nil {
			if err := syscall.Exec(p, append([]string{filepath.Base(p)}, chromiumArgs...), env); err != nil {
				fmt.Fprintf(os.Stderr, "exec chromium failed: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "chromium binary not found: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Not running as root: call runuser to exec as kernel user, providing env vars inside
	runuserPath, err := execLookPath("runuser")
	if err != nil {
		fmt.Fprintf(os.Stderr, "runuser not found: %v\n", err)
		os.Exit(1)
	}

	// Build: runuser -u kernel -- env DISPLAY=... DBUS_... XDG_... HOME=... chromium <args>
	inner := []string{
		"env",
		"DISPLAY=:1",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket",
		"XDG_CONFIG_HOME=/home/kernel/.config",
		"XDG_CACHE_HOME=/home/kernel/.cache",
		"HOME=/home/kernel",
		*chromiumPath,
	}
	inner = append(inner, chromiumArgs...)
	argv := append([]string{filepath.Base(runuserPath), "-u", "kernel", "--"}, inner...)
	if err := syscall.Exec(runuserPath, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "exec runuser failed: %v\n", err)
		os.Exit(1)
	}
}


// execLookPath helps satisfy syscall.Exec's requirement to pass an absolute path.
func execLookPath(file string) (string, error) {
	if strings.ContainsRune(file, os.PathSeparator) {
		return file, nil
	}
	return exec.LookPath(file)
}

// waitportStats summarizes a single waitForPort invocation for the
// chromium_launcher_phase_complete instrumentation event.
type waitportStats struct {
	elapsedMs int64
	attempts  int
	timedOut  bool
	lastErr   string
}

// waitForPort waits until the given port is available for binding on IPv4.
// This handles the delay after SIGKILL before the kernel releases the socket.
// We disable SO_REUSEADDR to get an accurate check matching chromium's bind behavior.
// Only IPv4 is checked because IPv6 is disabled at the kernel level in the VM.
func waitForPort(port string, timeout time.Duration) waitportStats {
	stats := waitportStats{}
	start := time.Now()
	deadline := start.Add(timeout)
	addrs := []string{"127.0.0.1:" + port}

	// ListenConfig with Control to disable SO_REUSEADDR for accurate port availability check
	lc := &net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				// Disable SO_REUSEADDR to match chromium's behavior
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}

	ctx := context.Background()
	for time.Now().Before(deadline) {
		stats.attempts++
		allFree := true
		for _, addr := range addrs {
			ln, err := lc.Listen(ctx, "tcp", addr)
			if err != nil {
				stats.lastErr = err.Error()
				allFree = false
				break
			}
			ln.Close()
		}
		if allFree {
			stats.elapsedMs = time.Since(start).Milliseconds()
			return stats
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Timeout reached, proceed anyway and let chromium report the error
	stats.elapsedMs = time.Since(start).Milliseconds()
	stats.timedOut = true
	return stats
}

// killStats summarizes a single killExistingChromium invocation for the
// chromium_launcher_phase_complete instrumentation event.
type killStats struct {
	pkillElapsedMs     int64
	pgrepWaitElapsedMs int64
	pgrepAttempts      int
	timedOut           bool
}

// killExistingChromium kills any existing chromium browser processes and waits for them to die.
// This ensures a clean restart where the new process can bind to IPv4.
// Note: We use -x for exact match to avoid killing chromium-launcher itself.
func killExistingChromium() killStats {
	stats := killStats{}

	// Kill chromium processes by exact name match.
	// Using -x prevents matching "chromium-launcher" which would kill this process.
	pkillStart := time.Now()
	_ = exec.Command("pkill", "-9", "-x", "chromium").Run()
	stats.pkillElapsedMs = time.Since(pkillStart).Milliseconds()

	// Wait up to 2 seconds for processes to fully terminate
	pgrepStart := time.Now()
	deadline := pgrepStart.Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats.pgrepAttempts++
		// Check if any chromium browser processes are still running (exact match)
		output, err := exec.Command("pgrep", "-x", "chromium").Output()
		if err != nil || len(strings.TrimSpace(string(output))) == 0 {
			// No processes found, we're done
			stats.pgrepWaitElapsedMs = time.Since(pgrepStart).Milliseconds()
			return stats
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Timeout - processes may still exist but we continue anyway
	stats.pgrepWaitElapsedMs = time.Since(pgrepStart).Milliseconds()
	stats.timedOut = true
	fmt.Fprintf(os.Stderr, "warning: chromium processes may still be running after kill attempt\n")
	return stats
}

// readVMUptimeMs returns the VM uptime in milliseconds by parsing /proc/uptime.
// Returns 0 on any error. Read-only, fail-soft.
func readVMUptimeMs() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(secs * 1000)
}

// readLoadAvg1m returns the 1-minute load average from /proc/loadavg.
// Returns 0 on any error. Read-only, fail-soft.
func readLoadAvg1m() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// readMemInfo returns MemAvailable / Cached / Dirty in kB from /proc/meminfo.
// Any field that fails to parse returns 0. Read-only, fail-soft.
func readMemInfo() (availKb, cachedKb, dirtyKb int64) {
	availKb, cachedKb, dirtyKb = parseMemInfo("/proc/meminfo")
	return
}

func parseMemInfo(path string) (availKb, cachedKb, dirtyKb int64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := line[:idx]
		rest := strings.Fields(line[idx+1:])
		if len(rest) == 0 {
			continue
		}
		val, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemAvailable":
			availKb = val
		case "Cached":
			cachedKb = val
		case "Dirty":
			dirtyKb = val
		}
	}
	return
}

// countSocketStates returns counts of ESTABLISHED / TIME_WAIT / LISTEN sockets
// on 127.0.0.1:port by parsing /proc/net/tcp. Returns zeros on any error.
// /proc/net/tcp local_address column encodes IP and port as hex; the port is
// big-endian uppercase hex (e.g. 9223 = 0x2407 = ":2407").
func countSocketStates(port uint16) (estab, timewait, listen int) {
	estab, timewait, listen = parseSocketStates("/proc/net/tcp", port)
	return
}

func parseSocketStates(path string, port uint16) (estab, timewait, listen int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	portSuffix := fmt.Sprintf(":%04X", port)
	sc := bufio.NewScanner(f)
	headerSkipped := false
	for sc.Scan() {
		if !headerSkipped {
			headerSkipped = true
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		if !strings.HasSuffix(fields[1], portSuffix) {
			continue
		}
		switch fields[3] {
		case "01":
			estab++
		case "06":
			timewait++
		case "0A":
			listen++
		}
	}
	return
}

// emitPhaseCompleteEvent writes a single chromium_launcher_phase_complete JSON
// log line to stderr right before exec'ing chromium. This is the entry point
// for SigNoz aggregation against vm-logs / s2.stream.
func emitPhaseCompleteEvent(
	launcherStart time.Time,
	headless bool,
	internalPortNum uint16,
	ks killStats,
	wp waitportStats,
) {
	estab, timewait, listen := countSocketStates(internalPortNum)
	availKb, cachedKb, dirtyKb := readMemInfo()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	logger.Info("chromium_launcher_phase_complete",
		"instance_name", strings.TrimSpace(os.Getenv("INSTANCE_NAME")),
		"headless", headless,
		"internal_port", internalPortNum,
		"pkill_elapsed_ms", ks.pkillElapsedMs,
		"pgrep_wait_elapsed_ms", ks.pgrepWaitElapsedMs,
		"pgrep_attempts", ks.pgrepAttempts,
		"pgrep_timed_out", ks.timedOut,
		"waitport_elapsed_ms", wp.elapsedMs,
		"waitport_attempts", wp.attempts,
		"waitport_timed_out", wp.timedOut,
		"waitport_last_err", wp.lastErr,
		"ss_estab_devtools", estab,
		"ss_timewait_devtools", timewait,
		"ss_listen_devtools", listen,
		"vm_uptime_ms", readVMUptimeMs(),
		"load_avg_1m", readLoadAvg1m(),
		"mem_available_kb", availKb,
		"mem_cached_kb", cachedKb,
		"mem_dirty_kb", dirtyKb,
		"launcher_total_ms", time.Since(launcherStart).Milliseconds(),
		"exec_started_unix_ms", time.Now().UnixMilli(),
	)
}
