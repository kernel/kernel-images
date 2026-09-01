package webmcpclient

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrNoPageTarget   = errors.New("no page target found")
	ErrToolNotFound   = errors.New("WebMCP tool not found")
	ErrOutcomeUnknown = errors.New("WebMCP invocation outcome is unknown")
)

type Tool struct {
	Ref           string
	Name          string
	Description   string
	InputSchema   map[string]any
	Annotations   *Annotations
	PageTargetID  string
	TargetID      string
	TargetType    string
	TargetURL     string
	FrameID       string
	FrameURL      string
	ParentFrameID string
	DocumentRef   string
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

type cdpRequest struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

type commandResult struct {
	result json.RawMessage
	err    error
}

type targetInfo struct {
	TargetID      string `json:"targetId"`
	Type          string `json:"type"`
	URL           string `json:"url"`
	ParentFrameID string `json:"parentFrameId,omitempty"`
}

type session struct {
	id       string
	parentID string
	target   targetInfo
}

type frameInfo struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	LoaderID string `json:"loaderId"`
	URL      string `json:"url"`
}

type frameTree struct {
	Frame       frameInfo   `json:"frame"`
	ChildFrames []frameTree `json:"childFrames,omitempty"`
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
}

type invocationKey struct {
	sessionID    string
	invocationID string
}
