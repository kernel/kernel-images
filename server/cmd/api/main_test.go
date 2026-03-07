package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteChromeURLs_RewritesNestedMapsAndArrays(t *testing.T) {
	chromeHost := "127.0.0.1:9223"
	proxyHost := "127.0.0.1:4444"

	payload := map[string]interface{}{
		"webSocketDebuggerUrl": "ws://127.0.0.1:9223/devtools/browser/root",
		"nested": map[string]interface{}{
			"devtoolsFrontendUrl": "https://chrome-devtools-frontend.appspot.com/serve_rev/@abc/inspector.html?ws=127.0.0.1:9223/devtools/page/nested",
		},
		"targets": []interface{}{
			map[string]interface{}{
				"webSocketDebuggerUrl": "ws://127.0.0.1:9223/devtools/page/in-array",
			},
		},
	}

	rewriteChromeURLs(payload, chromeHost, proxyHost)

	require.Equal(t,
		"ws://127.0.0.1:4444/devtools/browser/root",
		payload["webSocketDebuggerUrl"],
	)

	nested, ok := payload["nested"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t,
		"https://chrome-devtools-frontend.appspot.com/serve_rev/@abc/inspector.html?ws=127.0.0.1%3A4444%2Fdevtools%2Fpage%2Fnested",
		nested["devtoolsFrontendUrl"],
	)

	targets, ok := payload["targets"].([]interface{})
	require.True(t, ok)
	first, ok := targets[0].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t,
		"ws://127.0.0.1:4444/devtools/page/in-array",
		first["webSocketDebuggerUrl"],
	)
}
