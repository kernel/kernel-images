package cdpmonitor

import (
	"encoding/json"
	"fmt"
)

// targetInfo holds metadata about an attached CDP target/session.
type targetInfo struct {
	targetID   string
	url        string
	targetType string
}

// cdpError is the JSON-RPC error object returned by Chrome.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// cdpMessage is the JSON-RPC message envelope used by Chrome's DevTools Protocol.
type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

// networkReqState holds request + response metadata until loadingFinished.
type networkReqState struct {
	method       string
	url          string
	headers      json.RawMessage
	postData     string
	resourceType string
	initiator    json.RawMessage
	status       int
	statusText   string
	resHeaders   json.RawMessage
	mimeType     string
}

// cdpConsoleArg is a single Runtime.consoleAPICalled argument.
type cdpConsoleArg struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// cdpConsoleParams is the shape of Runtime.consoleAPICalled params.
type cdpConsoleParams struct {
	Type       string          `json:"type"`
	Args       []cdpConsoleArg `json:"args"`
	StackTrace json.RawMessage `json:"stackTrace"`
}

// cdpExceptionDetails is the shape of Runtime.exceptionThrown params.
type cdpExceptionDetails struct {
	ExceptionDetails struct {
		Text         string          `json:"text"`
		LineNumber   int             `json:"lineNumber"`
		ColumnNumber int             `json:"columnNumber"`
		URL          string          `json:"url"`
		StackTrace   json.RawMessage `json:"stackTrace"`
	} `json:"exceptionDetails"`
}

// cdpTargetInfo is the shared TargetInfo shape used by Target events.
type cdpTargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// cdpNetworkRequestParams is the shape of Network.requestWillBeSent params.
type cdpNetworkRequestParams struct {
	RequestID    string `json:"requestId"`
	ResourceType string `json:"resourceType"`
	Request      struct {
		Method   string          `json:"method"`
		URL      string          `json:"url"`
		Headers  json.RawMessage `json:"headers"`
		PostData string          `json:"postData"`
	} `json:"request"`
	Initiator json.RawMessage `json:"initiator"`
}

// cdpResponseReceivedParams is the shape of Network.responseReceived params.
type cdpResponseReceivedParams struct {
	RequestID string `json:"requestId"`
	Response  struct {
		Status     int             `json:"status"`
		StatusText string          `json:"statusText"`
		Headers    json.RawMessage `json:"headers"`
		MimeType   string          `json:"mimeType"`
	} `json:"response"`
}

// cdpAttachedToTargetParams is the shape of Target.attachedToTarget params.
type cdpAttachedToTargetParams struct {
	SessionID  string        `json:"sessionId"`
	TargetInfo cdpTargetInfo `json:"targetInfo"`
}

// cdpTargetCreatedParams is the shape of Target.targetCreated params.
type cdpTargetCreatedParams struct {
	TargetInfo cdpTargetInfo `json:"targetInfo"`
}
