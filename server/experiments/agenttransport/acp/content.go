package acp

import (
	"encoding/json"
	"fmt"

	transport "github.com/kernel/kernel-images/server/experiments/agenttransport"
)

// Capability checks do not rewrite the content or discard optional fields.
func (c *Client) ValidatePrompt(prompt json.RawMessage) error {
	var initialized struct {
		AgentCapabilities struct {
			PromptCapabilities struct {
				Image           bool `json:"image"`
				Audio           bool `json:"audio"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(c.Capabilities, &initialized); err != nil {
		return err
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(prompt, &blocks); err != nil || len(blocks) == 0 {
		return fmt.Errorf("%w: expected ACP content blocks", transport.ErrInvalidCommand)
	}
	caps := initialized.AgentCapabilities.PromptCapabilities
	for _, block := range blocks {
		supported := false
		switch block.Type {
		case "text", "resource_link":
			supported = true
		case "image":
			supported = caps.Image
		case "audio":
			supported = caps.Audio
		case "resource":
			supported = caps.EmbeddedContext
		}
		if !supported {
			return fmt.Errorf("%w: agent does not support prompt content type %q", transport.ErrInvalidCommand, block.Type)
		}
	}
	return nil
}
