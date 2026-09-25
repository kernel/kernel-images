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
	CustomID    string
	// Polyfill tools were read from a page-JavaScript navigator.modelContext
	// polyfill rather than the native registry. Title, OutputSchema, and
	// Hints are only populated for them.
	Polyfill     bool
	Title        string
	OutputSchema map[string]any
	Hints        map[string]bool
	Source       ToolSource
}

type ToolSource struct {
	WindowID  int
	TabID     int
	TargetID  string
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
	ref            string
	sessionID      string
	name           string
	registeredName string
	description    string
	inputSchema    map[string]any
	annotations    *Annotations
	customID       string
	frameID        string
	declarative    bool
	polyfill       bool
	rootFrame      bool
	title          string
	outputSchema   map[string]any
	hints          map[string]bool
}

func (t *registeredTool) key() string {
	if t.polyfill {
		return polyfillToolKey(t.sessionID, t.frameID, t.name)
	}
	return toolKey(t.sessionID, t.frameID, t.name)
}

type toolEvent struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	InputSchema   map[string]any `json:"inputSchema"`
	Annotations   *Annotations   `json:"annotations,omitempty"`
	FrameID       string         `json:"frameId"`
	BackendNodeID *int           `json:"backendNodeId,omitempty"`
}

type invocationResponse struct {
	InvocationID string `json:"invocationId"`
	Status       string `json:"status"`
	Output       any    `json:"output,omitempty"`
	ErrorText    string `json:"errorText,omitempty"`
}

type invocationKey struct {
	sessionID    string
	invocationID string
}
