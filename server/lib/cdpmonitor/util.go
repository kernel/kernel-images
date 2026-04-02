package cdpmonitor

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// isTextualResource reports whether the resource warrants body capture.
// resourceType is checked first; mimeType is a fallback for resources with no type (e.g. in-flight at attach time).
func isTextualResource(resourceType, mimeType string) bool {
	switch resourceType {
	case "Font", "Image", "Media":
		return false
	}
	return isCapturedMIME(mimeType)
}

// isCapturedMIME returns true for MIME types whose bodies are worth capturing.
// Binary formats (vendor types, binary encodings, raw streams) are excluded.
func isCapturedMIME(mime string) bool {
	if mime == "" {
		return true // unknown, capture conservatively
	}
	for _, prefix := range []string{"image/", "font/", "audio/", "video/"} {
		if strings.HasPrefix(mime, prefix) {
			return false
		}
	}
	if slices.Contains([]string{
		"application/octet-stream",
		"application/wasm",
		"application/pdf",
		"application/zip",
		"application/gzip",
		"application/x-protobuf",
		"application/x-msgpack",
		"application/x-thrift",
	}, mime) {
		return false
	}
	// Skip vendor binary formats; allow vnd types with text-based suffixes (+json, +xml, +csv).
	if sub, ok := strings.CutPrefix(mime, "application/vnd."); ok {
		for _, textSuffix := range []string{"+json", "+xml", "+csv"} {
			if strings.HasSuffix(sub, textSuffix) {
				return true
			}
		}
		return false
	}
	return true
}

// bodyCapFor returns the max body capture size for a MIME type.
// Structured data (JSON, XML, form data) gets 900 KB; everything else gets 10 KB.
func bodyCapFor(mime string) int {
	const fullCap = 900 * 1024
	const contextCap = 10 * 1024
	structuredPrefixes := []string{
		"application/json",
		"application/xml",
		"application/x-www-form-urlencoded",
		"application/graphql",
		"text/xml",
		"text/csv",
	}
	for _, p := range structuredPrefixes {
		if strings.HasPrefix(mime, p) {
			return fullCap
		}
	}
	// vnd types with +json/+xml suffix are treated as structured.
	for _, suffix := range []string{"+json", "+xml"} {
		if strings.HasSuffix(mime, suffix) {
			return fullCap
		}
	}
	return contextCap
}

// truncateBody caps body at the given limit on a valid UTF-8 boundary.
func truncateBody(body string, maxBody int) string {
	if len(body) <= maxBody {
		return body
	}
	// Walk back at most UTFMax bytes to find a clean rune boundary.
	i := maxBody
	for i > maxBody-utf8.UTFMax && !utf8.RuneStart(body[i]) {
		i--
	}
	return body[:i]
}
