// Package x11 provides helpers for talking to a local X server.
package x11

import (
	"net"
	"os/exec"
	"strings"
	"time"
)

// WaitForDisplay blocks until the X server is reachable on display :N, returning
// the time spent waiting. It tries both the named unix socket (Xorg, headful)
// and the abstract namespace socket (Xvfb runs with -nolisten unix, which
// disables the named socket but leaves the abstract one). Cheaper than spawning
// xdpyinfo in a loop.
//
// If the deadline elapses, WaitForDisplay still returns; callers can compare
// the returned duration against timeout to detect a miss.
func WaitForDisplay(display string, timeout time.Duration) time.Duration {
	start := time.Now()
	num := strings.TrimPrefix(display, ":")
	named := "/tmp/.X11-unix/X" + num
	abstract := "@/tmp/.X11-unix/X" + num // Linux abstract namespace
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", named, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return time.Since(start)
		}
		if c, err := net.DialTimeout("unix", abstract, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(start)
}

// WaitForMutter blocks until the mutter window manager has registered with
// the X server, returning time spent waiting. Chromium negotiates CSD (no WM
// titlebar) at window-map time; if mutter isn't up yet, it later reparents the
// already-mapped window with default SSD decoration and a titlebar appears.
// Polls `xdotool search --class mutter` to match the wrapper's readiness check.
//
// If the deadline elapses, WaitForMutter still returns; callers can compare
// the returned duration against timeout to detect a miss.
func WaitForMutter(timeout time.Duration) time.Duration {
	start := time.Now()
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		if exec.Command("xdotool", "search", "--class", "mutter").Run() == nil {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}
