package policy

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

const PolicyPath = "/etc/chromium/policies/managed/policy.json"

// Chrome extension IDs are 32 lowercase a-p characters
var extensionIDRegex = regexp.MustCompile(`^[a-p]{32}$`)
var extensionPathRegex = regexp.MustCompile(`/extensions/[^/]+/`)

// Policy represents the Chrome enterprise policy structure.
// Only fields that are programmatically modified are defined here.
// All other fields (like DefaultGeolocationSetting, PasswordManagerEnabled, etc.)
// are preserved through the unknownFields mechanism during read-modify-write cycles.
type Policy struct {
	mu sync.Mutex

	// ExtensionInstallForcelist is modified when adding force-installed extensions
	ExtensionInstallForcelist []string `json:"ExtensionInstallForcelist,omitempty"`
	// ExtensionSettings is modified when adding/configuring extensions
	ExtensionSettings map[string]ExtensionSetting `json:"ExtensionSettings"`

	// unknownFields preserves all JSON fields not explicitly defined in this struct.
	// This allows policy.json to contain any Chrome policy settings without
	// requiring updates to this Go struct.
	unknownFields map[string]json.RawMessage
}

// policyJSON is used for JSON marshaling/unmarshaling without the mutex.
// This avoids go vet warnings about copying mutex values.
type policyJSON struct {
	ExtensionInstallForcelist []string                    `json:"ExtensionInstallForcelist,omitempty"`
	ExtensionSettings         map[string]ExtensionSetting `json:"ExtensionSettings"`
}

// knownPolicyFields lists all JSON field names that have corresponding struct fields.
// All other fields are automatically preserved in unknownFields.
var knownPolicyFields = map[string]bool{
	"ExtensionInstallForcelist": true,
	"ExtensionSettings":         true,
}

// UnmarshalJSON implements custom JSON unmarshaling that preserves unknown fields.
func (p *Policy) UnmarshalJSON(data []byte) error {
	// First, unmarshal into a map to capture all fields
	var allFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &allFields); err != nil {
		return err
	}

	// Unmarshal known fields into the helper struct (no mutex)
	var pj policyJSON
	if err := json.Unmarshal(data, &pj); err != nil {
		return err
	}

	// Copy the known fields to p
	p.ExtensionInstallForcelist = pj.ExtensionInstallForcelist
	p.ExtensionSettings = pj.ExtensionSettings

	// Extract unknown fields
	p.unknownFields = make(map[string]json.RawMessage)
	for key, value := range allFields {
		if !knownPolicyFields[key] {
			p.unknownFields[key] = value
		}
	}

	return nil
}

// MarshalJSON implements custom JSON marshaling that includes unknown fields.
func (p *Policy) MarshalJSON() ([]byte, error) {
	// Create helper struct with known fields (no mutex)
	pj := policyJSON{
		ExtensionInstallForcelist: p.ExtensionInstallForcelist,
		ExtensionSettings:         p.ExtensionSettings,
	}

	// Marshal the known fields first
	knownData, err := json.Marshal(pj)
	if err != nil {
		return nil, err
	}

	// If no unknown fields, return as-is
	if len(p.unknownFields) == 0 {
		return knownData, nil
	}

	// Unmarshal known fields into a map so we can add unknown fields
	var result map[string]json.RawMessage
	if err := json.Unmarshal(knownData, &result); err != nil {
		return nil, err
	}

	// Add unknown fields
	for key, value := range p.unknownFields {
		result[key] = value
	}

	return json.Marshal(result)
}

// ExtensionSetting represents settings for a specific extension
type ExtensionSetting struct {
	InstallationMode    string   `json:"installation_mode,omitempty"`
	UpdateUrl           string   `json:"update_url,omitempty"`
	AllowedTypes        []string `json:"allowed_types,omitempty"`
	InstallSources      []string `json:"install_sources,omitempty"`
	RuntimeBlockedHosts []string `json:"runtime_blocked_hosts,omitempty"`
	RuntimeAllowedHosts []string `json:"runtime_allowed_hosts,omitempty"`
}

// readPolicyUnlocked reads the current enterprise policy from disk without locking
// This is an internal helper for use within already-locked operations
func (p *Policy) readPolicyUnlocked() (*Policy, error) {
	data, err := os.ReadFile(PolicyPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Return minimal policy if file doesn't exist.
			// In practice, policy.json ships with the container image.
			return &Policy{
				ExtensionInstallForcelist: []string{},
				ExtensionSettings:         make(map[string]ExtensionSetting),
			}, nil
		}
		return nil, fmt.Errorf("failed to read policy file: %w", err)
	}

	var policy Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("failed to parse policy file: %w", err)
	}

	// Initialize ExtensionSettings map if it's nil to prevent panic on write
	if policy.ExtensionSettings == nil {
		policy.ExtensionSettings = make(map[string]ExtensionSetting)
	}

	// Initialize ExtensionInstallForcelist if it's nil
	if policy.ExtensionInstallForcelist == nil {
		policy.ExtensionInstallForcelist = []string{}
	}

	return &policy, nil
}

// ReadPolicy reads the current enterprise policy from disk
func (p *Policy) ReadPolicy() (*Policy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.readPolicyUnlocked()
}

// writePolicyUnlocked writes the policy to disk without locking
// This is an internal helper for use within already-locked operations
func (p *Policy) writePolicyUnlocked(policy *Policy) error {
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal policy: %w", err)
	}

	if err := os.WriteFile(PolicyPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write policy file: %w", err)
	}

	return nil
}

// WritePolicy writes the policy to disk
func (p *Policy) WritePolicy(policy *Policy) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.writePolicyUnlocked(policy)
}

// AddExtension adds or updates an extension in the policy
// extensionName is the user-provided name used for the directory and URL paths
// chromeExtensionID is the actual Chrome extension ID (from update.xml appid) used in policy entries
// extensionPath is the full path to the unpacked extension directory
func (p *Policy) AddExtension(extensionName, chromeExtensionID, extensionPath string, requiresEnterprisePolicy bool) error {
	// Lock for the entire read-modify-write cycle to prevent race conditions
	p.mu.Lock()
	defer p.mu.Unlock()

	policy, err := p.readPolicyUnlocked()
	if err != nil {
		return err
	}

	// Ensure the wildcard policy exists
	if _, exists := policy.ExtensionSettings["*"]; !exists {
		policy.ExtensionSettings["*"] = ExtensionSetting{
			AllowedTypes:   []string{"extension"},
			InstallSources: []string{"*"},
		}
	}

	// Add the specific extension
	// Use extension name for the URL path (where files are served)
	// Use Chrome extension ID for the policy key (what Chrome expects)
	setting := ExtensionSetting{
		UpdateUrl: fmt.Sprintf("http://127.0.0.1:10001/extensions/%s/update.xml", extensionName),
	}

	// If the extension requires enterprise policy (like webRequestBlocking),
	// we need special handling for unpacked extensions loaded via --load-extension
	// https://github.com/cloudflare/web-bot-auth/blob/main/examples/browser-extension/policy/policy.json.templ
	if requiresEnterprisePolicy {
		// For unpacked extensions with webRequestBlocking:
		// Chrome requires the extension to be in ExtensionInstallForcelist
		// Format: "extension_id;update_url" per https://chromeenterprise.google/intl/en_ca/policies/#ExtensionInstallForcelist
		setting.InstallationMode = "force_installed"

		// Add to ExtensionInstallForcelist using the Chrome extension ID and update URL
		forcelistEntry := fmt.Sprintf("%s;%s", chromeExtensionID, setting.UpdateUrl)

		// Remove any existing entries with the same extension ID (different URLs)
		if policy.ExtensionInstallForcelist == nil {
			policy.ExtensionInstallForcelist = []string{}
		}

		// Filter out entries that start with the same extension ID
		extensionIDPrefix := chromeExtensionID + ";"
		policy.ExtensionInstallForcelist = slices.DeleteFunc(policy.ExtensionInstallForcelist, func(entry string) bool {
			return strings.HasPrefix(entry, extensionIDPrefix)
		})

		// Add the new entry
		policy.ExtensionInstallForcelist = append(policy.ExtensionInstallForcelist, forcelistEntry)

		// Use Chrome extension ID as the key in ExtensionSettings
		policy.ExtensionSettings[chromeExtensionID] = setting
	} else {
		// For normal extensions, use the extension name as the key
		policy.ExtensionSettings[extensionName] = setting
	}

	return p.writePolicyUnlocked(policy)
}

// GenerateExtensionID returns a stable identifier for the extension policy.
// For ExtensionSettings with local paths, Chrome allows custom identifiers.
// We use the extension name because it's stable, readable, and matches the directory.
func (p *Policy) GenerateExtensionID(extensionName string) string {
	return extensionName
}

// RequiresEnterprisePolicy checks if an extension requires enterprise policy
// by examining its manifest.json for webRequestBlocking or webRequest permissions
func (p *Policy) RequiresEnterprisePolicy(manifestPath string) (bool, error) {
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, err
	}

	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return false, err
	}

	// Check if permissions include webRequestBlocking or webRequest
	perms, ok := manifest["permissions"].([]interface{})
	if !ok {
		return false, nil
	}

	for _, perm := range perms {
		if permStr, ok := perm.(string); ok {
			if permStr == "webRequestBlocking" || permStr == "webRequest" {
				return true, nil
			}
		}
	}

	return false, nil
}

// updateManifest represents the Chrome extension update manifest XML structure
type updateManifest struct {
	XMLName xml.Name  `xml:"gupdate"`
	Apps    []appNode `xml:"app"`
}

type appNode struct {
	AppID string `xml:"appid,attr"`
}

// ExtractExtensionIDFromUpdateXML reads update.xml and extracts the appid attribute
// from the <app> element. Returns the appid or an error if the file doesn't exist
// or the appid cannot be found.
func ExtractExtensionIDFromUpdateXML(updateXMLPath string) (string, error) {
	data, err := os.ReadFile(updateXMLPath)
	if err != nil {
		return "", fmt.Errorf("failed to read update.xml: %w", err)
	}

	var manifest updateManifest
	if err := xml.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("failed to parse update.xml: %w", err)
	}

	if len(manifest.Apps) == 0 {
		return "", fmt.Errorf("no <app> element found in update.xml")
	}

	appID := manifest.Apps[0].AppID
	if appID == "" {
		return "", fmt.Errorf("appid attribute is empty in update.xml")
	}

	// Validate extension ID format: Chrome extension IDs are 32 lowercase a-p characters
	// This prevents injection attacks via semicolons or other special characters
	if !extensionIDRegex.MatchString(appID) {
		return "", fmt.Errorf("invalid Chrome extension ID format in update.xml: %s", appID)
	}

	return appID, nil
}

// RewriteUpdateXMLUrls rewrites the codebase URLs in update.xml to use the specified extension name.
// This ensures that regardless of what name was originally in the update.xml, the URLs will match
// the actual directory name where the extension is installed.
func RewriteUpdateXMLUrls(updateXMLPath, extensionName string) error {
	data, err := os.ReadFile(updateXMLPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read update.xml: %w", err)
	}

	content := string(data)
	newPath := fmt.Sprintf("/extensions/%s/", extensionName)

	newContent := extensionPathRegex.ReplaceAllString(content, newPath)

	if newContent != content {
		if err := os.WriteFile(updateXMLPath, []byte(newContent), 0644); err != nil {
			return fmt.Errorf("failed to write update.xml: %w", err)
		}
	}

	return nil
}
