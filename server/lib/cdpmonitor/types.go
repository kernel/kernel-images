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

// CDP-derived events — direct translations of DevTools Protocol notifications.
// Each maps 1-to-1 with a specific CDP domain event (Runtime.*, Network.*,
// Page.*, PerformanceTimeline.*) received from Chrome.
const (
	EventConsoleLog           = "console_log"             // Runtime.consoleAPICalled (type=log) or Runtime.exceptionThrown
	EventConsoleError         = "console_error"           // Runtime.consoleAPICalled (type=error)
	EventNetworkRequest       = "network_request"         // Network.requestWillBeSent
	EventNetworkResponse      = "network_response"        // Network.loadingFinished (with prior responseReceived)
	EventNetworkLoadingFailed = "network_loading_failed"  // Network.loadingFailed
	EventNavigation           = "navigation"              // Page.frameNavigated
	EventDOMContentLoaded     = "dom_content_loaded"      // Page.domContentEventFired
	EventPageLoad             = "page_load"               // Page.loadEventFired
	EventLayoutShift          = "layout_shift"            // PerformanceTimeline event of type "layout-shift"
	EventTargetCreated        = "target_created"          // Target.targetCreated
	EventTargetDestroyed      = "target_destroyed"        // Target.targetDestroyed
)

// Computed events — synthetic events derived by computed.go state machines.
// None of these correspond to a single CDP notification; they are inferred from
// sequences of CDP events and debounce timers.
const (
	EventNetworkIdle       = "network_idle"        // 500 ms after all in-flight requests finish
	EventLayoutSettled     = "layout_settled"      // 1 s after page_load with no intervening layout shifts
	EventNavigationSettled = "navigation_settled"  // fires once dom_content_loaded + network_idle + layout_settled all hold
)

// Interaction events — fired by injected page-side JS (interaction.js) via the
// Runtime.bindingCalled mechanism. They originate in the browser's renderer
// process, not from Chrome's network or page domains.
const (
	EventInteractionClick = "interaction_click"  // document click (target selector, coords, text)
	EventInteractionKey   = "interaction_key"    // keydown (key name, target selector)
	EventScrollSettled    = "scroll_settled"     // 300 ms after the last scroll event on a target
)

// Monitor lifecycle and internal events — emitted by the monitor itself, not by Chrome.
const (
	EventScreenshot             = "screenshot"              // periodic ffmpeg frame capture
	EventMonitorDisconnected    = "monitor_disconnected"    // WebSocket to Chrome closed unexpectedly
	EventMonitorReconnected     = "monitor_reconnected"     // successfully reconnected after a disconnect
	EventMonitorReconnectFailed = "monitor_reconnect_failed" // reconnect attempts exhausted
	EventMonitorInitFailed      = "monitor_init_failed"     // could not initialise the CDP session
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
