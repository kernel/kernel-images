package main

import (
	"os"
	"testing"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
