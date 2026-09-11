package devtoolsproxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Vault fills pass values as arguments to a static Runtime.callFunctionOn,
// never as Input.insertText. Runtime payloads must stay outside CDP telemetry.
func TestVaultFillRuntimeArgumentsAreNotCaptured(t *testing.T) {
	payload := []byte(`{"id":1,"method":"Runtime.callFunctionOn","params":{"objectId":"pinned-input","functionDeclaration":"(element, value) => writePinned(element, value)","arguments":[{"value":"secret-value"}]}}`)
	_, supported := supportedMethod(payload)
	require.False(t, supported)
	event, captured := cdpCommandEvent(payload, 1, "connection", "Runtime.callFunctionOn")
	require.False(t, captured)
	require.Empty(t, event.Data)
}
