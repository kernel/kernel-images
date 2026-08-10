package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMergeChromiumFlags(t *testing.T) {
	tests := []struct {
		name          string
		baseFlags     string
		runtimeTokens []string
		want          []string
	}{
		{
			name: "image default",
			want: []string{defaultPrivateNetworkBypassFlag},
		},
		{
			name:      "base list replaces image default",
			baseFlags: "--proxy-bypass-list=preview.internal",
			want: []string{
				defaultPrivateNetworkBypassFlag,
				"--proxy-bypass-list=preview.internal",
			},
		},
		{
			name:          "runtime list replaces image and base lists",
			baseFlags:     "--proxy-bypass-list=base.internal --kiosk",
			runtimeTokens: []string{"--proxy-bypass-list=runtime.internal"},
			want: []string{
				defaultPrivateNetworkBypassFlag,
				"--proxy-bypass-list=base.internal",
				"--kiosk",
				"--proxy-bypass-list=runtime.internal",
			},
		},
		{
			name:          "explicit empty runtime list clears image default",
			runtimeTokens: []string{"--proxy-bypass-list="},
			want: []string{
				defaultPrivateNetworkBypassFlag,
				"--proxy-bypass-list=",
			},
		},
		{
			name:      "duplicate image default is removed",
			baseFlags: defaultPrivateNetworkBypassFlag,
			want:      []string{defaultPrivateNetworkBypassFlag},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeChromiumFlags(tt.baseFlags, tt.runtimeTokens)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mergeChromiumFlags() mismatch:\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
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
