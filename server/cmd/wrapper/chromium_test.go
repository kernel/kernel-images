package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestApplyHeadlessDefaultFlags(t *testing.T) {
	t.Setenv("CHROMIUM_FLAGS", "")
	applyHeadlessDefaultFlags()
	flags := strings.Fields(os.Getenv("CHROMIUM_FLAGS"))
	for _, flag := range []string{"--disable-search-engine-choice-screen", "--silent-debugger-extension-api"} {
		if !slices.Contains(flags, flag) {
			t.Errorf("default flags missing %s", flag)
		}
	}
}

func TestApplyHeadlessDefaultFlagsPreservesOverride(t *testing.T) {
	const flags = "--headless --no-sandbox"
	t.Setenv("CHROMIUM_FLAGS", flags)
	applyHeadlessDefaultFlags()
	if got := os.Getenv("CHROMIUM_FLAGS"); got != flags {
		t.Errorf("CHROMIUM_FLAGS = %q, want %q", got, flags)
	}
}
