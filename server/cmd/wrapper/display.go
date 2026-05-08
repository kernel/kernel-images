package main

import (
	"net"
	"strings"
	"time"
)

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
