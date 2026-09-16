package cdpmonitor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeProxyErrorRawCode(t *testing.T) {
	assert.Equal(t, "some_future_code", sanitizeProxyErrorRawCode("some_future_code"))
	assert.Equal(t, "made_up_code", sanitizeProxyErrorRawCode(" Made-Up Code "))
	assert.Equal(t, "spoofed_by_origin____script_", sanitizeProxyErrorRawCode("Spoofed-By-Origin!! <script>"))
	assert.Len(t, sanitizeProxyErrorRawCode(strings.Repeat("a", 500)), proxyErrorRawCodeMaxLen)
}
