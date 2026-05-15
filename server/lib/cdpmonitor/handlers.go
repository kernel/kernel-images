package cdpmonitor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// logUnmarshalErr logs a Debug message when a handler can't parse CDP params.
// These indicate Chrome sent an unexpected params shape, rare and non-actionable
// at Warn/Error level, but useful in verbose mode.
func (m *Monitor) logUnmarshalErr(method string, err error) {
	m.log.Debug("cdpmonitor: failed to parse CDP params", "method", method, "err", err)
}

// publishEvent stamps common fields and publishes an event.
func (m *Monitor) publishEvent(eventType string, category oapi.TelemetryEventCategory, source oapi.BrowserEventSource, sourceEvent string, data json.RawMessage, sessionID string) {
	src := source
	src.Event = &sourceEvent
	if sessionID != "" {
		meta := make(map[string]string)
		if src.Metadata != nil {
			for k, v := range *src.Metadata {
				meta[k] = v
			}
		}
		meta[MetadataKeyCDPSessionID] = sessionID
		m.sessionsMu.RLock()
		info := m.sessions[sessionID]
		m.sessionsMu.RUnlock()
		meta[MetadataKeyTargetID] = info.targetID
		meta[MetadataKeyTargetType] = info.targetType
		src.Metadata = &meta
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
	sid, tid, ttype, fid, lid, url, nseq := cs.currentNavCtxFields()
	var stackTrace *oapi.BrowserCallStack
	if len(p.StackTrace) > 0 {
		stackTrace = &oapi.BrowserCallStack{}
		_ = json.Unmarshal(p.StackTrace, stackTrace)
	}
	data, _ := json.Marshal(oapi.BrowserConsoleLogEventData{
		SessionId:  sid,
		TargetId:   tid,
		TargetType: oapi.BrowserTargetType(ttype),
		FrameId:    ptrOf(fid),
		LoaderId:   ptrOf(lid),
		Url:        ptrOf(url),
		NavSeq:     nseq,
		Level:      p.Type,
		Text:       text,
		Args:       ptrOf(argValues),
		StackTrace: stackTrace,
	})
	m.publishEvent(eventType, events.Console, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Runtime.consoleAPICalled", data, sessionID)
}

func (m *Monitor) handleExceptionThrown(ctx context.Context, p cdpRuntimeExceptionThrownParams, sessionID string) {
	cs := m.computedFor(sessionID)
	sid, tid, ttype, fid, lid, url, nseq := cs.currentNavCtxFields()
	var stackTrace *oapi.BrowserCallStack
	if len(p.ExceptionDetails.StackTrace) > 0 {
		stackTrace = &oapi.BrowserCallStack{}
		_ = json.Unmarshal(p.ExceptionDetails.StackTrace, stackTrace)
	}
	// source_url is the script file URL; distinct from nav context's url (the page URL).
	data, _ := json.Marshal(oapi.BrowserConsoleErrorEventData{
		SessionId:  sid,
		TargetId:   tid,
		TargetType: oapi.BrowserTargetType(ttype),
		FrameId:    ptrOf(fid),
		LoaderId:   ptrOf(lid),
		Url:        ptrOf(url),
		NavSeq:     nseq,
		Text:       p.ExceptionDetails.Text,
		Line:       ptrOf(p.ExceptionDetails.LineNumber),
		Column:     ptrOf(p.ExceptionDetails.ColumnNumber),
		SourceUrl:  ptrOf(p.ExceptionDetails.URL),
		StackTrace: stackTrace,
	})
	m.publishEvent(EventConsoleError, events.Console, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Runtime.exceptionThrown", data, sessionID)
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
	m.publishEvent(header.Type, events.Interaction, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Runtime.bindingCalled", cs.navDataWith(payloadMap), sessionID)
}

// handleTimelineEvent processes PerformanceTimeline layout-shift and LCP events.
func (m *Monitor) handleTimelineEvent(p cdpPerformanceTimelineEventAddedParams, sessionID string) {
	switch p.Event.Type {
	case timelineEventLayoutShift:
		// source_frame_id is the frame where the shift occurred; distinct from nav
		// context's frame_id (the top-level navigated frame).
		cs := m.computedFor(sessionID)
		sid, tid, ttype, fid, lid, url, nseq := cs.currentNavCtxFields()
		payload := oapi.BrowserPageLayoutShiftEventData{
			SessionId:     sid,
			TargetId:      tid,
			TargetType:    oapi.BrowserTargetType(ttype),
			FrameId:       ptrOf(fid),
			LoaderId:      ptrOf(lid),
			Url:           ptrOf(url),
			NavSeq:        nseq,
			SourceFrameId: p.Event.FrameID,
			Time:          float32(p.Event.Time),
			Duration:      float32(p.Event.Duration),
		}
		var shift cdpLayoutShiftDetails
		if p.Event.LayoutShiftDetails != nil && json.Unmarshal(p.Event.LayoutShiftDetails, &shift) == nil {
			payload.LayoutShiftDetails = &struct {
				HadRecentInput *bool    `json:"had_recent_input,omitempty"`
				Value          *float32 `json:"value,omitempty"`
			}{
				Value:          ptrOf(float32(shift.Value)),
				HadRecentInput: ptrOf(shift.HadRecentInput),
			}
		}
		data, _ := json.Marshal(payload)
		m.publishEvent(EventLayoutShift, events.Page, oapi.BrowserEventSource{Kind: oapi.Cdp}, "PerformanceTimeline.timelineEventAdded", data, sessionID)
		if cs != nil {
			cs.onLayoutShift()
		}

	case timelineEventLCP:
		cs := m.computedFor(sessionID)
		sid, tid, ttype, fid, lid, url, nseq := cs.currentNavCtxFields()
		lcpPayload := oapi.BrowserPageLcpEventData{
			SessionId:     sid,
			TargetId:      tid,
			TargetType:    oapi.BrowserTargetType(ttype),
			FrameId:       ptrOf(fid),
			LoaderId:      ptrOf(lid),
			Url:           ptrOf(url),
			NavSeq:        nseq,
			SourceFrameId: p.Event.FrameID,
			Time:          float32(p.Event.Time),
		}
		var lcp cdpLcpDetails
		if p.Event.LcpDetails != nil && json.Unmarshal(p.Event.LcpDetails, &lcp) == nil {
			lcpPayload.LcpDetails = &struct {
				ElementId  *string  `json:"element_id,omitempty"`
				LoadTime   *float32 `json:"load_time,omitempty"`
				NodeId     *int     `json:"node_id,omitempty"`
				RenderTime *float32 `json:"render_time,omitempty"`
				Size       *float32 `json:"size,omitempty"`
				Url        *string  `json:"url,omitempty"`
			}{
				RenderTime: ptrOf(float32(lcp.RenderTime)),
				LoadTime:   ptrOf(float32(lcp.LoadTime)),
				Size:       ptrOf(float32(lcp.Size)),
				ElementId:  ptrOf(lcp.ElementID),
				Url:        ptrOf(lcp.URL),
				NodeId:     ptrOf(lcp.NodeID),
			}
		}
		data, _ := json.Marshal(lcpPayload)
		m.publishEvent(EventLCP, events.Page, oapi.BrowserEventSource{Kind: oapi.Cdp}, "PerformanceTimeline.timelineEventAdded", data, sessionID)
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

	m.sessionsMu.RLock()
	info := m.sessions[sessionID]
	m.sessionsMu.RUnlock()
	cs := m.computedFor(sessionID)
	var navSeq int64
	if cs != nil {
		navSeq = int64(cs.currentNavSeq())
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
	} else {
		navSeq = existing.navSeq
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
		navSeq:       navSeq,
		addedAt:      addedAt,
	}
	m.pendReqMu.Unlock()
	var hdrs oapi.BrowserHttpHeaders
	_ = json.Unmarshal(p.Request.Headers, &hdrs)
	payload := oapi.BrowserNetworkRequestEventData{
		SessionId:     sessionID,
		TargetId:      info.targetID,
		TargetType:    oapi.BrowserTargetType(info.targetType),
		FrameId:       ptrOf(p.FrameID),
		LoaderId:      ptrOf(p.LoaderID),
		Url:           ptrOf(p.Request.URL),
		NavSeq:        navSeq,
		RequestId:     p.RequestID,
		Method:        p.Request.Method,
		DocumentUrl:   p.DocumentURL,
		Headers:       hdrs,
		InitiatorType: initiatorType,
	}
	if p.Request.PostData != "" {
		payload.PostData = ptrOf(p.Request.PostData)
	}
	if p.Type != "" {
		payload.ResourceType = ptrOf(p.Type)
	}
	if isRedirect {
		payload.IsRedirect = ptrOf(true)
		payload.RedirectUrl = ptrOf(existing.url)
	}
	data, _ := json.Marshal(payload)
	m.publishEvent(EventNetworkRequest, events.Network, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Network.requestWillBeSent", data, sessionID)
	if !isRedirect && cs != nil {
		cs.onRequest()
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
	m.sessionsMu.RLock()
	info := m.sessions[sessionID]
	m.sessionsMu.RUnlock()
	if cs := m.computedFor(state.sessionID); cs != nil {
		cs.onLoadingFinished()
	}
	// Fetch response body async to avoid blocking readLoop; binary types are skipped.
	m.asyncWg.Go(func() {
		body := m.fetchResponseBody(ctx, p.RequestID, sessionID, state)
		var hdrs oapi.BrowserHttpHeaders
		_ = json.Unmarshal(state.resHeaders, &hdrs)
		resPayload := oapi.BrowserNetworkResponseEventData{
			SessionId:  sessionID,
			TargetId:   info.targetID,
			TargetType: oapi.BrowserTargetType(info.targetType),
			FrameId:    ptrOf(state.frameID),
			LoaderId:   ptrOf(state.loaderID),
			Url:        ptrOf(state.url),
			NavSeq:     state.navSeq,
			RequestId:  p.RequestID,
			Method:     state.method,
			Status:     state.status,
			Headers:    hdrs,
		}
		if state.statusText != "" {
			resPayload.StatusText = ptrOf(state.statusText)
		}
		if state.mimeType != "" {
			resPayload.MimeType = ptrOf(state.mimeType)
		}
		if state.resourceType != "" {
			resPayload.ResourceType = ptrOf(state.resourceType)
		}
		if body != "" {
			resPayload.Body = ptrOf(body)
		}
		data, _ := json.Marshal(resPayload)
		m.publishEvent(EventNetworkResponse, events.Network, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Network.loadingFinished", data, sessionID)
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

	m.sessionsMu.RLock()
	info := m.sessions[sessionID]
	m.sessionsMu.RUnlock()
	// Prefer the navSeq captured at requestWillBeSent time so request/failure
	// pairs share an epoch. For untracked requests (in flight at CDP attach),
	// fall back to the current navSeq.
	var nseq int64
	if ok {
		nseq = state.navSeq
	} else if cs := m.computedFor(sessionID); cs != nil {
		nseq = int64(cs.currentNavSeq())
	}
	failPayload := oapi.BrowserNetworkLoadingFailedEventData{
		SessionId:  sessionID,
		TargetId:   info.targetID,
		TargetType: oapi.BrowserTargetType(info.targetType),
		NavSeq:     nseq,
		RequestId:  p.RequestID,
		ErrorText:  p.ErrorText,
		Canceled:   p.Canceled,
	}
	if ok {
		failPayload.Url = ptrOf(state.url)
		failPayload.LoaderId = ptrOf(state.loaderID)
		failPayload.FrameId = ptrOf(state.frameID)
		if state.resourceType != "" {
			failPayload.ResourceType = ptrOf(state.resourceType)
		}
	}
	data, _ := json.Marshal(failPayload)
	m.publishEvent(EventNetworkLoadingFailed, events.Network, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Network.loadingFailed", data, sessionID)
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

	data, _ := json.Marshal(oapi.BrowserPageNavigationEventData{
		SessionId:     sessionID,
		TargetId:      info.targetID,
		TargetType:    oapi.BrowserTargetType(info.targetType),
		Url:           p.Frame.URL,
		FrameId:       p.Frame.ID,
		ParentFrameId: ptrOf(p.Frame.ParentID),
		LoaderId:      p.Frame.LoaderID,
	})
	m.publishEvent(EventNavigation, events.Page, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Page.frameNavigated", data, sessionID)

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
	sid, tid, ttype, fid, lid, url, nseq := cs.currentNavCtxFields()
	data, _ := json.Marshal(oapi.BrowserPageDomContentLoadedEventData{
		SessionId:    sid,
		TargetId:     tid,
		TargetType:   oapi.BrowserTargetType(ttype),
		FrameId:      ptrOf(fid),
		LoaderId:     ptrOf(lid),
		Url:          ptrOf(url),
		NavSeq:       nseq,
		CdpTimestamp: float32(p.Timestamp),
	})
	m.publishEvent(EventDOMContentLoaded, events.Page, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Page.domContentEventFired", data, sessionID)
	if cs != nil {
		cs.onDOMContentLoaded()
	}
}

func (m *Monitor) handleLoadEventFired(ctx context.Context, p cdpPageLoadEventFiredParams, sessionID string) {
	cs := m.computedFor(sessionID)
	sid, tid, ttype, fid, lid, url, nseq := cs.currentNavCtxFields()
	data, _ := json.Marshal(oapi.BrowserPageLoadEventData{
		SessionId:    sid,
		TargetId:     tid,
		TargetType:   oapi.BrowserTargetType(ttype),
		FrameId:      ptrOf(fid),
		LoaderId:     ptrOf(lid),
		Url:          ptrOf(url),
		NavSeq:       nseq,
		CdpTimestamp: float32(p.Timestamp),
	})
	m.publishEvent(EventPageLoad, events.Page, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Page.loadEventFired", data, sessionID)
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
		data, _ := json.Marshal(oapi.BrowserPageTabOpenedEventData{
			TargetId:   p.TargetInfo.TargetID,
			TargetType: oapi.BrowserTargetType(p.TargetInfo.Type),
			Url:        p.TargetInfo.URL,
			OpenerId:   ptrOf(p.TargetInfo.OpenerID),
			Title:      ptrOf(p.TargetInfo.Title),
		})
		m.publishEvent(EventTabOpened, events.Page, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Target.attachedToTarget", data, p.SessionID)
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
