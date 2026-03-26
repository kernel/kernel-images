package policy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChromiumPolicyOverrides_Validate_ValidPolicies(t *testing.T) {
	overrides := ChromiumPolicyOverrides{
		"DefaultCookiesSetting":  json.RawMessage(`1`),
		"BasicAuthOverHttpEnabled": json.RawMessage(`true`),
		"HttpsUpgradesEnabled":   json.RawMessage(`false`),
		"NewTabPageLocation":     json.RawMessage(`"https://example.com"`),
		"PopupsAllowedForUrls":   json.RawMessage(`["https://example.com", "https://test.com"]`),
		"MaxConnectionsPerProxy": json.RawMessage(`32`),
	}

	err := overrides.Validate()
	assert.NoError(t, err)
}

func TestChromiumPolicyOverrides_Validate_BlockedPolicies(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		value  string
	}{
		{"ExtensionSettings", "ExtensionSettings", `{}`},
		{"ExtensionInstallForcelist", "ExtensionInstallForcelist", `[]`},
		{"RemoteDebuggingAllowed", "RemoteDebuggingAllowed", `false`},
		{"DeveloperToolsAvailability", "DeveloperToolsAvailability", `2`},
		{"ProxySettings", "ProxySettings", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overrides := ChromiumPolicyOverrides{
				tt.policy: json.RawMessage(tt.value),
			}
			err := overrides.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot be overridden")
		})
	}
}

func TestChromiumPolicyOverrides_Validate_WrongTypes(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		value    string
		wantErr  string
	}{
		{
			name:    "bool expected, got string",
			policy:  "PasswordManagerEnabled",
			value:   `"true"`,
			wantErr: "expected boolean",
		},
		{
			name:    "int expected, got string",
			policy:  "DefaultCookiesSetting",
			value:   `"1"`,
			wantErr: "expected integer",
		},
		{
			name:    "int expected, got float",
			policy:  "DefaultCookiesSetting",
			value:   `1.5`,
			wantErr: "expected integer",
		},
		{
			name:    "string expected, got int",
			policy:  "NewTabPageLocation",
			value:   `123`,
			wantErr: "expected string",
		},
		{
			name:    "list expected, got string",
			policy:  "URLAllowlist",
			value:   `"https://example.com"`,
			wantErr: "expected list of strings",
		},
		{
			name:    "list of strings, got list of ints",
			policy:  "URLAllowlist",
			value:   `[1, 2, 3]`,
			wantErr: "expected string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overrides := ChromiumPolicyOverrides{
				tt.policy: json.RawMessage(tt.value),
			}
			err := overrides.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestChromiumPolicyOverrides_Validate_UnknownPoliciesAllowed(t *testing.T) {
	overrides := ChromiumPolicyOverrides{
		"SomeFutureChromePolicy": json.RawMessage(`true`),
	}

	err := overrides.Validate()
	assert.NoError(t, err)
}

func TestChromiumPolicyOverrides_Validate_UnknownPolicyInvalidJSON(t *testing.T) {
	overrides := ChromiumPolicyOverrides{
		"SomeFutureChromePolicy": json.RawMessage(`not-valid-json`),
	}

	err := overrides.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid JSON")
}

func TestChromiumPolicyOverrides_MergeIntoPolicy(t *testing.T) {
	input := `{
		"PasswordManagerEnabled": false,
		"DefaultGeolocationSetting": 2,
		"ExtensionSettings": {
			"*": {
				"allowed_types": ["extension"],
				"install_sources": ["*"]
			}
		}
	}`

	var p Policy
	err := json.Unmarshal([]byte(input), &p)
	require.NoError(t, err)

	overrides := ChromiumPolicyOverrides{
		"DefaultCookiesSetting":    json.RawMessage(`1`),
		"BasicAuthOverHttpEnabled": json.RawMessage(`true`),
		"HttpsUpgradesEnabled":     json.RawMessage(`false`),
		"PasswordManagerEnabled":   json.RawMessage(`true`), // override existing
	}

	overrides.MergeIntoPolicy(&p)

	data, err := json.Marshal(&p)
	require.NoError(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	// New overrides are present
	assert.Equal(t, float64(1), result["DefaultCookiesSetting"])
	assert.Equal(t, true, result["BasicAuthOverHttpEnabled"])
	assert.Equal(t, false, result["HttpsUpgradesEnabled"])

	// Overridden existing value
	assert.Equal(t, true, result["PasswordManagerEnabled"])

	// Untouched existing values preserved
	assert.Equal(t, float64(2), result["DefaultGeolocationSetting"])

	// Kernel-managed fields still present
	assert.NotNil(t, result["ExtensionSettings"])
}

func TestChromiumPolicyOverrides_Validate_DictPolicy(t *testing.T) {
	overrides := ChromiumPolicyOverrides{
		"ManagedBookmarks": json.RawMessage(`[{"name": "Google", "url": "https://google.com"}]`),
	}

	err := overrides.Validate()
	assert.NoError(t, err)
}

func TestChromiumPolicyOverrides_Validate_MultipleErrors(t *testing.T) {
	overrides := ChromiumPolicyOverrides{
		"ExtensionSettings":    json.RawMessage(`{}`),
		"DefaultCookiesSetting": json.RawMessage(`"wrong"`),
		"PasswordManagerEnabled": json.RawMessage(`1`),
	}

	err := overrides.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be overridden")
	assert.Contains(t, err.Error(), "expected integer")
	assert.Contains(t, err.Error(), "expected boolean")
}

func TestChromiumPolicyOverrides_EmptyIsValid(t *testing.T) {
	overrides := ChromiumPolicyOverrides{}
	err := overrides.Validate()
	assert.NoError(t, err)
}

func TestChromiumPolicyOverrides_NilIsValid(t *testing.T) {
	var overrides ChromiumPolicyOverrides
	err := overrides.Validate()
	assert.NoError(t, err)
}
