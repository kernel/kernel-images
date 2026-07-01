package policy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRecursionGuardURLBlocklist_DefaultWhenUnsetOrBlank(t *testing.T) {
	unsetLookup := func(string) (string, bool) { return "", false }
	blankLookup := func(string) (string, bool) { return "   ", true }

	assert.Equal(t, []string{DefaultChromiumRecursionGuardURLBlocklist}, recursionGuardURLBlocklistFromLookup(unsetLookup))
	assert.Equal(t, []string{DefaultChromiumRecursionGuardURLBlocklist}, recursionGuardURLBlocklistFromLookup(blankLookup))
}

func TestParseRecursionGuardURLBlocklist_CustomList(t *testing.T) {
	setLookup := func(string) (string, bool) {
		return "https://example.com:8443/browser/live,\nhttps://capture.example.net/live", true
	}

	got := recursionGuardURLBlocklistFromLookup(setLookup)

	assert.Equal(t, []string{
		"https://example.com:8443/browser/live",
		"https://capture.example.net/live",
	}, got)
}

func TestParseRecursionGuardURLBlocklist_DisableValues(t *testing.T) {
	for _, value := range []string{"0", "false", "off", "none"} {
		t.Run(value, func(t *testing.T) {
			setLookup := func(string) (string, bool) { return value, true }
			assert.Nil(t, recursionGuardURLBlocklistFromLookup(setLookup))
		})
	}
}

func TestMergeURLBlocklistGuard_AppendsWithoutDroppingExistingEntries(t *testing.T) {
	current := &Policy{
		ExtensionSettings: map[string]ExtensionSetting{},
		unknownFields: map[string]json.RawMessage{
			"URLBlocklist": json.RawMessage(`["https://blocked.example.com/*"]`),
		},
	}

	err := mergeURLBlocklistGuard(current, []string{
		DefaultChromiumRecursionGuardURLBlocklist,
		DefaultChromiumRecursionGuardURLBlocklist,
	})
	require.NoError(t, err)

	var blocklist []string
	require.NoError(t, json.Unmarshal(current.unknownFields["URLBlocklist"], &blocklist))
	assert.Equal(t, []string{
		"https://blocked.example.com/*",
		DefaultChromiumRecursionGuardURLBlocklist,
	}, blocklist)
}

func TestMergeURLBlocklistGuard_PreservesOtherUnknownPolicyFields(t *testing.T) {
	current := &Policy{
		ExtensionSettings: map[string]ExtensionSetting{},
		unknownFields: map[string]json.RawMessage{
			"PasswordManagerEnabled": json.RawMessage(`false`),
		},
	}

	err := mergeURLBlocklistGuard(current, []string{DefaultChromiumRecursionGuardURLBlocklist})
	require.NoError(t, err)

	assert.JSONEq(t, `false`, string(current.unknownFields["PasswordManagerEnabled"]))

	var blocklist []string
	require.NoError(t, json.Unmarshal(current.unknownFields["URLBlocklist"], &blocklist))
	assert.Equal(t, []string{DefaultChromiumRecursionGuardURLBlocklist}, blocklist)
}
