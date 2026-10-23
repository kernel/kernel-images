package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSetDesktopBackgroundStalledProbe guards the boot path: the background
// script runs synchronously between the X server coming up and openbox
// starting, so a stalled geometry probe must fall back within its advertised
// budget instead of blocking wrapper startup indefinitely.
func TestSetDesktopBackgroundStalledProbe(t *testing.T) {
	script := filepath.Join("..", "..", "..", "shared", "set-desktop-background.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("script not found: %v", err)
	}

	fake := t.TempDir()
	bin := filepath.Join(fake, "xwininfo")
	// A geometry probe that hangs forever: without the per-stage bound the
	// script would block for the full sleep instead of reaching the fallback.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BG_TIMEOUT=2",
		"DISPLAY=",
	)

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if elapsed > 8*time.Second {
		t.Fatalf("script ran for %v with a stalled probe; the per-stage timeout did not bound it", elapsed)
	}
	t.Logf("script fell back in %v (stalled probe bounded)", elapsed)
	_ = err // exit code is not asserted: the fallback chain fails without a display
}
