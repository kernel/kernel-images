package cdpmonitor

import (
	"encoding/json"
	"fmt"
	"time"
)

// mainSessionUnset is the sentinel stored in mainSessionID before any
// top-level frameNavigated has been received. Using a sentinel prevents the
// empty-string zero value from accidentally matching CDP messages that arrive
// on the root session (sessionID="") before navigation has been recorded.
const mainSessionUnset = "\x00unset"

// Event type constants for all events published by the cdpmonitor.
const (
	EventConsoleLog             = "console_log"
	EventConsoleError           = "console_error"
	EventNetworkRequest         = "network_request"
	EventNetworkResponse        = "network_response"
	EventNetworkLoadingFailed   = "network_loading_failed"
	EventNetworkIdle            = "network_idle"
	EventNavigation             = "navigation"
	EventDOMContentLoaded       = "dom_content_loaded"
	EventPageLoad               = "page_load"
	EventLayoutShift            = "layout_shift"
	EventLayoutSettled          = "layout_settled"
	EventNavigationSettled      = "navigation_settled"
	EventTargetCreated          = "target_created"
	EventTargetDestroyed        = "target_destroyed"
	EventInteractionClick       = "interaction_click"
	EventInteractionKey         = "interaction_key"
	EventScrollSettled          = "scroll_settled"
	EventScreenshot             = "screenshot"
	EventMonitorDisconnected    = "monitor_disconnected"
	EventMonitorReconnected     = "monitor_reconnected"
	EventMonitorReconnectFailed = "monitor_reconnect_failed"
	EventMonitorInitFailed      = "monitor_init_failed"
)

// Metadata key written into events.Source.Metadata for CDP-sourced events.
const MetadataKeyCDPSessionID = "cdp_session_id"

// CDP PerformanceTimeline event type for layout shifts.
const timelineEventLayoutShift = "layout-shift"

// CDP target type for browser pages (as opposed to workers, iframes, etc.).
const targetTypePage = "page"

// screenshot event payload key for the base64-encoded PNG data.
const screenshotDataKey = "png"

// Reason values carried in monitor lifecycle event payloads.
const (
	ReasonChromeRestarted    = "chrome_restarted"
	ReasonReconnectExhausted = "reconnect_exhausted"
)

// MonitorHealth is a point-in-time snapshot of the monitor's operational state.
type MonitorHealth struct {
	Running         bool
	PendingCommands int // in-flight send() calls
	PendingRequests int // unresolved network requests
	Sessions        int // attached CDP sessions
}

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
// ID is a pointer so we can distinguish an absent id (event) from id=0 (which
// Chrome never sends, but using a pointer is more correct than relying on that).
type cdpMessage struct {
	ID        *int64          `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

// networkReqState holds request + response metadata until loadingFinished.
type networkReqState struct {
	sessionID    string
	method       string
	url          string
	headers      json.RawMessage
	postData     string
	resourceType string
	status       int
	statusText   string
	resHeaders   json.RawMessage
	mimeType     string
	addedAt      time.Time // for TTL eviction
}

// cdpConsoleArg is a single Runtime.consoleAPICalled argument.
// Value is json.RawMessage because CDP sends strings, numbers, objects, etc.
type cdpConsoleArg struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value,omitempty"`
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
