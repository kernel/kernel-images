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
	EventConsoleLog           = "console_log"            // Runtime.consoleAPICalled (non-error types)
	EventConsoleError         = "console_error"          // Runtime.consoleAPICalled (type=error) or Runtime.exceptionThrown
	EventNetworkRequest       = "network_request"        // Network.requestWillBeSent
	EventNetworkResponse      = "network_response"       // Network.loadingFinished (with prior responseReceived)
	EventNetworkLoadingFailed = "network_loading_failed" // Network.loadingFailed
	EventNavigation           = "page_navigation"        // Page.frameNavigated
	EventDOMContentLoaded     = "page_dom_content_loaded" // Page.domContentEventFired
	EventPageLoad             = "page_load"               // Page.loadEventFired
	EventLayoutShift          = "page_layout_shift"       // PerformanceTimeline event of type "layout-shift"
	EventLCP                  = "page_lcp"                // PerformanceTimeline event of type "largest-contentful-paint"
	EventTabOpened            = "page_tab_opened"         // Target.attachedToTarget for type=page
)

// Computed events — synthetic events derived by computed.go state machines.
// None of these correspond to a single CDP notification; they are inferred from
// sequences of CDP events and debounce timers.
const (
	EventNetworkIdle       = "network_idle"           // 500 ms after all in-flight requests finish
	EventLayoutSettled     = "page_layout_settled"    // 1 s after page_load with no intervening layout shifts
	EventNavigationSettled = "page_navigation_settled" // fires once page_dom_content_loaded and page_layout_settled both hold
)

// Interaction events — fired by injected page-side JS (interaction.js) via the
// Runtime.bindingCalled mechanism. They originate in the browser's renderer
// process, not from Chrome's network or page domains.
const (
	EventInteractionClick = "interaction_click"         // document click (target selector, coords, text)
	EventInteractionKey   = "interaction_key"           // keydown (key name, target selector)
	EventScrollSettled    = "interaction_scroll_settled" // 300 ms after the last scroll event on a target
)

// Monitor lifecycle and internal events — emitted by the monitor itself, not by Chrome.
const (
	EventScreenshot             = "monitor_screenshot"    // ffmpeg frame capture on page load or JS exception
	EventMonitorDisconnected    = "monitor_disconnected"    // WebSocket to Chrome closed unexpectedly
	EventMonitorReconnected     = "monitor_reconnected"     // successfully reconnected after a disconnect
	EventMonitorReconnectFailed = "monitor_reconnect_failed" // reconnect attempts exhausted
	EventMonitorInitFailed      = "monitor_init_failed"     // could not initialise the CDP session
)

// Metadata keys written into events.Source.Metadata for CDP-sourced events.
const (
	MetadataKeyCDPSessionID = "cdp_session_id"
	MetadataKeyTargetID     = "target_id"
	MetadataKeyTargetType   = "target_type"
)

const (
	timelineEventLayoutShift = "layout-shift"
	timelineEventLCP         = "largest-contentful-paint"
)

const cdpMethodSetAutoAttach = "Target.setAutoAttach"

// CDP target type for browser pages (as opposed to workers, iframes, etc.).
const targetTypePage = "page"

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
	loaderID     string
	frameID      string
	navSeq       int64
	status       int
	statusText   string
	resHeaders   json.RawMessage
	mimeType     string
	addedAt      time.Time // for TTL eviction
}

// navContext carries the identity of the navigation that owns a computedState.
// Stamped at Page.frameNavigated and precomputed into event payloads/metadata.
type navContext struct {
	sessionID  string
	targetID   string
	targetType string
	frameID    string
	loaderID   string
	url        string
}
