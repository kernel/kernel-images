package webmcpclient

import (
	"errors"
)

var (
	ErrNoPageTarget   = errors.New("no browser tabs found")
	ErrToolNotFound   = errors.New("WebMCP tool not found")
	ErrOutcomeUnknown = errors.New("WebMCP invocation outcome is unknown")
)

type Tool struct {
	Ref         string
	Name        string
	Description string
	InputSchema map[string]any
	Annotations *Annotations
	Source      ToolSource
}

type ToolSource struct {
	WindowID  int
	TabID     int
	PageTitle string
	PageURL   string
	Frame     *ToolFrame
}

type ToolFrame struct {
	FrameID int
	URL     string
}

type Annotations struct {
	ReadOnly         bool `json:"readOnly"`
	UntrustedContent bool `json:"untrustedContent"`
	Consequential    bool `json:"consequential"`
	Autosubmit       bool `json:"autosubmit"`
}

type InvocationResult struct {
	InvocationID string
	Status       string
	Output       any
	ErrorText    string
}

type registeredTool struct {
	ref         string
	sessionID   string
	name        string
	description string
	inputSchema map[string]any
	annotations *Annotations
	frameID     string
}

type toolEvent struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *Annotations   `json:"annotations,omitempty"`
	FrameID     string         `json:"frameId"`
}

type invocationResponse struct {
	InvocationID string `json:"invocationId"`
	Status       string `json:"status"`
	Output       any    `json:"output,omitempty"`
	ErrorText    string `json:"errorText,omitempty"`
	observedAt   uint64
}

type invocationKey struct {
	sessionID    string
	invocationID string
}

type documentKey struct {
	sessionID string
	frameID   string
}
