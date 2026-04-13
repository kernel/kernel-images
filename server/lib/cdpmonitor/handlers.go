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

// dispatchEvent routes a CDP event to its handler.
func (m *Monitor) dispatchEvent(msg cdpMessage) {
	switch msg.Method {
	case "Runtime.consoleAPICalled":
		m.handleConsole(msg.Params, msg.SessionID)
	case "Runtime.exceptionThrown":
		m.handleExceptionThrown(msg.Params, msg.SessionID)
	case "Runtime.bindingCalled":
		m.handleBindingCalled(msg.Params, msg.SessionID)
	case "Network.requestWillBeSent":
		m.handleNetworkRequest(msg.Params, msg.SessionID)
	case "Network.responseReceived":
		m.handleResponseReceived(msg.Params, msg.SessionID)
	case "Network.loadingFinished":
		m.handleLoadingFinished(msg.Params, msg.SessionID)
	case "Network.loadingFailed":
		m.handleLoadingFailed(msg.Params, msg.SessionID)
	case "Page.frameNavigated":
		m.handleFrameNavigated(msg.Params, msg.SessionID)
	case "Page.domContentEventFired":
		m.handleDOMContentLoaded(msg.Params, msg.SessionID)
	case "Page.loadEventFired":
		m.handleLoadEventFired(msg.Params, msg.SessionID)
	case "PerformanceTimeline.timelineEventAdded":
		m.handleTimelineEvent(msg.Params, msg.SessionID)
	case "Target.attachedToTarget":
		m.handleAttachedToTarget(msg)
	case "Target.targetCreated":
		m.handleTargetCreated(msg.Params, msg.SessionID)
	case "Target.targetDestroyed":
		m.handleTargetDestroyed(msg.Params, msg.SessionID)
	case "Target.detachedFromTarget":
		m.handleDetachedFromTarget(msg.Params)
	}
}

func (m *Monitor) handleConsole(params json.RawMessage, sessionID string) {
	var p cdpConsoleParams
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Runtime.consoleAPICalled", err)
		return
	}

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
	m.publishEvent(EventConsoleLog, events.CategoryConsole, events.Source{Kind: events.KindCDP}, "Runtime.consoleAPICalled", data, sessionID)
}

func (m *Monitor) handleExceptionThrown(params json.RawMessage, sessionID string) {
	var p cdpExceptionDetails
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Runtime.exceptionThrown", err)
		return
	}
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
func (m *Monitor) handleBindingCalled(params json.RawMessage, sessionID string) {
	var p struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Runtime.bindingCalled", err)
		return
	}
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
func (m *Monitor) handleTimelineEvent(params json.RawMessage, sessionID string) {
	var p struct {
		Event struct {
			Type        string          `json:"type"`
			LayoutShift json.RawMessage `json:"layoutShiftDetails,omitempty"`
		} `json:"event"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("PerformanceTimeline.timelineEventAdded", err)
		return
	}
	if p.Event.Type != timelineEventLayoutShift {
		return
	}
	m.publishEvent(EventLayoutShift, events.CategoryPage, events.Source{Kind: events.KindCDP}, "PerformanceTimeline.timelineEventAdded", params, sessionID)
	m.computed.onLayoutShift()
}

// handleNetworkRequest publishes network_request events.
// NOTE: events include raw headers, post_data, and (on response) truncated
// bodies which may contain cookies, bearer tokens, or other credentials.
// This mirrors what CDP/DevTools itself exposes. Consumers should treat the
// event stream as privileged data; opt-in redaction can be added later.
func (m *Monitor) handleNetworkRequest(params json.RawMessage, sessionID string) {
	var p cdpNetworkRequestParams
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Network.requestWillBeSent", err)
		return
	}
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
		resourceType: p.ResourceType,
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
	if p.ResourceType != "" {
		ev["resource_type"] = p.ResourceType
	}
	data, _ := json.Marshal(ev)
	m.publishEvent(EventNetworkRequest, events.CategoryNetwork, events.Source{Kind: events.KindCDP}, "Network.requestWillBeSent", data, sessionID)
	if !isRedirect {
		m.computed.onRequest()
	}
}

func (m *Monitor) handleResponseReceived(params json.RawMessage, _ string) {
	var p cdpResponseReceivedParams
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Network.responseReceived", err)
		return
	}
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

func (m *Monitor) handleLoadingFinished(params json.RawMessage, sessionID string) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Network.loadingFinished", err)
		return
	}
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

func (m *Monitor) handleLoadingFailed(params json.RawMessage, sessionID string) {
	var p struct {
		RequestID string `json:"requestId"`
		ErrorText string `json:"errorText"`
		Canceled  bool   `json:"canceled"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Network.loadingFailed", err)
		return
	}
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

func (m *Monitor) handleFrameNavigated(params json.RawMessage, sessionID string) {
	var p struct {
		Frame struct {
			ID       string `json:"id"`
			ParentID string `json:"parentId"`
			URL      string `json:"url"`
		} `json:"frame"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Page.frameNavigated", err)
		return
	}
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

func (m *Monitor) handleDOMContentLoaded(params json.RawMessage, sessionID string) {
	m.publishEvent(EventDOMContentLoaded, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.domContentEventFired", params, sessionID)
	// Only advance the state machine for the main frame; subframe events arrive
	// on their own sessionId and would trigger navigation_settled prematurely.
	if m.mainSessionID.Load() == sessionID {
		m.computed.onDOMContentLoaded()
	}
}

func (m *Monitor) handleLoadEventFired(params json.RawMessage, sessionID string) {
	m.publishEvent(EventPageLoad, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.loadEventFired", params, sessionID)
	if m.mainSessionID.Load() == sessionID {
		m.computed.onPageLoad()
		m.tryScreenshot(m.getLifecycleCtx())
	}
}

// handleAttachedToTarget stores the new session then enables domains and injects script.
func (m *Monitor) handleAttachedToTarget(msg cdpMessage) {
	var params cdpAttachedToTargetParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		m.logUnmarshalErr("Target.attachedToTarget", err)
		return
	}
	m.sessionsMu.Lock()
	m.sessions[params.SessionID] = targetInfo{
		targetID:   params.TargetInfo.TargetID,
		url:        params.TargetInfo.URL,
		targetType: params.TargetInfo.Type,
	}
	m.sessionsMu.Unlock()

	targetType := params.TargetInfo.Type
	// Async to avoid blocking the readLoop.
	m.asyncWg.Go(func() {
		ctx := m.getLifecycleCtx()
		m.enableDomains(ctx, params.SessionID, targetType)
		if isPageLikeTarget(targetType) {
			_ = m.injectScript(ctx, params.SessionID)
		}
	})
}

func (m *Monitor) handleTargetCreated(params json.RawMessage, sessionID string) {
	var p cdpTargetCreatedParams
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Target.targetCreated", err)
		return
	}
	data, _ := json.Marshal(map[string]any{
		"target_id":   p.TargetInfo.TargetID,
		"target_type": p.TargetInfo.Type,
		"url":         p.TargetInfo.URL,
	})
	m.publishEvent(EventTargetCreated, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Target.targetCreated", data, sessionID)
}

func (m *Monitor) handleTargetDestroyed(params json.RawMessage, sessionID string) {
	var p struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Target.targetDestroyed", err)
		return
	}
	data, _ := json.Marshal(map[string]any{
		"target_id": p.TargetID,
	})
	m.publishEvent(EventTargetDestroyed, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Target.targetDestroyed", data, sessionID)
}

func (m *Monitor) handleDetachedFromTarget(params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		m.logUnmarshalErr("Target.detachedFromTarget", err)
		return
	}
	if p.SessionID == "" {
		return
	}
	m.sessionsMu.Lock()
	delete(m.sessions, p.SessionID)
	m.sessionsMu.Unlock()
}
