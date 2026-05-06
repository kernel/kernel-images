package cdpmonitor

import (
	"context"
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
		m.sessionsMu.RLock()
		info := m.sessions[sessionID]
		m.sessionsMu.RUnlock()
		src.Metadata[MetadataKeyTargetID] = info.targetID
		src.Metadata[MetadataKeyTargetType] = info.targetType
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
	m.lifeMu.Lock()
	ctx := m.lifecycleCtx
	m.lifeMu.Unlock()

	switch msg.Method {
	case "Runtime.consoleAPICalled":
		var p cdpRuntimeConsoleAPICalledParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleConsole(p, msg.SessionID)
		}
	case "Runtime.exceptionThrown":
		var p cdpRuntimeExceptionThrownParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleExceptionThrown(ctx, p, msg.SessionID)
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
			m.handleLoadingFinished(ctx, p, msg.SessionID)
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
			m.handleLoadEventFired(ctx, p, msg.SessionID)
		}
	case "PerformanceTimeline.timelineEventAdded":
		var p cdpPerformanceTimelineEventAddedParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleTimelineEvent(p, msg.SessionID)
		}
	case "Target.attachedToTarget":
		var p cdpTargetAttachedToTargetParams
		if m.decodeParams(msg.Method, msg.Params, &p) {
			m.handleAttachedToTarget(ctx, p)
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
	eventType := EventConsoleLog
	if p.Type == "error" {
		eventType = EventConsoleError
	}
	cs := m.computedFor(sessionID)
	data := cs.navDataWith(map[string]any{
		"level":       p.Type,
		"text":        text,
		"args":        argValues,
		"stack_trace": p.StackTrace,
	})
	m.publishEvent(eventType, events.CategoryConsole, events.Source{Kind: events.KindCDP}, "Runtime.consoleAPICalled", data, sessionID)
}

func (m *Monitor) handleExceptionThrown(ctx context.Context, p cdpRuntimeExceptionThrownParams, sessionID string) {
	cs := m.computedFor(sessionID)
	// source_url is the script file URL; distinct from nav context's url (the page URL).
	data := cs.navDataWith(map[string]any{
		"text":        p.ExceptionDetails.Text,
		"line":        p.ExceptionDetails.LineNumber,
		"column":      p.ExceptionDetails.ColumnNumber,
		"source_url":  p.ExceptionDetails.URL,
		"stack_trace": p.ExceptionDetails.StackTrace,
	})
	m.publishEvent(EventConsoleError, events.CategoryConsole, events.Source{Kind: events.KindCDP}, "Runtime.exceptionThrown", data, sessionID)
	m.tryScreenshot(ctx, "Runtime.exceptionThrown", sessionID)
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

	var payloadMap map[string]any
	_ = json.Unmarshal(payload, &payloadMap)
	cs := m.computedFor(sessionID)
	m.publishEvent(header.Type, events.CategoryInteraction, events.Source{Kind: events.KindCDP}, "Runtime.bindingCalled", cs.navDataWith(payloadMap), sessionID)
}

// handleTimelineEvent processes PerformanceTimeline layout-shift and LCP events.
func (m *Monitor) handleTimelineEvent(p cdpPerformanceTimelineEventAddedParams, sessionID string) {
	switch p.Event.Type {
	case timelineEventLayoutShift:
		// source_frame_id is the frame where the shift occurred; distinct from nav
		// context's frame_id (the top-level navigated frame).
		ev := map[string]any{
			"source_frame_id": p.Event.FrameID,
			"time":            p.Event.Time,
			"duration":        p.Event.Duration,
		}
		var shift cdpLayoutShiftDetails
		if p.Event.LayoutShiftDetails != nil && json.Unmarshal(p.Event.LayoutShiftDetails, &shift) == nil {
			ev["layout_shift_details"] = map[string]any{
				"value":            shift.Value,
				"had_recent_input": shift.HadRecentInput,
			}
		}
		cs := m.computedFor(sessionID)
		data := cs.navDataWith(ev)
		m.publishEvent(EventLayoutShift, events.CategoryPage, events.Source{Kind: events.KindCDP}, "PerformanceTimeline.timelineEventAdded", data, sessionID)
		if cs != nil {
			cs.onLayoutShift()
		}

	case timelineEventLCP:
		ev := map[string]any{
			"source_frame_id": p.Event.FrameID,
			"time":            p.Event.Time,
		}
		var lcp cdpLcpDetails
		if p.Event.LcpDetails != nil && json.Unmarshal(p.Event.LcpDetails, &lcp) == nil {
			ev["lcp_details"] = map[string]any{
				"render_time": lcp.RenderTime,
				"load_time":   lcp.LoadTime,
				"size":        lcp.Size,
				"element_id":  lcp.ElementID,
				"url":         lcp.URL,
				"node_id":     lcp.NodeID,
			}
		}
		cs := m.computedFor(sessionID)
		data := cs.navDataWith(ev)
		m.publishEvent(EventLCP, events.CategoryPage, events.Source{Kind: events.KindCDP}, "PerformanceTimeline.timelineEventAdded", data, sessionID)
	}
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
		loaderID:     p.LoaderID,
		frameID:      p.FrameID,
		addedAt:      addedAt,
	}
	m.pendReqMu.Unlock()
	ev := map[string]any{
		"request_id":     p.RequestID,
		"loader_id":      p.LoaderID,
		"frame_id":       p.FrameID,
		"document_url":   p.DocumentURL,
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
	if isRedirect {
		ev["is_redirect"] = true
		ev["redirect_url"] = existing.url
	}
	data, _ := json.Marshal(ev)
	m.publishEvent(EventNetworkRequest, events.CategoryNetwork, events.Source{Kind: events.KindCDP}, "Network.requestWillBeSent", data, sessionID)
	if !isRedirect {
		if cs := m.computedFor(sessionID); cs != nil {
			cs.onRequest()
		}
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

func (m *Monitor) handleLoadingFinished(ctx context.Context, p cdpNetworkLoadingFinishedParams, sessionID string) {
	m.pendReqMu.Lock()
	state, ok := m.pendingRequests[p.RequestID]
	if ok {
		delete(m.pendingRequests, p.RequestID)
	}
	m.pendReqMu.Unlock()
	if !ok {
		return
	}
	if cs := m.computedFor(state.sessionID); cs != nil {
		cs.onLoadingFinished()
	}
	// Fetch response body async to avoid blocking readLoop; binary types are skipped.
	m.asyncWg.Go(func() {
		body := m.fetchResponseBody(ctx, p.RequestID, sessionID, state)
		ev := map[string]any{
			"request_id": p.RequestID,
			"loader_id":  state.loaderID,
			"frame_id":   state.frameID,
			"method":     state.method,
			"url":        state.url,
			"status":     state.status,
			"headers":    state.resHeaders,
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
func (m *Monitor) fetchResponseBody(ctx context.Context, requestID, sessionID string, state networkReqState) string {
	if !isTextualResource(state.resourceType, state.mimeType) {
		return ""
	}
	result, err := m.send(ctx, "Network.getResponseBody", map[string]any{
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
		"request_id": p.RequestID,
		"error_text": p.ErrorText,
		"canceled":   p.Canceled,
	}
	if ok {
		ev["url"] = state.url
		ev["loader_id"] = state.loaderID
		ev["frame_id"] = state.frameID
		ev["resource_type"] = state.resourceType
	}
	data, _ := json.Marshal(ev)
	m.publishEvent(EventNetworkLoadingFailed, events.CategoryNetwork, events.Source{Kind: events.KindCDP}, "Network.loadingFailed", data, sessionID)
	if ok {
		if cs := m.computedFor(state.sessionID); cs != nil {
			cs.onLoadingFinished()
		}
	}
}

func (m *Monitor) handleFrameNavigated(p cdpPageFrameNavigatedParams, sessionID string) {
	// Pre-fetch target info and computedState before acquiring pendReqMu to
	// avoid a pendReqMu → sessionsMu ordering cycle.
	m.sessionsMu.RLock()
	info := m.sessions[sessionID]
	cs := m.computedStates[sessionID]
	m.sessionsMu.RUnlock()

	data, _ := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"target_id":       info.targetID,
		"target_type":     info.targetType,
		"url":             p.Frame.URL,
		"frame_id":        p.Frame.ID,
		"parent_frame_id": p.Frame.ParentID,
		"loader_id":       p.Frame.LoaderID,
	})
	m.publishEvent(EventNavigation, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.frameNavigated", data, sessionID)

	// Only reset state for top-level navigations; subframe (iframe) navigations
	// should not disrupt main-page tracking.
	if p.Frame.ParentID == "" {
		m.mainSessionID.Store(sessionID)

		navCtx := navContext{
			sessionID:  sessionID,
			targetID:   info.targetID,
			targetType: info.targetType,
			frameID:    p.Frame.ID,
			loaderID:   p.Frame.LoaderID,
			url:        p.Frame.URL,
		}

		m.pendReqMu.Lock()
		for id, req := range m.pendingRequests {
			if req.sessionID == sessionID {
				delete(m.pendingRequests, id)
			}
		}
		// Reset while holding pendReqMu so new requests arriving concurrently
		// can't increment netPending before the reset completes.
		// inflight=0: remaining pendingRequests belong to other target sessions;
		// their loadingFinished events decrement those sessions' own state machines,
		// not this one, so we start fresh.
		if cs != nil {
			if err := cs.resetOnNavigation(0, navCtx); err != nil {
				m.log.Error("cdpmonitor: failed to build nav event payload", "err", err)
			}
		}
		m.pendReqMu.Unlock()
	}
}

func (m *Monitor) handleDOMContentLoaded(p cdpPageDomContentEventFiredParams, sessionID string) {
	cs := m.computedFor(sessionID)
	data := cs.navDataWith(map[string]any{"cdp_timestamp": p.Timestamp})
	m.publishEvent(EventDOMContentLoaded, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.domContentEventFired", data, sessionID)
	if cs != nil {
		cs.onDOMContentLoaded()
	}
}

func (m *Monitor) handleLoadEventFired(ctx context.Context, p cdpPageLoadEventFiredParams, sessionID string) {
	cs := m.computedFor(sessionID)
	data := cs.navDataWith(map[string]any{"cdp_timestamp": p.Timestamp})
	m.publishEvent(EventPageLoad, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Page.loadEventFired", data, sessionID)
	if cs != nil {
		cs.onPageLoad()
	}
	if m.mainSessionID.Load() == sessionID {
		m.tryScreenshot(ctx, "Page.loadEventFired", sessionID)
	}
}

// handleAttachedToTarget stores the new session then enables domains and injects script.
// The outer message sessionID (root session) is unused; the child session we
// attached to is in p.SessionID.
func (m *Monitor) handleAttachedToTarget(ctx context.Context, p cdpTargetAttachedToTargetParams) {
	m.sessionsMu.Lock()
	m.sessions[p.SessionID] = targetInfo{
		targetID:   p.TargetInfo.TargetID,
		url:        p.TargetInfo.URL,
		targetType: p.TargetInfo.Type,
	}
	if p.TargetInfo.Type == targetTypePage {
		m.computedStates[p.SessionID] = newComputedState(m.publish)
	}
	m.sessionsMu.Unlock()

	if p.TargetInfo.Type == targetTypePage {
		data, _ := json.Marshal(map[string]any{
			"target_id":   p.TargetInfo.TargetID,
			"target_type": p.TargetInfo.Type,
			"url":         p.TargetInfo.URL,
			"opener_id":   p.TargetInfo.OpenerID,
			"title":       p.TargetInfo.Title,
		})
		m.publishEvent(EventTabOpened, events.CategoryPage, events.Source{Kind: events.KindCDP}, "Target.attachedToTarget", data, p.SessionID)
	}

	targetType := p.TargetInfo.Type
	// Async to avoid blocking the readLoop.
	m.asyncWg.Go(func() {
		m.enableDomains(ctx, p.SessionID, targetType)
		if isPageLikeTarget(targetType) {
			_ = m.injectScript(ctx, p.SessionID)
		}
	})
}

func (m *Monitor) handleDetachedFromTarget(p cdpTargetDetachedFromTargetParams) {
	if p.SessionID == "" {
		return
	}
	m.sessionsMu.Lock()
	cs := m.computedStates[p.SessionID]
	delete(m.sessions, p.SessionID)
	delete(m.computedStates, p.SessionID)
	m.sessionsMu.Unlock()
	if cs != nil {
		cs.stop()
	}
}
