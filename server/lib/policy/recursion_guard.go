package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	// ChromiumRecursionGuardURLBlocklistEnv configures URLBlocklist entries
	// that are always merged into Chromium policy before the browser starts.
	// Values may be comma- or newline-separated. Unset or blank uses the default.
	// Set to "0", "false", "off", or "none" to disable the guard explicitly.
	ChromiumRecursionGuardURLBlocklistEnv = "CHROMIUM_RECURSION_GUARD_URL_BLOCKLIST"

	// URLBlocklist treats a bare host as matching that host and its subdomains.
	// The query is intentionally omitted so livestream JWT variations are
	// covered without per-request proxy work.
	DefaultChromiumRecursionGuardURLBlocklist = "https://onkernel.com:8443/browser/live"
)

// RecursionGuardURLBlocklistFromEnv returns the configured recursion guard
// URLBlocklist entries. The default blocks deployed live-browser capture URLs.
func RecursionGuardURLBlocklistFromEnv() []string {
	return recursionGuardURLBlocklistFromLookup(os.LookupEnv)
}

func recursionGuardURLBlocklistFromLookup(lookup func(string) (string, bool)) []string {
	value, ok := lookup(ChromiumRecursionGuardURLBlocklistEnv)
	if !ok || strings.TrimSpace(value) == "" {
		return []string{DefaultChromiumRecursionGuardURLBlocklist}
	}

	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "0", "false", "off", "none":
		return nil
	}

	return splitURLBlocklist(value)
}

func splitURLBlocklist(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})

	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if v := strings.TrimSpace(field); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ApplyURLBlocklistGuard merges guard entries into Chrome's URLBlocklist policy
// without removing customer-provided blocklist entries.
func (p *Policy) ApplyURLBlocklistGuard(entries []string) error {
	entries = uniqueStrings(entries)
	if len(entries) == 0 {
		return nil
	}

	return p.Modify(func(current *Policy) error {
		return mergeURLBlocklistGuard(current, entries)
	})
}

func mergeURLBlocklistGuard(current *Policy, entries []string) error {
	entries = uniqueStrings(entries)
	if len(entries) == 0 {
		return nil
	}

	if current.unknownFields == nil {
		current.unknownFields = make(map[string]json.RawMessage)
	}

	var blocklist []string
	if raw, ok := current.unknownFields["URLBlocklist"]; ok && len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &blocklist); err != nil {
			return fmt.Errorf("failed to parse existing URLBlocklist policy: %w", err)
		}
	}

	blocklist = append(blocklist, entries...)
	blocklist = uniqueStrings(blocklist)

	raw, err := json.Marshal(blocklist)
	if err != nil {
		return fmt.Errorf("failed to marshal URLBlocklist policy: %w", err)
	}
	current.unknownFields["URLBlocklist"] = raw
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
