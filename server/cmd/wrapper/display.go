package main

import (
	"time"

	"github.com/kernel/kernel-images/server/lib/x11"
)

// waitForX blocks until the X server is reachable on display :N. Logs a
// warning if the wait times out.
func waitForX(display string, timeout time.Duration) {
	if d := x11.WaitForDisplay(display, timeout); d >= timeout {
		logf("WARNING: X display %s not responsive after %s", display, timeout)
	}
}
