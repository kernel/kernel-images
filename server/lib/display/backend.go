// Package display contains configuration shared by the display lifecycle and
// browser launcher. Backend selection is explicit so a failed Wayland setup
// cannot silently continue on X11.
package display

import (
	"fmt"
	"os"
	"strings"
)

// Backend identifies the display protocol used by the headful image.
type Backend string

const (
	BackendX11     Backend = "x11"
	BackendWayland Backend = "wayland"
)

// Config contains the process environment needed to connect to a display.
type Config struct {
	Backend        Backend
	XDisplay       string
	WaylandDisplay string
	RuntimeDir     string
}

// FromEnv reads display backend configuration. X11 remains the default for
// compatibility with existing images and callers.
func FromEnv() (Config, error) {
	backend, err := ParseBackend(os.Getenv("DISPLAY_BACKEND"))
	if err != nil {
		return Config{}, err
	}

	config := Config{
		Backend:        backend,
		XDisplay:       strings.TrimSpace(os.Getenv("DISPLAY")),
		WaylandDisplay: strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")),
		RuntimeDir:     strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")),
	}
	if config.XDisplay == "" {
		config.XDisplay = ":1"
	}
	if config.WaylandDisplay == "" {
		config.WaylandDisplay = "wayland-0"
	}
	return config, nil
}

// ParseBackend parses DISPLAY_BACKEND. An empty value selects X11.
func ParseBackend(value string) (Backend, error) {
	backend := Backend(strings.ToLower(strings.TrimSpace(value)))
	if backend == "" {
		return BackendX11, nil
	}
	switch backend {
	case BackendX11, BackendWayland:
		return backend, nil
	default:
		return "", fmt.Errorf("unsupported DISPLAY_BACKEND %q (expected x11 or wayland)", value)
	}
}
