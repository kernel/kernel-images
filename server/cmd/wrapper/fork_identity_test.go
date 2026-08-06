package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArmForkIdentityWaitClearsStaleState(t *testing.T) {
	dir := t.TempDir()
	oldPayloadFile := forkidentity.PayloadFile
	oldAppliedFile := forkidentity.AppliedFile
	oldReadyFile := forkidentity.ReadyFile
	forkidentity.PayloadFile = filepath.Join(dir, "fork-identity.json")
	forkidentity.AppliedFile = filepath.Join(dir, "fork-identity-applied")
	forkidentity.ReadyFile = filepath.Join(dir, "fork-identity-ready")
	t.Cleanup(func() {
		forkidentity.PayloadFile = oldPayloadFile
		forkidentity.AppliedFile = oldAppliedFile
		forkidentity.ReadyFile = oldReadyFile
	})

	require.NoError(t, os.WriteFile(forkidentity.PayloadFile, []byte("stale"), 0o600))
	require.NoError(t, os.WriteFile(forkidentity.AppliedFile, []byte("stale"), 0o644))

	armForkIdentityWait(true)

	_, err := os.Stat(forkidentity.PayloadFile)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(forkidentity.AppliedFile)
	require.ErrorIs(t, err, os.ErrNotExist)
	ready, err := os.ReadFile(forkidentity.ReadyFile)
	require.NoError(t, err)
	assert.Equal(t, "waiting\n", string(ready))
}

func TestForkIdentityAppliedMarkerRequiresEveryReadinessProbe(t *testing.T) {
	dir := t.TempDir()
	oldAppliedFile := forkidentity.AppliedFile
	forkidentity.AppliedFile = filepath.Join(dir, "fork-identity-applied")
	t.Cleanup(func() { forkidentity.AppliedFile = oldAppliedFile })

	probeNames := []string{"cdp", "chromedriver", "envoy"}
	for _, failedProbe := range probeNames {
		t.Run(failedProbe, func(t *testing.T) {
			probes := make([]probe, 0, len(probeNames))
			for _, name := range probeNames {
				ready := name != failedProbe
				probes = append(probes, probe{name: name, fn: func() bool { return ready }})
			}

			durations, allReady := waitProbesReady(time.Now(), probes, 50*time.Millisecond)
			require.False(t, allReady)
			assert.NotContains(t, durations, failedProbe)
			for _, name := range probeNames {
				if name != failedProbe {
					assert.Contains(t, durations, name)
				}
			}
			require.Error(t, writeForkIdentityAppliedMarker("browser-1", allReady))
			_, err := os.Stat(forkidentity.AppliedFile)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestForkIdentityAppliedMarkerWrittenAfterReadiness(t *testing.T) {
	dir := t.TempDir()
	oldAppliedFile := forkidentity.AppliedFile
	forkidentity.AppliedFile = filepath.Join(dir, "fork-identity-applied")
	t.Cleanup(func() { forkidentity.AppliedFile = oldAppliedFile })

	_, allReady := waitProbesReady(time.Now(), []probe{
		{name: "cdp", fn: func() bool { return true }},
		{name: "chromedriver", fn: func() bool { return true }},
		{name: "envoy", fn: func() bool { return true }},
	}, time.Second)
	require.True(t, allReady)
	require.NoError(t, writeForkIdentityAppliedMarker("browser-1", allReady))

	applied, err := os.ReadFile(forkidentity.AppliedFile)
	require.NoError(t, err)
	assert.Equal(t, "browser-1\n", string(applied))
}

func TestApplyForkIdentityPayloadSetsAndClearsEnv(t *testing.T) {
	t.Setenv("METRO_NAME", "old-metro")
	t.Setenv("S2_STREAM", "old-stream")
	t.Setenv("FUTURE_IDENTITY_FIELD_NAME", "old-future")

	err := applyForkIdentityPayload(forkidentity.Payload{
		"instance_name":               "browser-1",
		"metro_name":                  "iad",
		"xds_server":                  "xds.example.test",
		"kernel_instance_jwt":         "jwt",
		"metro_api_url":               "https://metro.example.test/browser/kernel",
		"session_intel_url":           "https://intel.example.test",
		"future_identity_field_name":  "future-value",
		"empty_future_identity_field": "",
	})
	require.NoError(t, err)

	assert.Equal(t, "browser-1", os.Getenv("INSTANCE_NAME"))
	assert.Equal(t, "browser-1", os.Getenv("INST_NAME"))
	assert.Equal(t, "iad", os.Getenv("METRO_NAME"))
	assert.Equal(t, "xds.example.test", os.Getenv("XDS_SERVER"))
	assert.Equal(t, "jwt", os.Getenv("KERNEL_INSTANCE_JWT"))
	assert.Equal(t, "https://metro.example.test/browser/kernel", os.Getenv("KERNEL_METRO_API_BASE_URL"))
	assert.Equal(t, "https://intel.example.test", os.Getenv("SESSION_INTEL_URL"))
	assert.Equal(t, "future-value", os.Getenv("FUTURE_IDENTITY_FIELD_NAME"))
	assert.Empty(t, os.Getenv("EMPTY_FUTURE_IDENTITY_FIELD"))
	assert.Empty(t, os.Getenv("S2_STREAM"))
}

func TestForkIdentityURLPrecedence(t *testing.T) {
	payload := forkidentity.Payload{
		"metro_api_url":             "https://legacy.example.test/browser/kernel",
		"kernel_metro_api_base_url": "https://metro.example.test/browser/kernel",
		"session_intel_url":         "https://intel.example.test",
	}

	assert.Equal(t, "https://metro.example.test/browser/kernel", forkidentity.MetroAPIURL(payload))
	assert.Equal(t, "https://intel.example.test", forkidentity.ExtensionAPIURL(payload))
}
