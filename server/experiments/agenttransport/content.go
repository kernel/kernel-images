package agenttransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxPromptBytes bounds the encoded content array, including base64 media.
const MaxPromptBytes = 4 << 20

var ErrInvalidCommand = errors.New("invalid command")

// TextPrompt is a convenience for callers that only need one ACP text block.
func TextPrompt(text string) json.RawMessage {
	data, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	return data
}

// Keep content as JSON rather than a reduced union that drops ACP metadata.
// Canonical member ordering makes retries insensitive to JSON formatting.
func normalizePrompt(prompt json.RawMessage) (json.RawMessage, error) {
	if len(prompt) > MaxPromptBytes {
		return nil, fmt.Errorf("%w: prompt exceeds size limit", ErrInvalidCommand)
	}
	decoder := json.NewDecoder(bytes.NewReader(prompt))
	decoder.UseNumber()
	var blocks []map[string]any
	if err := decoder.Decode(&blocks); err != nil || len(blocks) == 0 {
		return nil, fmt.Errorf("%w: prompt must be a nonempty ACP content array", ErrInvalidCommand)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w: expected one content array", ErrInvalidCommand)
	}
	for _, block := range blocks {
		if kind, ok := block["type"].(string); !ok || kind == "" {
			return nil, fmt.Errorf("%w: content block requires type", ErrInvalidCommand)
		}
	}
	data, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxPromptBytes {
		return nil, fmt.Errorf("%w: prompt exceeds size limit", ErrInvalidCommand)
	}
	return data, nil
}

func copyOperation(op Operation) Operation {
	op.Command.Prompt = append(json.RawMessage(nil), op.Command.Prompt...)
	return op
}
