package cdpmonitor

import (
	"regexp"
	"strings"
)

// proxyErrorUnknownCode is the published code for an X-Kernel-Proxy-Error
// header value this image does not recognize.
const proxyErrorUnknownCode = "unknown"

// proxyErrorRawCodeMaxLen bounds raw_code so a hostile origin cannot pad the
// event with arbitrary header text.
const proxyErrorRawCodeMaxLen = 64

var proxyErrorRawCodeInvalid = regexp.MustCompile(`[^a-z0-9_]`)

// sanitizeProxyErrorRawCode reduces a header value to the character set the
// proxy uses for its own codes: lowercase, [a-z0-9_] only, at most 64 bytes.
func sanitizeProxyErrorRawCode(s string) string {
	s = proxyErrorRawCodeInvalid.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "_")
	if len(s) > proxyErrorRawCodeMaxLen {
		s = s[:proxyErrorRawCodeMaxLen]
	}
	return s
}
