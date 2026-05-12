package cdpmonitor

// Layer-1 PDL-faithful CDP types — one struct per dispatched event, retaining
// every top-level field the PDL declares. Layer-2 projection lives in
// handlers.go.
//
// Invariants:
//   - Every PDL field for a handled event appears here. Missing fields fail the
//     round-trip test in cdp_proto_test.go.
//   - omitempty on PDL-optional fields, omitted on PDL-required fields. A
//     required false/0/"" must round-trip, not vanish.
//   - Complex sub-objects (StackTrace, Initiator, ResourceTiming, etc.) stay as
//     json.RawMessage — retained, not typed. Callers unmarshal when needed.
//   - Network.Headers stays raw: PDL says string→string but some Chromium
//     builds emit non-string values.
//
// Naming: cdp<Domain><TypeName>, unexported. The cdp prefix avoids collisions
// with stdlib and the events package.
//
// PDL source: https://chromedevtools.github.io/devtools-protocol/tot/
// Written against Chrome M146 (ChromeDriver 146.0.7680.165).

import "encoding/json"

// --- Runtime domain ---

// cdpRuntimeRemoteObject mirrors Runtime.RemoteObject.
type cdpRuntimeRemoteObject struct {
	Type                string          `json:"type"`
	Subtype             string          `json:"subtype,omitempty"`
	ClassName           string          `json:"className,omitempty"`
	Value               json.RawMessage `json:"value,omitempty"`
	UnserializableValue string          `json:"unserializableValue,omitempty"`
	Description         string          `json:"description,omitempty"`
	DeepSerializedValue json.RawMessage `json:"deepSerializedValue,omitempty"`
	ObjectID            string          `json:"objectId,omitempty"`
	Preview             json.RawMessage `json:"preview,omitempty"`
	CustomPreview       json.RawMessage `json:"customPreview,omitempty"`
}

// cdpRuntimeConsoleAPICalledParams mirrors Runtime.consoleAPICalled params.
type cdpRuntimeConsoleAPICalledParams struct {
	Type               string                   `json:"type"`
	Args               []cdpRuntimeRemoteObject `json:"args"`
	ExecutionContextID int                      `json:"executionContextId"`
	Timestamp          float64                  `json:"timestamp"`
	StackTrace         json.RawMessage          `json:"stackTrace,omitempty"`
	Context            string                   `json:"context,omitempty"`
}

// cdpRuntimeExceptionDetails mirrors Runtime.ExceptionDetails.
// Exception is kept as json.RawMessage to avoid coupling to the RemoteObject
// surface used only inside exception payloads.
type cdpRuntimeExceptionDetails struct {
	ExceptionID        int             `json:"exceptionId"`
	Text               string          `json:"text"`
	LineNumber         int             `json:"lineNumber"`
	ColumnNumber       int             `json:"columnNumber"`
	ScriptID           string          `json:"scriptId,omitempty"`
	URL                string          `json:"url,omitempty"`
	StackTrace         json.RawMessage `json:"stackTrace,omitempty"`
	Exception          json.RawMessage `json:"exception,omitempty"`
	ExecutionContextID int             `json:"executionContextId,omitempty"`
	ExceptionMetaData  json.RawMessage `json:"exceptionMetaData,omitempty"`
}

// cdpRuntimeExceptionThrownParams mirrors Runtime.exceptionThrown params.
type cdpRuntimeExceptionThrownParams struct {
	Timestamp        float64                    `json:"timestamp"`
	ExceptionDetails cdpRuntimeExceptionDetails `json:"exceptionDetails"`
}

// cdpRuntimeBindingCalledParams mirrors Runtime.bindingCalled params.
type cdpRuntimeBindingCalledParams struct {
	Name               string `json:"name"`
	Payload            string `json:"payload"`
	ExecutionContextID int    `json:"executionContextId"`
}

// --- Network domain ---

// cdpNetworkRequest mirrors Network.Request.
type cdpNetworkRequest struct {
	URL              string          `json:"url"`
	URLFragment      string          `json:"urlFragment,omitempty"`
	Method           string          `json:"method"`
	Headers          json.RawMessage `json:"headers"`
	PostData         string          `json:"postData,omitempty"`
	HasPostData      bool            `json:"hasPostData,omitempty"`
	PostDataEntries  json.RawMessage `json:"postDataEntries,omitempty"`
	MixedContentType string          `json:"mixedContentType,omitempty"`
	InitialPriority  string          `json:"initialPriority"`
	ReferrerPolicy   string          `json:"referrerPolicy,omitempty"`
	IsLinkPreload    bool            `json:"isLinkPreload,omitempty"`
	TrustTokenParams json.RawMessage `json:"trustTokenParams,omitempty"`
	IsSameSite       bool            `json:"isSameSite,omitempty"`
	IsAdRelated      bool            `json:"isAdRelated,omitempty"`
}

// cdpNetworkResponse mirrors Network.Response.
//
// Bool fields marked required by PDL (connectionReused, fromDiskCache,
// fromServiceWorker, fromPrefetchCache) must not have omitempty: Chrome sends
// them verbatim as true or false and a false→absent coercion would break
// round-trip fidelity and mislead consumers.
type cdpNetworkResponse struct {
	URL                         string          `json:"url"`
	Status                      int             `json:"status"`
	StatusText                  string          `json:"statusText"`
	Headers                     json.RawMessage `json:"headers"`
	HeadersText                 string          `json:"headersText,omitempty"`
	MimeType                    string          `json:"mimeType"`
	Charset                     string          `json:"charset,omitempty"`
	RequestHeaders              json.RawMessage `json:"requestHeaders,omitempty"`
	RequestHeadersText          string          `json:"requestHeadersText,omitempty"`
	ConnectionReused            bool            `json:"connectionReused"`
	ConnectionID                float64         `json:"connectionId"`
	RemoteIPAddress             string          `json:"remoteIPAddress,omitempty"`
	RemotePort                  int             `json:"remotePort,omitempty"`
	FromDiskCache               bool            `json:"fromDiskCache"`
	FromServiceWorker           bool            `json:"fromServiceWorker"`
	FromPrefetchCache           bool            `json:"fromPrefetchCache"`
	FromEarlyHints              bool            `json:"fromEarlyHints,omitempty"`
	ServiceWorkerRouterInfo     json.RawMessage `json:"serviceWorkerRouterInfo,omitempty"`
	EncodedDataLength           float64         `json:"encodedDataLength"`
	Timing                      json.RawMessage `json:"timing,omitempty"`
	ServiceWorkerResponseSource string          `json:"serviceWorkerResponseSource,omitempty"`
	ResponseTime                float64         `json:"responseTime,omitempty"`
	CacheStorageCacheName       string          `json:"cacheStorageCacheName,omitempty"`
	Protocol                    string          `json:"protocol,omitempty"`
	AlternateProtocolUsage      string          `json:"alternateProtocolUsage,omitempty"`
	SecurityState               string          `json:"securityState"`
	SecurityDetails             json.RawMessage `json:"securityDetails,omitempty"`
}

// cdpNetworkRequestWillBeSentParams mirrors Network.requestWillBeSent params.
// Type (ResourceType) is PDL-required — the old projection used the wrong wire
// key "resourceType" which never matched real Chrome output.
type cdpNetworkRequestWillBeSentParams struct {
	RequestID              string            `json:"requestId"`
	LoaderID               string            `json:"loaderId"`
	DocumentURL            string            `json:"documentURL"`
	Request                cdpNetworkRequest `json:"request"`
	Timestamp              float64           `json:"timestamp"`
	WallTime               float64           `json:"wallTime"`
	Initiator              json.RawMessage   `json:"initiator"`
	RedirectHasExtraInfo   bool              `json:"redirectHasExtraInfo,omitempty"`
	RedirectResponse       json.RawMessage   `json:"redirectResponse,omitempty"`
	Type                   string            `json:"type"`
	FrameID                string            `json:"frameId,omitempty"`
	HasUserGesture         bool              `json:"hasUserGesture,omitempty"`
	RenderBlockingBehavior string            `json:"renderBlockingBehavior,omitempty"`
}

// cdpNetworkResponseReceivedParams mirrors Network.responseReceived params.
type cdpNetworkResponseReceivedParams struct {
	RequestID    string             `json:"requestId"`
	LoaderID     string             `json:"loaderId"`
	Timestamp    float64            `json:"timestamp"`
	Type         string             `json:"type"`
	Response     cdpNetworkResponse `json:"response"`
	HasExtraInfo bool               `json:"hasExtraInfo,omitempty"`
	FrameID      string             `json:"frameId,omitempty"`
}

// cdpNetworkLoadingFinishedParams mirrors Network.loadingFinished params.
type cdpNetworkLoadingFinishedParams struct {
	RequestID         string  `json:"requestId"`
	Timestamp         float64 `json:"timestamp"`
	EncodedDataLength float64 `json:"encodedDataLength"`
}

// cdpNetworkLoadingFailedParams mirrors Network.loadingFailed params.
type cdpNetworkLoadingFailedParams struct {
	RequestID       string          `json:"requestId"`
	Timestamp       float64         `json:"timestamp"`
	Type            string          `json:"type"`
	ErrorText       string          `json:"errorText"`
	Canceled        bool            `json:"canceled,omitempty"`
	BlockedReason   string          `json:"blockedReason,omitempty"`
	CorsErrorStatus json.RawMessage `json:"corsErrorStatus,omitempty"`
}

// --- Page domain ---

// cdpPageFrame mirrors Page.Frame.
type cdpPageFrame struct {
	ID                             string          `json:"id"`
	ParentID                       string          `json:"parentId,omitempty"`
	LoaderID                       string          `json:"loaderId"`
	Name                           string          `json:"name,omitempty"`
	URL                            string          `json:"url"`
	URLFragment                    string          `json:"urlFragment,omitempty"`
	DomainAndRegistry              string          `json:"domainAndRegistry,omitempty"`
	SecurityOrigin                 string          `json:"securityOrigin"`
	SecurityOriginDetails          json.RawMessage `json:"securityOriginDetails,omitempty"`
	MimeType                       string          `json:"mimeType"`
	UnreachableURL                 string          `json:"unreachableUrl,omitempty"`
	AdFrameStatus                  json.RawMessage `json:"adFrameStatus,omitempty"`
	SecureContextType              string          `json:"secureContextType,omitempty"`
	CrossOriginIsolatedContextType string          `json:"crossOriginIsolatedContextType,omitempty"`
	GatedAPIFeatures               json.RawMessage `json:"gatedAPIFeatures,omitempty"`
}

// cdpPageFrameNavigatedParams mirrors Page.frameNavigated params.
type cdpPageFrameNavigatedParams struct {
	Frame cdpPageFrame `json:"frame"`
	Type  string       `json:"type,omitempty"`
}

// cdpPageDomContentEventFiredParams mirrors Page.domContentEventFired params.
type cdpPageDomContentEventFiredParams struct {
	Timestamp float64 `json:"timestamp"`
}

// cdpPageLoadEventFiredParams mirrors Page.loadEventFired params.
type cdpPageLoadEventFiredParams struct {
	Timestamp float64 `json:"timestamp"`
}

// --- PerformanceTimeline domain ---

// cdpPerformanceTimelineEvent mirrors PerformanceTimeline.TimelineEvent.
// Only one of lcpDetails / layoutShiftDetails is populated per event,
// depending on Type.
type cdpPerformanceTimelineEvent struct {
	FrameID            string          `json:"frameId,omitempty"`
	Type               string          `json:"type"`
	Name               string          `json:"name,omitempty"`
	Time               float64         `json:"time"`
	Duration           float64         `json:"duration,omitempty"`
	LcpDetails         json.RawMessage `json:"lcpDetails,omitempty"`
	LayoutShiftDetails json.RawMessage `json:"layoutShiftDetails,omitempty"`
}

// cdpPerformanceTimelineEventAddedParams mirrors
// PerformanceTimeline.timelineEventAdded params.
type cdpPerformanceTimelineEventAddedParams struct {
	Event cdpPerformanceTimelineEvent `json:"event"`
}

// cdpLayoutShiftDetails mirrors PerformanceTimeline.LayoutShiftDetails (PDL wire format).
type cdpLayoutShiftDetails struct {
	Value          float64 `json:"value"`
	HadRecentInput bool    `json:"hadRecentInput"`
}

// cdpLcpDetails mirrors PerformanceTimeline.LargestContentfulPaintDetails (PDL wire format).
type cdpLcpDetails struct {
	RenderTime float64 `json:"renderTime"`
	LoadTime   float64 `json:"loadTime"`
	Size       float64 `json:"size"`
	ElementID  string  `json:"elementId,omitempty"`
	URL        string  `json:"url,omitempty"`
	NodeID     int     `json:"nodeId,omitempty"`
}

// --- Target domain ---

// cdpTargetTargetInfo mirrors Target.TargetInfo.
type cdpTargetTargetInfo struct {
	TargetID         string `json:"targetId"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	Attached         bool   `json:"attached"`
	OpenerID         string `json:"openerId,omitempty"`
	CanAccessOpener  bool   `json:"canAccessOpener,omitempty"`
	OpenerFrameID    string `json:"openerFrameId,omitempty"`
	ParentFrameID    string `json:"parentFrameId,omitempty"`
	BrowserContextID string `json:"browserContextId,omitempty"`
	Subtype          string `json:"subtype,omitempty"`
}

// cdpTargetAttachedToTargetParams mirrors Target.attachedToTarget params.
type cdpTargetAttachedToTargetParams struct {
	SessionID          string              `json:"sessionId"`
	TargetInfo         cdpTargetTargetInfo `json:"targetInfo"`
	WaitingForDebugger bool                `json:"waitingForDebugger"`
}

// cdpTargetDetachedFromTargetParams mirrors Target.detachedFromTarget params.
type cdpTargetDetachedFromTargetParams struct {
	SessionID string `json:"sessionId"`
	TargetID  string `json:"targetId,omitempty"`
}
