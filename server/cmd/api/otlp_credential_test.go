package main

import (
	"log/slog"
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

func writeAppliedPayload(t *testing.T, jwt, instanceName, metro string) {
	t.Helper()
	p := forkidentity.Payload{
		"instance_name":       instanceName,
		"metro_name":          metro,
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

func bootIdentity() otlpIdentity {
	return otlpIdentity{jwt: "boot-jwt", instanceName: "boot-inst", metro: "boot-metro"}
}

func TestOTLPIdentityProvider(t *testing.T) {
	t.Run("non-fork returns boot identity", func(t *testing.T) {
		withForkIdentityFiles(t)
		t.Setenv(forkidentity.WaitEnv, "")
		writeAppliedPayload(t, "payload-jwt", "payload-inst", "payload-metro") // ignored in non-fork
		p := newOTLPIdentityProvider(bootIdentity(), slog.Default())
		if p.Token() != "boot-jwt" || p.InstanceName() != "boot-inst" || p.Metro() != "boot-metro" {
			t.Errorf("got (%q,%q,%q), want boot values", p.Token(), p.InstanceName(), p.Metro())
		}
	})

	t.Run("fork before apply falls back to boot", func(t *testing.T) {
		withForkIdentityFiles(t)
		t.Setenv(forkidentity.WaitEnv, "true")
		p := newOTLPIdentityProvider(bootIdentity(), slog.Default())
		if p.Token() != "boot-jwt" || p.InstanceName() != "boot-inst" {
			t.Errorf("got (%q,%q), want boot fallback", p.Token(), p.InstanceName())
		}
	})

	t.Run("fork after apply returns fresh payload identity", func(t *testing.T) {
		withForkIdentityFiles(t)
		t.Setenv(forkidentity.WaitEnv, "true")
		// Empty boot values mirror an unbound fork; the fresh identity comes from
		// the applied payload.
		p := newOTLPIdentityProvider(otlpIdentity{}, slog.Default())
		writeAppliedPayload(t, "fresh-jwt", "fresh-inst", "fresh-metro")
		if p.Token() != "fresh-jwt" || p.InstanceName() != "fresh-inst" || p.Metro() != "fresh-metro" {
			t.Errorf("got (%q,%q,%q), want fresh payload values", p.Token(), p.InstanceName(), p.Metro())
		}
	})

	t.Run("boot fallback is not cached; apply is picked up next call", func(t *testing.T) {
		withForkIdentityFiles(t)
		t.Setenv(forkidentity.WaitEnv, "true")
		p := newOTLPIdentityProvider(bootIdentity(), slog.Default())
		// Pre-apply: boot fallback, and must not be cached.
		if got := p.Token(); got != "boot-jwt" {
			t.Fatalf("pre-apply Token() = %q, want boot-jwt", got)
		}
		// Apply lands; the very next call must reflect it despite the cache TTL.
		writeAppliedPayload(t, "applied-jwt", "applied-inst", "applied-metro")
		if got := p.Token(); got != "applied-jwt" {
			t.Errorf("post-apply Token() = %q, want applied-jwt (boot fallback must not be cached)", got)
		}
	})
}
