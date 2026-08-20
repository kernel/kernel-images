package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kernel/kernel-images/server/lib/chromiumflags"
	"github.com/kernel/kernel-images/server/lib/display"
)

func TestChromiumDisplaySetup(t *testing.T) {
	tests := []struct {
		name      string
		config    display.Config
		wantFlags []string
		wantEnv   []string
		wantErr   bool
	}{
		{
			name:    "x11",
			config:  display.Config{Backend: display.BackendX11, XDisplay: ":7"},
			wantEnv: []string{"DISPLAY=:7"},
		},
		{
			name:   "wayland",
			config: display.Config{Backend: display.BackendWayland, WaylandDisplay: "wayland-2", RuntimeDir: "/run/user/1000"},
			wantFlags: []string{
				"--enable-features=UseOzonePlatform",
				"--ozone-platform=wayland",
			},
			wantEnv: []string{"WAYLAND_DISPLAY=wayland-2", "XDG_RUNTIME_DIR=/run/user/1000"},
		},
		{
			name:    "wayland requires runtime directory",
			config:  display.Config{Backend: display.BackendWayland, WaylandDisplay: "wayland-0"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, env, err := chromiumDisplaySetup(tt.config)
			if tt.wantErr {
				if err == nil {
					t.Fatal("chromiumDisplaySetup() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("chromiumDisplaySetup() error = %v", err)
			}
			if !reflect.DeepEqual(flags, tt.wantFlags) {
				t.Fatalf("flags = %#v, want %#v", flags, tt.wantFlags)
			}
			if !reflect.DeepEqual(env, tt.wantEnv) {
				t.Fatalf("env = %#v, want %#v", env, tt.wantEnv)
			}
		})
	}
}

func TestWithDefaultPrivateNetworkBypass(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  []string
	}{
		{
			name: "image default",
			want: []string{defaultPrivateNetworkBypassFlag},
		},
		{
			name:  "default follows unrelated flags",
			flags: []string{"--kiosk"},
			want:  []string{"--kiosk", defaultPrivateNetworkBypassFlag},
		},
		{
			name:  "custom list replaces image default",
			flags: []string{"--proxy-bypass-list=preview.internal"},
			want:  []string{"--proxy-bypass-list=preview.internal"},
		},
		{
			name:  "explicit empty list clears image default",
			flags: []string{"--proxy-bypass-list="},
			want:  []string{"--proxy-bypass-list="},
		},
		{
			name:  "bare empty list clears image default",
			flags: []string{"--proxy-bypass-list"},
			want:  []string{"--proxy-bypass-list"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withDefaultPrivateNetworkBypass(tt.flags)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("withDefaultPrivateNetworkBypass() mismatch:\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}

func TestDefaultPrivateNetworkBypassPreservesRuntimePrecedence(t *testing.T) {
	configured := chromiumflags.MergeFlagsWithRuntimeTokens(
		"--proxy-bypass-list=preview.internal",
		[]string{defaultPrivateNetworkBypassFlag},
	)
	got := withDefaultPrivateNetworkBypass(configured)
	want := []string{
		"--proxy-bypass-list=preview.internal",
		defaultPrivateNetworkBypassFlag,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime precedence changed:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExecLookPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mybin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	oldPath := os.Getenv("PATH")
	defer func() { _ = os.Setenv("PATH", oldPath) }()
	if err := os.Setenv("PATH", dir); err != nil {
		t.Fatalf("set PATH: %v", err)
	}

	// lookPath should find by PATH
	if p, err := exec.LookPath("mybin"); err != nil || p != bin {
		t.Fatalf("lookPath failed: p=%q err=%v", p, err)
	}

	// execLookPath should return input when absolute
	if p, err := execLookPath(bin); err != nil || p != bin {
		t.Fatalf("execLookPath absolute failed: p=%q err=%v", p, err)
	}

	// execLookPath should resolve by PATH for bare names
	if p, err := execLookPath("mybin"); err != nil || p != bin {
		t.Fatalf("execLookPath PATH search failed: p=%q err=%v", p, err)
	}
}
