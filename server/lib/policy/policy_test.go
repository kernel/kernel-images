package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicy_PreservesUnknownFields(t *testing.T) {
	// Simulate the policy.json with many fields not in the struct
	// Only ExtensionInstallForcelist and ExtensionSettings are in the struct
	input := `{
		"PasswordManagerEnabled": false,
		"AutofillCreditCardEnabled": false,
		"TranslateEnabled": false,
		"DefaultNotificationsSetting": 2,
		"DefaultGeolocationSetting": 2,
		"DefaultSearchProviderEnabled": true,
		"DefaultSearchProviderName": "DuckDuckGo",
		"DefaultSearchProviderSearchURL": "https://duckduckgo.com/?q={searchTerms}",
		"DefaultSearchProviderSuggestURL": "https://duckduckgo.com/ac/?q={searchTerms}",
		"NewTabPageLocation": "https://start.duckduckgo.com/",
		"SomeOtherUnknownField": {"nested": "value"},
		"ExtensionSettings": {
			"*": {
				"allowed_types": ["extension"],
				"install_sources": ["*"]
			}
		}
	}`

	var policy Policy
	err := json.Unmarshal([]byte(input), &policy)
	require.NoError(t, err)

	// Verify known struct fields are parsed correctly
	assert.NotNil(t, policy.ExtensionSettings)
	assert.Contains(t, policy.ExtensionSettings, "*")

	// Verify all non-struct fields are captured as unknown
	// All fields except ExtensionSettings should be in unknownFields
	expectedUnknownFields := []string{
		"PasswordManagerEnabled",
		"AutofillCreditCardEnabled",
		"TranslateEnabled",
		"DefaultNotificationsSetting",
		"DefaultGeolocationSetting",
		"DefaultSearchProviderEnabled",
		"DefaultSearchProviderName",
		"DefaultSearchProviderSearchURL",
		"DefaultSearchProviderSuggestURL",
		"NewTabPageLocation",
		"SomeOtherUnknownField",
	}
	for _, field := range expectedUnknownFields {
		assert.Contains(t, policy.unknownFields, field, "field %s should be preserved", field)
	}

	// Now marshal it back
	output, err := json.Marshal(&policy)
	require.NoError(t, err)

	// Unmarshal into a map to verify all fields are present
	var result map[string]interface{}
	err = json.Unmarshal(output, &result)
	require.NoError(t, err)

	// Verify all fields are preserved
	assert.Equal(t, float64(2), result["DefaultGeolocationSetting"])
	assert.Equal(t, float64(2), result["DefaultNotificationsSetting"])
	assert.Equal(t, false, result["PasswordManagerEnabled"])
	assert.Equal(t, "DuckDuckGo", result["DefaultSearchProviderName"])
	assert.NotNil(t, result["SomeOtherUnknownField"])
}

func TestPolicy_ModifyAndPreserveUnknownFields(t *testing.T) {
	// This test simulates the AddExtension read-modify-write cycle
	input := `{
		"PasswordManagerEnabled": false,
		"DefaultGeolocationSetting": 2,
		"DefaultNotificationsSetting": 2,
		"ExtensionSettings": {}
	}`

	var policy Policy
	err := json.Unmarshal([]byte(input), &policy)
	require.NoError(t, err)

	// Add an extension setting (this is what AddExtension does)
	policy.ExtensionSettings["test-extension"] = ExtensionSetting{
		UpdateUrl: "http://127.0.0.1:10001/extensions/test/update.xml",
	}

	// Marshal it back
	output, err := json.Marshal(&policy)
	require.NoError(t, err)

	// Verify the unknown fields are still there
	var result map[string]interface{}
	err = json.Unmarshal(output, &result)
	require.NoError(t, err)

	assert.Equal(t, float64(2), result["DefaultGeolocationSetting"], "DefaultGeolocationSetting should be preserved")
	assert.Equal(t, float64(2), result["DefaultNotificationsSetting"], "DefaultNotificationsSetting should be preserved")
	assert.Equal(t, false, result["PasswordManagerEnabled"], "PasswordManagerEnabled should be preserved")

	// Verify extension was added
	extSettings, ok := result["ExtensionSettings"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, extSettings, "test-extension")
}

func TestPolicy_ExtensionInstallForcelist(t *testing.T) {
	// Test that ExtensionInstallForcelist works correctly
	input := `{
		"DefaultGeolocationSetting": 2,
		"ExtensionInstallForcelist": ["existing-ext;http://example.com/update.xml"],
		"ExtensionSettings": {}
	}`

	var policy Policy
	err := json.Unmarshal([]byte(input), &policy)
	require.NoError(t, err)

	// Verify forcelist is parsed
	assert.Len(t, policy.ExtensionInstallForcelist, 1)
	assert.Equal(t, "existing-ext;http://example.com/update.xml", policy.ExtensionInstallForcelist[0])

	// Add to forcelist
	policy.ExtensionInstallForcelist = append(policy.ExtensionInstallForcelist, "new-ext;http://example.com/new.xml")

	// Marshal back
	output, err := json.Marshal(&policy)
	require.NoError(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(output, &result)
	require.NoError(t, err)

	// Both unknown field and forcelist should be present
	assert.Equal(t, float64(2), result["DefaultGeolocationSetting"])
	forcelist, ok := result["ExtensionInstallForcelist"].([]interface{})
	require.True(t, ok)
	assert.Len(t, forcelist, 2)
}

func TestPolicy_EmptyPolicy(t *testing.T) {
	// Test with minimal input
	input := `{}`

	var policy Policy
	err := json.Unmarshal([]byte(input), &policy)
	require.NoError(t, err)

	assert.Empty(t, policy.unknownFields)
	assert.Nil(t, policy.ExtensionSettings)
	assert.Nil(t, policy.ExtensionInstallForcelist)

	output, err := json.Marshal(&policy)
	require.NoError(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(output, &result)
	require.NoError(t, err)

	// Should have empty ExtensionSettings as null/missing
	assert.Nil(t, result["ExtensionSettings"])
}

func TestManifestVersion(t *testing.T) {
	dir := t.TempDir()
	write := func(name, contents string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
		return path
	}

	t.Run("manifest v3", func(t *testing.T) {
		version, found, err := ManifestVersion(write("mv3.json", `{"manifest_version": 3, "name": "x"}`))
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, 3, version)
	})

	t.Run("manifest v2", func(t *testing.T) {
		version, found, err := ManifestVersion(write("mv2.json", `{"manifest_version": 2, "name": "x"}`))
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, 2, version)
	})

	t.Run("missing file is not an error", func(t *testing.T) {
		_, found, err := ManifestVersion(filepath.Join(dir, "does-not-exist.json"))
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("invalid json", func(t *testing.T) {
		_, _, err := ManifestVersion(write("bad.json", `{not json`))
		require.Error(t, err)
	})
}
