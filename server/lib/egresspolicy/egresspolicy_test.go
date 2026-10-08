package egresspolicy

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "egress-policy.json")
}

// A session nothing has been applied to behaves as it did before the policy
// existed.
func TestLoadWithoutStateIsUnfiltered(t *testing.T) {
	if Load(statePath(t), silent()).Filtered() {
		t.Fatal("a VM with no policy reported itself filtered")
	}
}

// supervisord restarts the API process automatically. A restart that came back
// unfiltered would drop the refusal while the allowlist was still enforced at
// the proxy, which is invisible from outside the VM.
func TestPolicySurvivesProcessRestart(t *testing.T) {
	path := statePath(t)
	applied := Load(path, silent())
	if err := applied.Set(Policy{Filtered: true}); err != nil {
		t.Fatalf("set filtered: %v", err)
	}

	restarted := Load(path, silent())
	if !restarted.Filtered() {
		t.Fatal("policy did not survive a restart")
	}

	if err := restarted.Set(Policy{}); err != nil {
		t.Fatalf("clear filtered: %v", err)
	}
	if Load(path, silent()).Filtered() {
		t.Fatal("a removed allowlist came back filtered after a restart")
	}
}

// Resolving an unreadable security control in the direction that switches it
// off is the one answer that cannot be right.
func TestLoadWithUnreadableStateIsFiltered(t *testing.T) {
	path := statePath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	if !Load(path, silent()).Filtered() {
		t.Fatal("a policy that could not be decoded was treated as unfiltered")
	}
}

// A policy that was not written down is not applied, so the control plane is
// told it failed rather than assuming a refusal is in place.
func TestSetFailsWhenItCannotPersist(t *testing.T) {
	dir := t.TempDir()
	state := Load(filepath.Join(dir, "egress-policy.json"), silent())
	// Take away the right to write the file, leaving the path itself valid.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := state.Set(Policy{Filtered: true}); err == nil {
		t.Fatal("a policy that could not be persisted reported success")
	}
	if state.Filtered() {
		t.Fatal("a policy that could not be persisted was applied in memory")
	}
}

// The launcher pins the private hosts it reads back, so a restart must keep
// "no list" and "an empty list" apart: the first keeps the image's default
// bypass, the second bypasses nothing.
func TestPrivateHostsSurviveProcessRestart(t *testing.T) {
	hosts := []string{"10.1.0.0/16", "preview.internal:8443"}
	for _, want := range []Policy{
		{Filtered: true},
		{Filtered: true, PrivateHosts: &[]string{}},
		{Filtered: true, PrivateHosts: &hosts},
	} {
		path := statePath(t)
		if err := Load(path, silent()).Set(want); err != nil {
			t.Fatalf("Set(%+v): %v", want, err)
		}
		if got := Load(path, silent()).Policy(); !got.Equal(want) {
			t.Fatalf("policy after restart = %+v, want %+v", got, want)
		}
	}
}
