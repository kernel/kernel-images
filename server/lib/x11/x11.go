// Package x11 provides helpers for talking to a local X server.
package x11

import (
	"context"
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

// WaitForWindowManager blocks until an EWMH-compliant window manager has
// registered with the X server, returning time spent waiting. Chromium
// negotiates CSD (no WM titlebar) at window-map time; if the WM isn't up yet,
// it later reparents the already-mapped window with default SSD decoration and
// a titlebar appears.
//
// If the deadline elapses, WaitForWindowManager still returns; callers can
// compare the returned duration against timeout to detect a miss.
func WaitForWindowManager(timeout time.Duration) time.Duration {
	start := time.Now()
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		if windowManagerReady(deadline) {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

// windowManagerReady reports whether a window manager owns the display, by
// reading the _NET_SUPPORTING_WM_CHECK property EWMH requires every conforming
// WM to set on the root window. Both probes draw from the caller's deadline.
//
// This is deliberately WM-agnostic. Searching for a specific WM process by name
// turns into a full-timeout stall the moment the image ships a different WM.
// _NET_ACTIVE_WINDOW is not usable here: it only exists once a client window
// has mapped, which is after the interval this check has to cover.
func windowManagerReady(deadline time.Time) bool {
	id, ok := readSupportingWindowID(deadline, "-root")
	if !ok {
		return false
	}

	// EWMH requires the supporting window to carry the same property. Checking
	// the window itself rejects the stale root property left after a WM exits.
	childID, ok := readSupportingWindowID(deadline, "-id", id)
	return ok && childID == id
}

func readSupportingWindowID(deadline time.Time, args ...string) (string, bool) {
	args = append(args, "_NET_SUPPORTING_WM_CHECK")
	// Bound each probe so a wedged X server can't stall the poll past the
	// caller's deadline.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xprop", args...).Output()
	if err != nil {
		return "", false
	}
	return supportingWindowID(string(out))
}

// supportingWindowID extracts the window ID from `xprop
// _NET_SUPPORTING_WM_CHECK`. A set property prints `...: window id # 0x...`;
// an unset one prints `no such atom on any window.` and still exits 0, so the
// output has to be read rather than the exit code.
func supportingWindowID(out string) (string, bool) {
	_, id, found := strings.Cut(out, "window id # ")
	if !found {
		return "", false
	}
	id = strings.TrimSpace(id)
	if id == "" || id == "0x0" {
		return "", false
	}
	return id, true
}
