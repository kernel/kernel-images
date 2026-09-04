package display

import (
	"os"
	"testing"
)

func TestParseBackend(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  Backend
		ok    bool
	}{
		{name: "empty defaults to x11", want: BackendX11, ok: true},
		{name: "x11", value: "x11", want: BackendX11, ok: true},
		{name: "wayland", value: "wayland", want: BackendWayland, ok: true},
		{name: "case and whitespace", value: " WayLand ", want: BackendWayland, ok: true},
		{name: "unknown", value: "mir", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBackend(tt.value)
			if tt.ok {
				if err != nil {
					t.Fatalf("ParseBackend() error = %v", err)
				}
				if got != tt.want {
					t.Fatalf("ParseBackend() = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatal("ParseBackend() error = nil, want error")
			}
		})
	}
}

func TestFromEnvDefaultsAndOverrides(t *testing.T) {
	env := map[string]string{
		"DISPLAY_BACKEND": "wayland",
		"DISPLAY":         ":7",
		"WAYLAND_DISPLAY": "wayland-2",
		"XDG_RUNTIME_DIR": "/run/user/1000",
	}
	for key, value := range env {
		t.Setenv(key, value)
	}

	got, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if got.Backend != BackendWayland || got.XDisplay != ":7" || got.WaylandDisplay != "wayland-2" || got.RuntimeDir != "/run/user/1000" {
		t.Fatalf("FromEnv() = %#v", got)
	}
}

func TestFromEnvDefaults(t *testing.T) {
	for _, key := range []string{"DISPLAY_BACKEND", "DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR"} {
		t.Setenv(key, "")
	}

	got, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	want := Config{Backend: BackendX11, XDisplay: ":1", WaylandDisplay: "wayland-0"}
	if got != want {
		t.Fatalf("FromEnv() = %#v, want %#v", got, want)
	}

	_ = os.Unsetenv("DISPLAY_BACKEND")
}
