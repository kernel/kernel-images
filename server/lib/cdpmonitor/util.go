package cdpmonitor

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func ptrOf[T any](v T) *T { return &v }

// marshalNavEventContext marshals a navContext and sequence number into the
// BrowserEventContext JSON payload used by network_idle, page_layout_settled,
// and page_navigation_settled events.
func marshalNavEventContext(ctx navContext, seq int) json.RawMessage {
	data, _ := json.Marshal(oapi.BrowserEventContext{
		SessionId:  ctx.sessionID,
		TargetId:   ctx.targetID,
		TargetType: oapi.BrowserTargetType(ctx.targetType),
		FrameId:    ptrOf(ctx.frameID),
		LoaderId:   ptrOf(ctx.loaderID),
		Url:        ptrOf(ctx.url),
		NavSeq:     int64(seq),
	})
	return data
}

// consoleArgString extracts a display string from a CDP console argument.
// For strings it unquotes the JSON value; for other types it returns the raw JSON.
func consoleArgString(a cdpRuntimeRemoteObject) string {
	if len(a.Value) == 0 {
		return a.Type // e.g. "undefined", "null"
	}
	if a.Type == "string" {
		var s string
		if json.Unmarshal(a.Value, &s) == nil {
			return s
		}
	}
	return string(a.Value)
}

// isTextualResource reports whether the resource warrants body capture.
// resourceType is checked first; mimeType is a fallback for resources with no type (e.g. in-flight at attach time).
func isTextualResource(resourceType, mimeType string) bool {
	switch resourceType {
	case "Font", "Image", "Media", "Stylesheet", "Script":
		return false
	}
	return isCapturedMIME(mimeType)
}

// isCapturedMIME returns true for MIME types whose bodies are worth capturing.
// Uses an allow-list of known textual types; everything else is excluded.
func isCapturedMIME(mime string) bool {
	if mime == "" {
		return false
	}
	// Allow plain text subtypes.
	if strings.HasPrefix(mime, "text/plain") ||
		strings.HasPrefix(mime, "text/html") ||
		strings.HasPrefix(mime, "text/xml") ||
		strings.HasPrefix(mime, "text/csv") {
		return true
	}
	// Allow structured application types.
	if strings.HasPrefix(mime, "application/json") ||
		strings.HasPrefix(mime, "application/xml") ||
		strings.HasPrefix(mime, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(mime, "application/graphql") {
		return true
	}
	// Allow vendor types with text-based suffixes.
	if sub, ok := strings.CutPrefix(mime, "application/vnd."); ok {
		for _, textSuffix := range []string{"+json", "+xml", "+csv"} {
			if strings.HasSuffix(sub, textSuffix) {
				return true
			}
		}
	}
	return false
}

// structuredPrefixes lists MIME type prefixes that warrant full (8 KB) body capture.
var structuredPrefixes = []string{
	"application/json",
	"application/xml",
	"application/x-www-form-urlencoded",
	"application/graphql",
	"text/xml",
	"text/csv",
}

// bodyCapFor returns the max body capture size for a MIME type.
// Structured data (JSON, XML, CSV, form data) gets 8 KB; everything else gets 4 KB.
// Vendor types with +json, +xml, or +csv suffixes are also treated as structured,
// matching the allow-list in isCapturedMIME.
func bodyCapFor(mime string) int {
	const fullCap = 8 * 1024
	const contextCap = 4 * 1024
	for _, p := range structuredPrefixes {
		if strings.HasPrefix(mime, p) {
			return fullCap
		}
	}
	// vnd types with +json/+xml/+csv suffix are treated as structured.
	for _, suffix := range []string{"+json", "+xml", "+csv"} {
		if strings.HasSuffix(mime, suffix) {
			return fullCap
		}
	}
	return contextCap
}

const truncatedSuffix = "...[truncated]"

// truncateBody caps body at the given limit on a valid UTF-8 boundary.
// The result never splits a multi-byte rune. A truncation suffix is appended
// when the body is cut so callers can distinguish truncated from full content.
func truncateBody(body string, maxBody int) string {
	if len(body) <= maxBody {
		return body
	}
	if maxBody <= 0 {
		return ""
	}
	// Reserve room for the truncation suffix within the limit.
	cutAt := maxBody - len(truncatedSuffix)
	if cutAt <= 0 {
		return truncatedSuffix[:maxBody]
	}
	// Walk forward through complete runes, stopping before we exceed cutAt.
	end := 0
	for end < cutAt {
		_, size := utf8.DecodeRuneInString(body[end:])
		if end+size > cutAt {
			break
		}
		end += size
	}
	return body[:end] + truncatedSuffix
}
