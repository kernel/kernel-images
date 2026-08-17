package main

import (
	"os"
	"strings"
	"time"
)

const (
	waylandRuntimeDir = "/tmp/runtime-kernel"
	waylandDisplay    = "wayland-1"
)

func nestedWaylandEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("KERNEL_WAYLAND_NESTED")), "true")
}

func pureWaylandEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("KERNEL_WAYLAND_PURE")), "true")
}

func configureWaylandEnv() {
	_ = os.Setenv("XDG_RUNTIME_DIR", waylandRuntimeDir)
	_ = os.Setenv("WAYLAND_DISPLAY", waylandDisplay)
}

func waitForWayland(timeout time.Duration) {
	waitForSocket(waylandRuntimeDir+"/"+waylandDisplay, timeout)
}
