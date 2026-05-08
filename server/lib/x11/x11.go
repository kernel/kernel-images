// Package x11 provides helpers for talking to a local X server.
package x11

import (
	"net"
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
