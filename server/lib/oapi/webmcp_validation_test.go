package oapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebMCPInvokeRequestValidatesJSONContract(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{name: "valid", payload: `{"tool_ref":"wmcp_test","input":{}}`, valid: true},
		{name: "missing input", payload: `{"tool_ref":"wmcp_test"}`},
		{name: "null input", payload: `{"tool_ref":"wmcp_test","input":null}`},
		{name: "empty ref", payload: `{"tool_ref":"","input":{}}`},
		{name: "long ref", payload: `{"tool_ref":"` + strings.Repeat("x", 129) + `","input":{}}`},
		{name: "unknown property", payload: `{"tool_ref":"wmcp_test","input":{},"extra":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request WebMCPInvokeRequest
			err := json.Unmarshal([]byte(test.payload), &request)
			if test.valid {
				require.NoError(t, err)
				require.NotNil(t, request.Input)
				return
			}
			require.Error(t, err)
		})
	}
}
