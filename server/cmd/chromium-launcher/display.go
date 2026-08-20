package main

import (
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/kernel/kernel-images/server/lib/display"
)

func chromiumDisplaySetup(config display.Config) (flags, env []string, err error) {
	switch config.Backend {
	case display.BackendX11:
		return nil, []string{"DISPLAY=" + config.XDisplay}, nil
	case display.BackendWayland:
		if config.RuntimeDir == "" {
			return nil, nil, fmt.Errorf("XDG_RUNTIME_DIR is required for Wayland")
		}
		return []string{
				"--enable-features=UseOzonePlatform",
				"--ozone-platform=wayland",
				"--start-fullscreen",
			}, []string{
				"WAYLAND_DISPLAY=" + config.WaylandDisplay,
				"XDG_RUNTIME_DIR=" + config.RuntimeDir,
			}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported display backend %q", config.Backend)
	}
}

func displayConfigFromEnv() (display.Config, []string, []string, error) {
	config, err := display.FromEnv()
	if err != nil {
		return display.Config{}, nil, nil, err
	}
	flags, env, err := chromiumDisplaySetup(config)
	if err != nil {
		return display.Config{}, nil, nil, err
	}
	return config, flags, env, nil
}

func waitForDisplay(config display.Config, timeout time.Duration) time.Duration {
	if config.Backend == display.BackendX11 {
		return waitForXDisplay(config.XDisplay, timeout)
	}
	return waitForWayland(config, timeout)
}

func waitForXDisplay(displayName string, timeout time.Duration) time.Duration {
	start := time.Now()
	num := displayName
	if len(num) > 0 && num[0] == ':' {
		num = num[1:]
	}
	named := "/tmp/.X11-unix/X" + num
	abstract := "@/tmp/.X11-unix/X" + num
	return waitForUnixSocket(start, []string{named, abstract}, timeout)
}

func waitForWayland(config display.Config, timeout time.Duration) time.Duration {
	start := time.Now()
	socket := filepath.Join(config.RuntimeDir, config.WaylandDisplay)
	return waitForUnixSocket(start, []string{socket}, timeout)
}

func waitForUnixSocket(start time.Time, sockets []string, timeout time.Duration) time.Duration {
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		for _, socket := range sockets {
			if conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond); err == nil {
				_ = conn.Close()
				return time.Since(start)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(start)
}
