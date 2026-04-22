package cdpmonitor

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
)

// logUnmarshalErr logs a Debug message when a handler can't parse CDP params.
// These indicate Chrome sent an unexpected params shape, rare and non-actionable
// at Warn/Error level, but useful in verbose mode.
func (m *Monitor) logUnmarshalErr(method string, err error) {
	m.log.Debug("cdpmonitor: failed to parse CDP params", "method", method, "err", err)
}

// publishEvent stamps common fields and publishes an event.
func (m *Monitor) publishEvent(eventType string, category events.EventCategory, source events.Source, sourceEvent string, data json.RawMessage, sessionID string) {
	src := source
	src.Event = sourceEvent
	if sessionID != "" {
		if src.Metadata == nil {
			src.Metadata = make(map[string]string)
		}
		src.Metadata[MetadataKeyCDPSessionID] = sessionID
	}
	m.publish(events.Event{
		Ts:       time.Now().UnixMicro(),
		Type:     eventType,
		Category: category,
		Source:   src,
		Data:     data,
	})
}

// decodeParams unmarshals msg.Params into dst, logging on failure.
// Returns true on success so dispatch can gate the handler call.
func (m *Monitor) decodeParams(method string, params json.RawMessage, dst any) bool {
	if err := json.Unmarshal(params, dst); err != nil {
		m.logUnmarshalErr(method, err)
		return false
	}
	return true
}

// dispatchEvent routes a CDP event to its handler.
func (m *Monitor) dispatchEvent(msg cdpMessage) {
	switch msg.Method {
	case "Runtime.consoleAPICalled":
		var p cdpRuntimeConsoleAPICalledParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleConsole(p, msg.SessionID)
		}
	case "Runtime.exceptionThrown":
		var p cdpRuntimeExceptionThrownParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleExceptionThrown(p, msg.SessionID)
		}
	case "Runtime.bindingCalled":
		var p cdpRuntimeBindingCalledParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleBindingCalled(p, msg.SessionID)
		}
	case "Network.requestWillBeSent":
		var p cdpNetworkRequestWillBeSentParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleNetworkRequest(p, msg.SessionID)
		}
	case "Network.responseReceived":
		var p cdpNetworkResponseReceivedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleResponseReceived(p, msg.SessionID)
		}
	case "Network.loadingFinished":
		var p cdpNetworkLoadingFinishedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleLoadingFinished(p, msg.SessionID)
		}
	case "Network.loadingFailed":
		var p cdpNetworkLoadingFailedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleLoadingFailed(p, msg.SessionID)
		}
	case "Page.frameNavigated":
		var p cdpPageFrameNavigatedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleFrameNavigated(p, msg.SessionID)
		}
	case "Page.domContentEventFired":
		var p cdpPageDomContentEventFiredParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleDOMContentLoaded(p, msg.SessionID)
		}
	case "Page.loadEventFired":
		var p cdpPageLoadEventFiredParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleLoadEventFired(p, msg.SessionID)
		}
	case "PerformanceTimeline.timelineEventAdded":
		var p cdpPerformanceTimelineEventAddedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleTimelineEvent(p, msg.SessionID)
		}
	case "Target.attachedToTarget":
		var p cdpTargetAttachedToTargetParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleAttachedToTarget(p)
		}
	case "Target.targetCreated":
		var p cdpTargetTargetCreatedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleTargetCreated(p, msg.SessionID)
		}
	case "Target.targetDestroyed":
		var p cdpTargetTargetDestroyedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleTargetDestroyed(p, msg.SessionID)
		}
	case "Target.detachedFromTarget":
		var p cdpTargetDetachedFromTargetParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleDetachedFromTarget(p)
		}
	}
}

func (m *Monitor) handleConsole(p cdpRuntimeConsoleAPICalledParams, sessionID string) {
	text := ""
	if len(p.Args) > 0 {
		text = consoleArgString(p.Args[0])
	}
	argValues := make([]string, 0, len(p.Args))
	for _, a := range p.Args {
		argValues = append(argValues, consoleArgString(a))
	}
	data, _ := json.Marshal(map[string]any{
		"level":       p.Type,
		"text":        text,
		"args":        argValues,
		"stack_trace": p.StackTrace,
	})
	eventType := EventConsoleLog
	if p.Type == "error" {
		eventType = EventConsoleError
	}
	m.publishEvent(eventType, events.CategoryConsole, events.Source{Kind: events.KindCDP}, "Runtime.consoleAPICalled", data, sessionID)
}

func (m *Monitor) handleExceptionThrown(p cdpRuntimeExceptionThrownParams, sessionID string) {
	data, _ := json.Marshal(map[string]any{
		"text":        p.ExceptionDetails.Text,
		"line":        p.ExceptionDetails.LineNumber,
		"column":      p.ExceptionDetails.ColumnNumber,
		"url":         p.ExceptionDetails.URL,
		"stack_trace": p.ExceptionDetails.StackTrace,
	})
	m.publishEvent(EventConsoleError, events.CategoryConsole, events.Source{Kind: events.KindCDP}, "Runtime.exceptionThrown", data, sessionID)
	m.tryScreenshot(m.getLifecycleCtx())
}

// bindingMinInterval is the minimum time between accepted __kernelEvent binding
// calls per session. This caps throughput at 20 events/s per session, preventing
// a misbehaving page from flooding the event pipeline.
const bindingMinInterval = 50 * time.Millisecond

// handleBindingCalled processes __kernelEvent binding calls from the page.
func (m *Monitor) handleBindingCalled(p cdpRuntimeBindingCalledParams, sessionID string) {
	if p.Name != bindingName {
		return
	}

	payload := json.RawMessage(p.Payload)
	if !json.Valid(payload) {
		return
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return
	}
	switch header.Type {
	case EventInteractionClick, EventInteractionKey, EventScrollSettled:
	default:
		return
	}

	// Rate-limit per (session, event type): cap at 20 events/s per pair so a
	// misbehaving page cannot flood the event pipeline with a single event type.
	now := time.Now()
	rateKey := sessionID + ":" + header.Type
	m.bindingRateMu.Lock()
	last := m.bindingLastSeen[rateKey]
	if now.Sub(last) < bindingMinInterval {
		m.bindingRateMu.Unlock()
		return
	}
	m.bindingLastSeen[rateKey] = now
	m.bindingRateMu.Unlock()

	m.publishEvent(header.Type, events.CategoryInteraction, events.Source{Kind: events.KindCDP}, "Runtime.bindingCalled", payload, sessionID)
}

// handleTimelineEvent processes PerformanceTimeline layout-shift events.
func (m *Monitor) handleTimelineEvent(p cdpPerformanceTimelineEventAddedParams, sessionID string) {
	if p.Event.Type != timelineEventLayoutShift {
		return
	}
	data, _ := json.Marshal(p)
	m.publishEvent(EventLayoutShift, events.CategoryPage, events.Source{Kind: events.KindCDP}, "PerformanceTimeline.timelineEventAdded", data, sessionID)
	m.computed.onLayoutShift()
}

// handleNetworkRequest publishes network_request events.
// NOTE: events include raw headers, post_data, and (on response) truncated
// bodies which may contain cookies, bearer tokens, or other credentials.
// This mirrors what CDP/DevTools itself exposes. Consumers should treat the
// event stream as privileged data; opt-in redaction can be added later.
func (m *Monitor) handleNetworkRequest(p cdpNetworkRequestWillBeSentParams, sessionID string) {
	// Extract only the initiator type; the stack trace is too verbose and dominates event size.
	var initiatorType string
	var raw struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(p.Initiator, &raw) == nil {
		initiatorType = raw.Type
	}

	// Redirects reuse the same requestId and fire additional requestWillBeSent
	// events, but only a single loadingFinished fires per chain. Only increment
	// netPending for genuinely new requests to avoid permanently inflating the
	// counter and blocking network_idle.
	m.pendReqMu.Lock()
	existing, isRedirect := m.pendingRequests[p.RequestID]
	addedAt := existing.addedAt
	if !isRedirect {
		addedAt = time.Now()
	}
	m.pendingRequests[p.RequestID] = networkReqState{
		sessionID:    sessionID,
		method:       p.Request.Method,
		url:          p.Request.URL,
		headers:      p.Request.Headers,
		postData:     p.Request.PostData,
		resourceType: p.Type,
		addedAt:      addedAt,
	}
	m.pendReqMu.Unlock()
	ev := map[string]any{
		"method":         p.Request.Method,
		"url":            p.Request.URL,
		"headers":        p.Request.Headers,
		"initiator_type": initiatorType,
	}
	if p.Request.PostData != "" {
		ev["post_data"] = p.Request.PostData
	}
	if p.Type != "" {
		ev["resource_type"] = p.Type
	}
	data, _ := json.Marshal(ev)
	m.publishEvent(EventNetworkRequest, events.CategoryNetwork, events.Source{Kind: events.KindCDP}, "Network.requestWillBeSent", data, sessionID)
	if !isRedirect {
		m.computed.onRequest()
	}
}

func (m *Monitor) handleResponseReceived(p cdpNetworkResponseReceivedParams, _ string) {
	m.pendReqMu.Lock()
	if state, ok := m.pendingRequests[p.RequestID]; ok {
		state.status = p.Response.Status
		state.statusText = p.Response.StatusText
		state.resHeaders = p.Response.Headers
		state.mimeType = p.Response.MimeType
		m.pendingRequests[p.RequestID] = state
	}
	m.pendReqMu.Unlock()
}

func (m *Monitor) handleLoadingFinished(p cdpNetworkLoadingFinishedParams, sessionID string) {
	m.pendReqMu.Lock()
	state, ok := m.pendingRequests[p.RequestID]
	if ok {
		delete(m.pendingRequests, p.RequestID)
	}
	m.pendReqMu.Unlock()
	if !ok {
		return
	}
	m.computed.onLoadingFinished()
	// Fetch response body async to avoid blocking readLoop; binary types are skipped.
	m.asyncWg.Go(func() {
		body := m.fetchResponseBody(p.RequestID, sessionID, state)
		ev := map[string]any{
			"method":  state.method,
			"url":     state.url,
			"status":  state.status,
			"headers": state.resHeaders,
		}
		if state.statusText != "" {
			ev["status_text"] = state.statusText
		}
		if state.mimeType != "" {
			ev["mime_type"] = state.mimeType
		}
		if state.resourceType != "" {
			ev["resource_type"] = state.resourceType
		}
		if body != "" {
			ev["body"] = body
		}
		data, _ := json.Marshal(ev)
		m.publishEvent(EventNetworkResponse, events.CategoryNetwork, events.Source{Kind: events.KindCDP}, "Network.loadingFinished", data, sessionID)
	})
}

// fetchResponseBody retrieves and truncates the response body for textual resources.
func (m *Monitor) fetchResponseBody(requestID, sessionID string, state networkReqState) string {
	if !isTextualResource(state.resourceType, state.mimeType) {
		return ""
	}
	result, err := m.send(m.getLifecycleCtx(), "Network.getResponseBody", map[string]any{
		"requestId": requestID,
	}, sessionID)
	if err != nil {
		return ""
	}
	var resp struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if json.Unmarshal(result, &resp) != nil {
		return ""
	}
	body := resp.Body
	if resp.Base64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return ""
		}
		body = string(decoded)
	}
	return truncateBody(body, bodyCapFor(state.mimeType))
}

func (m *Monitor) handleLoadingFailed(p cdpNetworkLoadingFailedParams, sessionID string) {
	m.pendReqMu.Lock()
	state, ok := m.pendingRequests[p.RequestID]
	if ok {
		delete(m.pendingRequests, p.RequestID)
	}
	m.pendReqMu.Unlock()

	ev := map[string]any{
		"error_text": p.ErrorText,
		"canceled":   p.Canceled,
	}
	if ok {
		ev["url"] = state.url
	}
	data, _ := json.Marshal(ev)
	m.publishEvent(EventNetworkLoadingFailed, events.CategoryNetwork, events.Source{Kind: events.KindCDP}, "Network.loadingFailed", data, sessionID)
	if ok {
		m.computed.onLoadingFinished()
	}
}

func (m *Monitor) handleFrameNavigated(p cdpPageFrameNavigatedParams, sessionID string) {
	data, _ := json.Marshal(map[string]any{
		"url":             p.Frame.URL,
		"frame_id":        p.Frame.ID,
		"parent_frame_id": p.Frame.ParentID,
	})
	m.publishEvent(EventNavigation, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.frameNavigated", data, sessionID)

	// Only reset state for top-level navigations; subframe (iframe) navigations
	// should not disrupt main-page tracking.
	if p.Frame.ParentID == "" {
		m.mainSessionID.Store(sessionID)
		m.pendReqMu.Lock()
		for id, req := range m.pendingRequests {
			if req.sessionID == sessionID {
				delete(m.pendingRequests, id)
			}
		}
		inflight := len(m.pendingRequests)
		// Reset computed state while still holding pendReqMu so new requests
		// arriving concurrently can't increment netPending before the reset.
		m.computed.resetOnNavigation(inflight)
		m.pendReqMu.Unlock()
	}
}

func (m *Monitor) handleDOMContentLoaded(p cdpPageDomContentEventFiredParams, sessionID string) {
	data, _ := json.Marshal(p)
	m.publishEvent(EventDOMContentLoaded, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.domContentEventFired", data, sessionID)
	// Only advance the state machine for the main frame; subframe events arrive
	// on their own sessionId and would trigger navigation_settled prematurely.
	if m.mainSessionID.Load() == sessionID {
		m.computed.onDOMContentLoaded()
	}
}

func (m *Monitor) handleLoadEventFired(p cdpPageLoadEventFiredParams, sessionID string) {
	data, _ := json.Marshal(p)
	m.publishEvent(EventPageLoad, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.loadEventFired", data, sessionID)
	if m.mainSessionID.Load() == sessionID {
		m.computed.onPageLoad()
		m.tryScreenshot(m.getLifecycleCtx())
	}
}

// handleAttachedToTarget stores the new session then enables domains and injects script.
// The outer message sessionID (root session) is unused; the child session we
// attached to is in p.SessionID.
func (m *Monitor) handleAttachedToTarget(p cdpTargetAttachedToTargetParams) {
	m.sessionsMu.Lock()
	m.sessions[p.SessionID] = targetInfo{
		targetID:   p.TargetInfo.TargetID,
		url:        p.TargetInfo.URL,
		targetType: p.TargetInfo.Type,
	}
	m.sessionsMu.Unlock()

	targetType := p.TargetInfo.Type
	// Async to avoid blocking the readLoop.
	m.asyncWg.Go(func() {
		ctx := m.getLifecycleCtx()
		m.enableDomains(ctx, p.SessionID, targetType)
		if isPageLikeTarget(targetType) {
			_ = m.injectScript(ctx, p.SessionID)
		}
	})
}

func (m *Monitor) handleTargetCreated(p cdpTargetTargetCreatedParams, sessionID string) {
	data, _ := json.Marshal(map[string]any{
		"target_id":   p.TargetInfo.TargetID,
		"target_type": p.TargetInfo.Type,
		"url":         p.TargetInfo.URL,
	})
	m.publishEvent(EventTargetCreated, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Target.targetCreated", data, sessionID)
}

func (m *Monitor) handleTargetDestroyed(p cdpTargetTargetDestroyedParams, sessionID string) {
	data, _ := json.Marshal(map[string]any{
		"target_id": p.TargetID,
	})
	m.publishEvent(EventTargetDestroyed, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Target.targetDestroyed", data, sessionID)
}

func (m *Monitor) handleDetachedFromTarget(p cdpTargetDetachedFromTargetParams) {
	if p.SessionID == "" {
		return
	}
	m.sessionsMu.Lock()
	delete(m.sessions, p.SessionID)
	m.sessionsMu.Unlock()
}
