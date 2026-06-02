package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveBaseFlags(t *testing.T) {
	// Mirrors a real flag string: spaces, a quoted value, and comma lists —
	// exactly the characters a cmdline encoder may escape.
	flags := `--no-sandbox --lang="en-US,en;q=0.9" --disable-features=A,B,C`
	enc := base64.RawURLEncoding.EncodeToString([]byte(flags))

	tests := []struct {
		name    string
		plain   string
		encoded string
		want    string
		wantErr bool
	}{
		{name: "plain only", plain: flags, encoded: "", want: flags},
		{name: "encoded preferred over plain", plain: "--corrupted\\ value", encoded: enc, want: flags},
		{name: "blank encoded falls back to plain", plain: flags, encoded: "   ", want: flags},
		{name: "both empty", plain: "", encoded: "", want: ""},
		{name: "malformed encoded falls back to plain", plain: flags, encoded: "not!base64!", want: flags, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBaseFlags(tt.plain, tt.encoded)
			if got != tt.want {
				t.Errorf("resolveBaseFlags(%q, %q) = %q, want %q", tt.plain, tt.encoded, got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveBaseFlags(%q, %q) err = %v, wantErr %v", tt.plain, tt.encoded, err, tt.wantErr)
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
