// Package x11 provides helpers for talking to a local X server.
package x11

import (
	"bufio"
	"context"
	"net"
	"os/exec"
	"strconv"
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

// RootSize reads the root window geometry from xwininfo. The headful
// launcher uses it to size chromium's startup bounds to the display (see
// withSeededRestoreBounds in cmd/chromium-launcher). Bounded by timeout so a
// wedged X server can't stall the caller.
func RootSize(display string, timeout time.Duration) (int, int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xwininfo", "-display", display, "-root").Output()
	if err != nil {
		return 0, 0, false
	}
	return rootSize(string(out))
}

// rootSize parses the "  Width:  W" / "  Height: H" lines from xwininfo's
// root-window dump. Both lines must parse for the size to be reported.
func rootSize(out string) (int, int, bool) {
	width, height := 0, 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := leadingInt(line, "Width:"); ok {
			width = v
		}
		if v, ok := leadingInt(line, "Height:"); ok {
			height = v
		}
	}
	if width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// leadingInt parses the integer after prefix in line, e.g. "Width: 1920".
func leadingInt(line, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return 0, false
	}
	return v, true
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

// ActiveWindowID reads the root window's _NET_ACTIVE_WINDOW property, which
// openbox points at the browser window once chromium maps it. Returns false
// while no client window is mapped yet.
func ActiveWindowID(display string, timeout time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xprop", "-display", display, "-root", "_NET_ACTIVE_WINDOW").Output()
	if err != nil {
		return "", false
	}
	return activeWindowID(string(out))
}

// activeWindowID extracts the window ID from `xprop -root
// _NET_ACTIVE_WINDOW`, which prints `...: window id # 0x...` when a client
// window has focus and `0x0` when none does.
func activeWindowID(out string) (string, bool) {
	_, id, found := strings.Cut(out, "window id # ")
	if !found {
		return "", false
	}
	id, _, _ = strings.Cut(strings.TrimSpace(id), ",")
	if id == "" || id == "0x0" {
		return "", false
	}
	return id, true
}

// WatchWindowCoverage reports whether the window covers the display, once for
// the state it is in when the watch starts and again on every later change.
// It returns when ctx is cancelled, when the window goes away, or when xprop
// cannot be run.
//
// The signal is _NET_WM_STATE, which openbox rewrites the moment the window
// leaves or re-enters the maximized state. Watching the property costs one
// long-lived xprop process per browser start and reports a change as it
// happens, where polling the state over CDP costs a round trip per pass and
// reports it up to one interval late. A window manager that does not maintain
// the EWMH atoms would simply never call onCoverage; the promotion on browser
// start is what guarantees the window starts covered.
func WatchWindowCoverage(ctx context.Context, display, windowID string, onCoverage func(covered bool)) error {
	cmd := exec.CommandContext(ctx, "xprop", "-display", display, "-spy", "-id", windowID, "_NET_WM_STATE")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if covered, ok := windowCovered(scanner.Text()); ok {
			onCoverage(covered)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return scanner.Err()
}

// windowCovered reads one _NET_WM_STATE line from xprop. A window covers the
// display when it is fullscreen (kiosk) or maximized in both directions;
// maximized in one direction leaves desktop visible beside it, so it counts as
// uncovered. The second return reports whether the line was a state line at
// all, so unrelated xprop output is ignored rather than read as a demotion.
func windowCovered(line string) (bool, bool) {
	_, atoms, found := strings.Cut(line, "_NET_WM_STATE(ATOM) =")
	if !found {
		return false, false
	}
	if strings.Contains(atoms, "_NET_WM_STATE_FULLSCREEN") {
		return true, true
	}
	return strings.Contains(atoms, "_NET_WM_STATE_MAXIMIZED_VERT") &&
		strings.Contains(atoms, "_NET_WM_STATE_MAXIMIZED_HORZ"), true
}
