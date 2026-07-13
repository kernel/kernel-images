package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/kernel-images/server/lib/chromiumflags"
	"github.com/kernel/kernel-images/server/lib/x11"
)

// shared/start-pulseaudio.sh is the authority for the audio topology. These are
// the fixed contract values it creates, not overridable defaults: chromium must
// connect to the same socket and play into the same sink the daemon sets up.
// Keep them in sync with start-pulseaudio.sh.
const (
	// pulseServer is the PulseAudio socket the recorder and chromium share.
	pulseServer = "unix:/tmp/pulse/native"
	// pulseSink is the null sink chromium plays into; the recorder captures
	// its .monitor source.
	pulseSink = "KernelOutput"
)

func main() {
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
	killExistingChromium()

	// Inputs
	internalPort := strings.TrimSpace(os.Getenv("INTERNAL_PORT"))
	if internalPort == "" {
		internalPort = "9223"
	}

	// Wait for devtools port to be available (handles SIGKILL socket cleanup delay)
	waitForPort(internalPort, 5*time.Second)

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

	// Prepare environment. PULSE_SERVER/PULSE_SINK route chromium's audio into the
	// recorder's sink; the root path below relies on this inherited env, while the
	// non-root path re-asserts them in its runuser env allowlist.
	env := os.Environ()
	env = append(env,
		"DISPLAY=:1",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket",
		"PULSE_SERVER="+pulseServer,
		"PULSE_SINK="+pulseSink,
	)

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
	// PULSE_SERVER tells libpulse which daemon socket to connect to; without it
	// chromium-as-kernel-user can't reach the recorder's PulseAudio instance and
	// has no audio output at all. PULSE_SINK then selects which sink within that
	// daemon playback lands on: Chromium's AudioManagerPulse honors it to redirect
	// playback into KernelOutput (see media/audio/pulse/audio_manager_pulse.cc
	// GetDefaultOutputDeviceID), which is the sink the recorder captures.
	inner := []string{
		"env",
		"DISPLAY=:1",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket",
		"PULSE_SERVER=" + pulseServer,
		"PULSE_SINK=" + pulseSink,
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

// waitForPort waits until the devtools port can be bound on IPv4. After SIGKILL
// the old listener may linger briefly; this loop waits for that to clear.
// Go's net.Listen sets SO_REUSEADDR, matching chromium's DevTools bind — so
// TIME_WAIT sockets from prior CDP client connections do not block the probe.
// Only IPv4 is checked because IPv6 is disabled at the kernel level in the VM.
func waitForPort(port string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	addr := "127.0.0.1:" + port

	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Timeout reached, proceed anyway and let chromium report the error
}

// killExistingChromium kills any existing chromium browser processes and waits for them to die.
// This ensures a clean restart where the new process can bind to IPv4.
// Note: We use -x for exact match to avoid killing chromium-launcher itself.
func killExistingChromium() {
	// Kill chromium processes by exact name match.
	// Using -x prevents matching "chromium-launcher" which would kill this process.
	_ = exec.Command("pkill", "-9", "-x", "chromium").Run()

	// Wait up to 2 seconds for processes to fully terminate
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// Check if any chromium browser processes are still running (exact match)
		output, err := exec.Command("pgrep", "-x", "chromium").Output()
		if err != nil || len(strings.TrimSpace(string(output))) == 0 {
			// No processes found, we're done
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Timeout - processes may still exist but we continue anyway
	fmt.Fprintf(os.Stderr, "warning: chromium processes may still be running after kill attempt\n")
}
