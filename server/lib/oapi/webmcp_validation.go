package oapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

func (r *WebMCPInvokeRequest) UnmarshalJSON(data []byte) error {
	type request WebMCPInvokeRequest
	var decoded request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if decoded.Input == nil {
		return fmt.Errorf("input is required and must be an object")
	}
	toolRefLength := utf8.RuneCountInString(decoded.ToolRef)
	if toolRefLength < 1 || toolRefLength > 128 {
		return fmt.Errorf("tool_ref must be between 1 and 128 characters")
	}
	*r = WebMCPInvokeRequest(decoded)
	return nil
}
