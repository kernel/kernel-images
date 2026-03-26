package policy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PolicyValueType describes the expected JSON type for a Chromium enterprise policy.
type PolicyValueType int

const (
	PolicyTypeBool       PolicyValueType = iota // "main" in Chromium YAML
	PolicyTypeInt                               // "int" and "int-enum"
	PolicyTypeString                            // "string", "string-enum"
	PolicyTypeListString                        // "list", "string-enum-list"
	PolicyTypeDict                              // "dict" — arbitrary JSON object/array
)

func (t PolicyValueType) String() string {
	switch t {
	case PolicyTypeBool:
		return "boolean"
	case PolicyTypeInt:
		return "integer"
	case PolicyTypeString:
		return "string"
	case PolicyTypeListString:
		return "list of strings"
	case PolicyTypeDict:
		return "object"
	default:
		return "unknown"
	}
}

// ChromiumPolicyOverrides represents user-provided Chromium enterprise policy
// overrides. These are merged on top of the base policy.json during browser
// creation. The map keys are Chromium policy names (e.g. "DefaultCookiesSetting")
// and values are the policy values in their native JSON types.
type ChromiumPolicyOverrides map[string]json.RawMessage

// blockedPolicies are policies that users must not override because they are
// managed by kernel infrastructure or are security-critical for the platform.
var blockedPolicies = map[string]string{
	// Managed by kernel's extension upload system
	"ExtensionSettings":         "managed by kernel extension system",
	"ExtensionInstallForcelist": "managed by kernel extension system",

	// Could disable kernel's own extensions
	"ExtensionInstallBlocklist":  "could interfere with kernel extensions",
	"ExtensionInstallBlacklist":  "could interfere with kernel extensions",
	"ExtensionInstallAllowlist":  "could interfere with kernel extensions",
	"ExtensionInstallWhitelist":  "could interfere with kernel extensions",
	"BlockExternalExtensions":    "could interfere with kernel extensions",
	"ExtensionAllowedTypes":      "could interfere with kernel extensions",
	"ExtensionInstallSources":    "could interfere with kernel extensions",
	"ExtensionManifestV2Availability": "could interfere with kernel extensions",

	// Required for CDP / automation
	"RemoteDebuggingAllowed":                "required for CDP connectivity",
	"DeveloperToolsAvailability":            "required for CDP connectivity",
	"DeveloperToolsDisabled":                "required for CDP connectivity",
	"DeveloperToolsAvailabilityAllowlist":   "required for CDP connectivity",
	"DeveloperToolsAvailabilityBlocklist":   "required for CDP connectivity",
	"ChromeForTestingAllowed":               "required for automation",
	"WebDriverOverridesIncompatiblePolicies": "required for automation",

	// Proxy is managed via kernel's proxy feature
	"ProxySettings":  "use kernel's proxy API instead",
	"ProxyMode":      "use kernel's proxy API instead",
	"ProxyServer":    "use kernel's proxy API instead",
	"ProxyBypassList": "use kernel's proxy API instead",
	"ProxyPacUrl":    "use kernel's proxy API instead",
}

// policyRegistry maps all Chromium enterprise policies supported on Linux
// (chrome.* or chrome.linux) to their expected value types. Generated from
// chromium/src/components/policy/resources/templates/policy_definitions/
// at Chromium version 133.x (655 policies). Policies not in this registry
// are still accepted with basic JSON validation (see Validate).
//
//nolint:dupword
var policyRegistry map[string]PolicyValueType

// Validate checks that the overrides contain valid Chromium policy names with
// correct value types, and that no blocked policies are being overridden.
func (o ChromiumPolicyOverrides) Validate() error {
	var errs []string
	for name, raw := range o {
		if reason, blocked := blockedPolicies[name]; blocked {
			errs = append(errs, fmt.Sprintf("policy %q cannot be overridden: %s", name, reason))
			continue
		}

		expectedType, known := policyRegistry[name]
		if !known {
			// Allow unknown policies so new Chrome versions don't require
			// immediate code updates, but still validate JSON is well-formed.
			var v interface{}
			if err := json.Unmarshal(raw, &v); err != nil {
				errs = append(errs, fmt.Sprintf("policy %q: invalid JSON value", name))
			}
			continue
		}

		if err := validatePolicyValue(name, raw, expectedType); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid chromium policy overrides:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// validatePolicyValue checks that a raw JSON value matches the expected type.
func validatePolicyValue(name string, raw json.RawMessage, expected PolicyValueType) error {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("policy %q: invalid JSON value", name)
	}

	switch expected {
	case PolicyTypeBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("policy %q: expected %s, got %T", name, expected, v)
		}
	case PolicyTypeInt:
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("policy %q: expected %s, got %T", name, expected, v)
		}
		if f != float64(int(f)) {
			return fmt.Errorf("policy %q: expected integer, got float", name)
		}
	case PolicyTypeString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("policy %q: expected %s, got %T", name, expected, v)
		}
	case PolicyTypeListString:
		arr, ok := v.([]interface{})
		if !ok {
			return fmt.Errorf("policy %q: expected %s, got %T", name, expected, v)
		}
		for i, item := range arr {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("policy %q[%d]: expected string, got %T", name, i, item)
			}
		}
	case PolicyTypeDict:
		switch v.(type) {
		case map[string]interface{}, []interface{}:
			// dict policies can be objects or arrays of objects
		default:
			return fmt.Errorf("policy %q: expected %s, got %T", name, expected, v)
		}
	}

	return nil
}

// NewChromiumPolicyOverrides converts a map[string]interface{} (as produced by
// JSON decoding with oapi-codegen) into ChromiumPolicyOverrides by re-marshaling
// each value to json.RawMessage.
func NewChromiumPolicyOverrides(m map[string]interface{}) (ChromiumPolicyOverrides, error) {
	if m == nil {
		return nil, nil
	}
	o := make(ChromiumPolicyOverrides, len(m))
	for k, v := range m {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("policy %q: failed to marshal value: %w", k, err)
		}
		o[k] = json.RawMessage(raw)
	}
	return o, nil
}

// MergeIntoPolicy applies user overrides on top of an existing Policy.
// Overrides are stored into the Policy's unknownFields so they are preserved
// during the normal read-modify-write cycle. This must be called AFTER
// Validate().
func (o ChromiumPolicyOverrides) MergeIntoPolicy(p *Policy) {
	if p.unknownFields == nil {
		p.unknownFields = make(map[string]json.RawMessage)
	}
	for name, raw := range o {
		p.unknownFields[name] = raw
	}
}

// ApplyOverrides validates user-provided overrides, reads the current policy
// from disk, merges the overrides in, and writes the result back.
// This is the main entry point for the PATCH /chromium/policies endpoint.
func (p *Policy) ApplyOverrides(overrides ChromiumPolicyOverrides) error {
	if err := overrides.Validate(); err != nil {
		return err
	}

	return p.Modify(func(current *Policy) error {
		overrides.MergeIntoPolicy(current)
		return nil
	})
}
