package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDockerPort(t *testing.T) {
	tests := map[string]int{
		"127.0.0.1:49153":      49153,
		"0.0.0.0:32768":        32768,
		"[::1]:50000":          50000,
		"127.0.0.1:49153\n":    49153,
		"127.0.0.1:49153\n::1": 49153,
	}
	for input, want := range tests {
		got, err := parseDockerPort(input)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestSummarizeDataRedactsScreenshotsAndLargeFields(t *testing.T) {
	raw := json.RawMessage(`{
		"png":"abcd",
		"headers":{"Authorization":"Bearer token"},
		"stack_trace":[{"functionName":"f"}],
		"url":"https://example.test"
	}`)

	got := summarizeData(raw, 1000)

	assert.Contains(t, got, `"<redacted 4 base64 chars>"`)
	assert.Contains(t, got, `"<headers `)
	assert.Contains(t, got, `"<stack_trace `)
	assert.Contains(t, got, `"url":"https://example.test"`)
	assert.NotContains(t, got, "Bearer token")
}
