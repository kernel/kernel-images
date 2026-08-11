package events

import "unicode/utf8"

// TruncatedSuffix marks a captured string that was cut at its cap, so a
// consumer can tell a clipped value from a complete one.
const TruncatedSuffix = "...[truncated]"

// CapturedFieldCap is the ceiling for a single captured string on an event:
// response bodies of structured types, and the source submitted to Playwright
// execution. It sits two orders of magnitude below maxS2RecordBytes so no one
// field can push an envelope past the record limit, which would null the whole
// payload rather than clip the field.
const CapturedFieldCap = 8 * 1024

// TruncateCaptured caps s at maxBytes on a rune boundary, so the result never
// splits a multi-byte character, and appends TruncatedSuffix when it cuts.
func TruncateCaptured(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	// Reserve room for the suffix within the cap.
	cutAt := maxBytes - len(TruncatedSuffix)
	if cutAt <= 0 {
		return TruncatedSuffix[:maxBytes]
	}
	// Walk forward through complete runes, stopping before we exceed cutAt.
	end := 0
	for end < cutAt {
		_, size := utf8.DecodeRuneInString(s[end:])
		if end+size > cutAt {
			break
		}
		end += size
	}
	return s[:end] + TruncatedSuffix
}
