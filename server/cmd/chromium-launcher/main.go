package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/onkernel/kernel-images/server/lib/chromiumflags"
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

// waitForPort waits until the given port is available for binding on both IPv4 and IPv6.
// This handles the delay after SIGKILL before the kernel releases the socket.
// We disable SO_REUSEADDR to get an accurate check matching chromium's bind behavior.
func waitForPort(port string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	addrs := []string{"127.0.0.1:" + port, "[::1]:" + port}

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
		allFree := true
		for _, addr := range addrs {
			ln, err := lc.Listen(ctx, "tcp", addr)
			if err != nil {
				allFree = false
				break
			}
			ln.Close()
		}
		if allFree {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Timeout reached, proceed anyway and let chromium report the error
}

// killExistingChromium kills any existing chromium browser processes and waits for them to die.
// This ensures a clean restart where the new process can bind to both IPv4 and IPv6.
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
