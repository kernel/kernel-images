package main

import (
	"path/filepath"
	"testing"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
)

// withForkIdentityFiles points the forkidentity package vars at temp paths for
// the duration of a test and restores them after.
func withForkIdentityFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	payload, applied := forkidentity.PayloadFile, forkidentity.AppliedFile
	forkidentity.PayloadFile = filepath.Join(dir, "fork-identity.json")
	forkidentity.AppliedFile = filepath.Join(dir, "fork-identity-applied")
	t.Cleanup(func() {
		forkidentity.PayloadFile = payload
		forkidentity.AppliedFile = applied
	})
}

func writeAppliedPayload(t *testing.T, jwt string) {
	t.Helper()
	p := forkidentity.Payload{
		"instance_name":       "browser-fork-1",
		"session_intel_url":   "https://intel.example",
		"kernel_instance_jwt": jwt,
	}
	if err := forkidentity.WritePayload(p); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := forkidentity.WriteAppliedMarker(p.InstanceName()); err != nil {
		t.Fatalf("write applied marker: %v", err)
	}
}

func TestInstanceJWTProvider(t *testing.T) {
	t.Run("non-fork returns boot jwt", func(t *testing.T) {
		withForkIdentityFiles(t)
		t.Setenv(forkidentity.WaitEnv, "")
		// Even with an applied payload present, non-fork mode ignores it.
		writeAppliedPayload(t, "payload-jwt")
		p := newInstanceJWTProvider("boot-jwt")
		if got := p.Token(); got != "boot-jwt" {
			t.Errorf("Token() = %q, want boot-jwt", got)
		}
	})

	t.Run("fork before apply falls back to boot jwt", func(t *testing.T) {
		withForkIdentityFiles(t)
		t.Setenv(forkidentity.WaitEnv, "true")
		// No payload/applied marker written yet: mid-apply, must not read a torn value.
		p := newInstanceJWTProvider("boot-jwt")
		if got := p.Token(); got != "boot-jwt" {
			t.Errorf("Token() = %q, want boot-jwt", got)
		}
	})

	t.Run("fork after apply returns fresh payload jwt", func(t *testing.T) {
		withForkIdentityFiles(t)
		t.Setenv(forkidentity.WaitEnv, "true")
		writeAppliedPayload(t, "fresh-jwt")
		// Boot jwt is empty, mirroring an unbound fork; the fresh credential must
		// come from the applied payload.
		p := newInstanceJWTProvider("")
		if got := p.Token(); got != "fresh-jwt" {
			t.Errorf("Token() = %q, want fresh-jwt", got)
		}
	})
}
