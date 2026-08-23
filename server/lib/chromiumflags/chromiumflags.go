package chromiumflags

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// FlagsFile is the structured JSON representation stored at /chromium/flags.
//
// Example on disk:
// { "flags": ["--foo", "--bar=1"] }
type FlagsFile struct {
	Flags []string `json:"flags"`
}

// parseFlags splits a space-delimited string of Chromium flags into tokens.
// Tokens are expected in the form --flag or --flag=value. Quotes are not supported,
// matching the previous bash implementation which used simple word-splitting.
func parseFlags(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return []string{}
	}
	return strings.Fields(input)
}

// appendCSVInto appends comma-separated values into dst, skipping empty items.
func appendCSVInto(dst *[]string, csv string) {
	for _, part := range strings.Split(csv, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*dst = append(*dst, p)
		}
	}
}

// parseTokenStream extracts extension-related and feature-list flags and collects the rest.
// It returns the leftover tokens and, via references, fills the buckets for
// --load-extension, --disable-extensions-except, --enable-features, --disable-features
// and a possible --disable-extensions token for that stream.
func parseTokenStream(tokens []string, load, except, enableFeatures, disableFeatures *[]string, disableAll *string) (nonExt []string) {
	for _, tok := range tokens {
		switch {
		case strings.HasPrefix(tok, "--load-extension="):
			val := strings.TrimPrefix(tok, "--load-extension=")
			appendCSVInto(load, val)
		case strings.HasPrefix(tok, "--disable-extensions-except="):
			val := strings.TrimPrefix(tok, "--disable-extensions-except=")
			appendCSVInto(except, val)
		case strings.HasPrefix(tok, "--enable-features="):
			val := strings.TrimPrefix(tok, "--enable-features=")
			appendCSVInto(enableFeatures, val)
		case strings.HasPrefix(tok, "--disable-features="):
			val := strings.TrimPrefix(tok, "--disable-features=")
			appendCSVInto(disableFeatures, val)
		case tok == "--disable-extensions":
			*disableAll = tok
		default:
			nonExt = append(nonExt, tok)
		}
	}
	return nonExt
}

// union merges two lists of strings, returning a new list with duplicates removed.
func union(base, rt []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, v := range append(append([]string{}, base...), rt...) {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// ReadOptionalFlagFile returns the flags array from the JSON file at path.
// If the file does not exist, it returns nil and a nil error.
func ReadOptionalFlagFile(path string) ([]string, error) {
	// If the file doesn't exist, treat as empty overlay
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Read entire content to allow JSON detection
	content, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	b := strings.TrimSpace(string(content))
	if b == "" {
		return nil, nil
	}

	// format: JSON with { "flags": ["--flag", "--flag=val"] }
	var jf FlagsFile
	if err := json.Unmarshal([]byte(b), &jf); err != nil {
		return nil, err
	}
	if jf.Flags == nil {
		return nil, errors.New("flags file missing 'flags' array")
	}
	// Normalize tokens and return slice
	normalized := []string{}
	for _, tok := range jf.Flags {
		if t := strings.TrimSpace(tok); t != "" {
			normalized = append(normalized, t)
		}
	}
	return normalized, nil
}

// MergeFlags merges base flags with runtime flags, returning the final merged flags as a string.
// The merging logic respects extension-related flag semantics:
// 1) If runtime specifies --disable-extensions, it overrides everything extension related
// 2) Else if base specifies --disable-extensions and runtime does NOT specify any --load-extension, keep base disable
// 3) Else, build from merged load-extension paths
//
// NOTE: --disable-extensions-except is intentionally parsed but NOT re-emitted because it causes
// Chrome to disable external providers (including the policy loader), which prevents enterprise
// policy extensions (ExtensionInstallForcelist) from being fetched and installed.
// See Chromium source: extension_service.cc - external providers are only created when
// extensions_enabled() returns true, which is false when --disable-extensions-except is used.
// Any paths from --disable-extensions-except are merged into --load-extension instead.
//
// Feature-list flags (--enable-features / --disable-features) from both sources are also
// merged into single comma-separated tokens. Chromium keeps only one value per switch, so
// emitting duplicates would silently drop all but the last list. When a feature appears in
// both lists Chromium disables it — disable wins over enable.
//
// Non-feature, non-extension flags from both base and runtime are combined with deduplication
// (first occurrence preserved).
func MergeFlags(baseTokens, runtimeTokens []string) []string {
	// Buckets
	var (
		baseNonExt      []string // Non-extension related flags contained in base
		runtimeNonExt   []string // Non-extension related flags contained in runtime
		baseLoad        []string // --load-extension flags contained in base
		baseExcept      []string // --disable-extensions-except flags for base (parsed but not re-emitted)
		baseEnableFeat  []string // --enable-features values contained in base
		baseDisableFeat []string // --disable-features values contained in base
		rtLoad          []string // --load-extension flags contained in runtime
		rtExcept        []string // --disable-extensions-except flags contained in runtime (parsed but not re-emitted)
		rtEnableFeat    []string // --enable-features values contained in runtime
		rtDisableFeat   []string // --disable-features values contained in runtime
		baseDisableAll  string   // --disable-extensions flag contained in base
		rtDisableAll    string   // --disable-extensions flag contained in runtime
	)

	baseNonExt = parseTokenStream(baseTokens, &baseLoad, &baseExcept, &baseEnableFeat, &baseDisableFeat, &baseDisableAll)
	runtimeNonExt = parseTokenStream(runtimeTokens, &rtLoad, &rtExcept, &rtEnableFeat, &rtDisableFeat, &rtDisableAll)

	// Merge extension lists - include paths from --disable-extensions-except in load paths
	// since we no longer emit --disable-extensions-except
	mergedLoad := union(baseLoad, rtLoad)
	mergedLoad = union(mergedLoad, baseExcept)
	mergedLoad = union(mergedLoad, rtExcept)

	// Merge feature lists - a feature listed in both enable and disable stays in both;
	// Chromium resolves that conflict as disabled.
	mergedEnableFeat := union(baseEnableFeat, rtEnableFeat)
	mergedDisableFeat := union(baseDisableFeat, rtDisableFeat)

	var featureFlags []string
	if len(mergedEnableFeat) > 0 {
		featureFlags = append(featureFlags, "--enable-features="+strings.Join(mergedEnableFeat, ","))
	}
	if len(mergedDisableFeat) > 0 {
		featureFlags = append(featureFlags, "--disable-features="+strings.Join(mergedDisableFeat, ","))
	}

	// Construct final extension-related flags respecting override semantics:
	// 1) If runtime specifies --disable-extensions, it overrides everything extension related
	// 2) Else if base specifies --disable-extensions and runtime does NOT specify any --load-extension, keep base disable
	// 3) Else, build from merged load-extension paths
	var extFlags []string
	if rtDisableAll != "" {
		extFlags = append(extFlags, rtDisableAll)
	} else {
		if baseDisableAll != "" && len(rtLoad) == 0 && len(rtExcept) == 0 {
			extFlags = append(extFlags, baseDisableAll)
		} else if len(mergedLoad) > 0 {
			extFlags = append(extFlags, "--load-extension="+strings.Join(mergedLoad, ","))
		}
		// NOTE: --disable-extensions-except is intentionally NOT emitted here
	}

	// Combine and dedupe (preserving first occurrence)
	combined := append(append([]string{}, baseNonExt...), runtimeNonExt...)
	combined = append(combined, featureFlags...)
	combined = append(combined, extFlags...)
	seen := make(map[string]struct{}, len(combined))
	final := make([]string, 0, len(combined))
	for _, tok := range combined {
		if tok == "" {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		final = append(final, tok)
	}
	return final
}

// MergeFlagsWithRuntimeTokens merges base flags (string, e.g. from env CHROMIUM_FLAGS)
// with runtime token slice and returns final tokens.
func MergeFlagsWithRuntimeTokens(baseFlags string, runtimeTokens []string) []string {
	base := parseFlags(baseFlags)
	return MergeFlags(base, runtimeTokens)
}

// MergeExtensionPath appends an extension path to existing --load-extension flags
// within an args slice. If the flag exists, the path is appended to its comma-separated
// list. If it doesn't exist, a new flag is added. This preserves other extensions that
// may already be configured.
//
// NOTE: We intentionally do NOT use --disable-extensions-except here because it causes
// Chrome to disable external providers (including the policy loader), which prevents
// enterprise policy extensions (ExtensionInstallForcelist) from being fetched and installed.
// See Chromium source: extension_service.cc - external providers are only created when
// extensions_enabled() returns true, which is false when --disable-extensions-except is used.
func MergeExtensionPath(args []string, extPath string) []string {
	foundLoad := false
	result := make([]string, 0, len(args)+1)

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--load-extension="):
			existing := strings.TrimPrefix(arg, "--load-extension=")
			result = append(result, "--load-extension="+existing+","+extPath)
			foundLoad = true
		default:
			result = append(result, arg)
		}
	}

	if !foundLoad {
		result = append(result, "--load-extension="+extPath)
	}

	return result
}

// WriteFlagFile writes the provided tokens to the given path as JSON in the
// form: { "flags": ["--foo", "--bar=1"] } with file mode 0644.
// The function creates or truncates the file.
func WriteFlagFile(path string, tokens []string) error {
	// Normalize tokens: trim and drop empties
	normalized := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if s := strings.TrimSpace(t); s != "" {
			normalized = append(normalized, s)
		}
	}
	data, err := json.Marshal(FlagsFile{Flags: normalized})
	if err != nil {
		return err
	}
	// Ensure trailing newline for readability
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// KernelDisableFeaturesPrefix is a kernel-namespaced switch for per-session feature
// disabling. Chromium ignores unknown switches, so on images that predate translation
// the token is inert instead of last-win clobbering boot-time --disable-features lists.
const KernelDisableFeaturesPrefix = "--kernel-disable-features="

// TranslateKernelDisableFeatures folds any --kernel-disable-features tokens into the
// single --disable-features token and returns the result. Values are unioned with any
// existing disable list; ordering of unrelated tokens is preserved. Idempotent.
func TranslateKernelDisableFeatures(tokens []string) []string {
	var kernelVals, disableVals []string
	rest := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		switch {
		case strings.HasPrefix(tok, KernelDisableFeaturesPrefix):
			appendCSVInto(&kernelVals, strings.TrimPrefix(tok, KernelDisableFeaturesPrefix))
		case strings.HasPrefix(tok, "--disable-features="):
			appendCSVInto(&disableVals, strings.TrimPrefix(tok, "--disable-features="))
		default:
			rest = append(rest, tok)
		}
	}
	if len(kernelVals) == 0 {
		return tokens
	}
	merged := union(disableVals, kernelVals)
	rest = append(rest, "--disable-features="+strings.Join(merged, ","))
	return rest
}
