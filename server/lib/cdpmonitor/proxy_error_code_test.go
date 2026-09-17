package cdpmonitor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeProxyErrorRawCode(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "unchanged", input: "some_future_code", want: "some_future_code"},
		{name: "case and separators", input: "Made-Up Code", want: "made_up_code"},
		{name: "leading and trailing whitespace", input: " Made-Up Code ", want: "_made_up_code_"},
		{name: "markup", input: "Spoofed-By-Origin!! <script>", want: "spoofed_by_origin____script_"},
		{name: "controls", input: "\tFuture\nCode\r", want: "_future_code_"},
		{name: "empty", input: "", want: ""},
		{name: "long ascii", input: strings.Repeat("a", 500), want: strings.Repeat("a", proxyErrorRawCodeMaxLen)},
		{name: "replace before truncating", input: strings.Repeat("🙂", proxyErrorRawCodeMaxLen+1), want: strings.Repeat("_", proxyErrorRawCodeMaxLen)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeProxyErrorRawCode(tt.input))
		})
	}
}
