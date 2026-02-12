package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

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

func TestHasAppFlag(t *testing.T) {
	assert.False(t, hasAppFlag([]string{}), "empty args")
	assert.False(t, hasAppFlag([]string{"--no-first-run", "--disable-gpu"}), "no app flag")
	assert.True(t, hasAppFlag([]string{"--no-first-run", "--app=about:blank"}), "--app=about:blank")
	assert.True(t, hasAppFlag([]string{"--app=https://example.com"}), "--app=URL")
	assert.True(t, hasAppFlag([]string{"--app"}), "bare --app")
	assert.False(t, hasAppFlag([]string{"--application-name=foo"}), "--application-name should not match")
}
