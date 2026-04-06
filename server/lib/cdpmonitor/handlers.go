package cdpmonitor

import (
	"encoding/json"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
)

// publishEvent stamps common fields and publishes an event.
func (m *Monitor) publishEvent(eventType string, detail events.DetailLevel, source events.Source, sourceEvent string, data json.RawMessage, sessionID string) {
	src := source
	src.Event = sourceEvent
	if sessionID != "" {
		if src.Metadata == nil {
			src.Metadata = make(map[string]string)
		}
		src.Metadata["cdp_session_id"] = sessionID
	}
	url, _ := m.currentURL.Load().(string)
	m.publish(events.Event{
		Ts:          time.Now().UnixMilli(),
		Type:        eventType,
		Category:    events.CategoryFor(eventType),
		Source:      src,
		DetailLevel: detail,
		URL:         url,
		Data:        data,
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
	}
}

func (m *Monitor) handleConsole(params json.RawMessage, sessionID string) {
	var p cdpConsoleParams
	if err := json.Unmarshal(params, &p); err != nil {
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
	m.publishEvent(EventConsoleLog, events.DetailStandard, events.Source{Kind: events.KindCDP}, "Runtime.consoleAPICalled", data, sessionID)
}

func (m *Monitor) handleExceptionThrown(params json.RawMessage, sessionID string) {
	var p cdpExceptionDetails
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{
		"text":        p.ExceptionDetails.Text,
		"line":        p.ExceptionDetails.LineNumber,
		"column":      p.ExceptionDetails.ColumnNumber,
		"url":         p.ExceptionDetails.URL,
		"stack_trace": p.ExceptionDetails.StackTrace,
	})
	m.publishEvent(EventConsoleError, events.DetailStandard, events.Source{Kind: events.KindCDP}, "Runtime.exceptionThrown", data, sessionID)
	go m.tryScreenshot(m.getLifecycleCtx())
}

// handleBindingCalled processes __kernelEvent binding calls from the page.
func (m *Monitor) handleBindingCalled(params json.RawMessage, sessionID string) {
	var p struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name != bindingName {
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
		m.publishEvent(header.Type, events.DetailStandard, events.Source{Kind: events.KindCDP}, "Runtime.bindingCalled", payload, sessionID)
	}
}

// handleTimelineEvent processes PerformanceTimeline layout-shift events.
func (m *Monitor) handleTimelineEvent(params json.RawMessage, sessionID string) {
	var p struct {
		Event struct {
			Type        string          `json:"type"`
			LayoutShift json.RawMessage `json:"layoutShiftDetails,omitempty"`
		} `json:"event"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Event.Type != "layout-shift" {
		return
	}
	m.publishEvent(EventLayoutShift, events.DetailStandard, events.Source{Kind: events.KindCDP}, "PerformanceTimeline.timelineEventAdded", params, sessionID)
	m.computed.onLayoutShift()
}

func (m *Monitor) handleNetworkRequest(params json.RawMessage, sessionID string) {
	var p cdpNetworkRequestParams
	if err := json.Unmarshal(params, &p); err != nil {
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

	m.pendReqMu.Lock()
	m.pendingRequests[p.RequestID] = networkReqState{
		sessionID:    sessionID,
		method:       p.Request.Method,
		url:          p.Request.URL,
		headers:      p.Request.Headers,
		postData:     p.Request.PostData,
		resourceType: p.ResourceType,
	}
	m.pendReqMu.Unlock()
	data, _ := json.Marshal(map[string]any{
		"method":           p.Request.Method,
		"url":              p.Request.URL,
		"headers":          p.Request.Headers,
		"post_data":        p.Request.PostData,
		"resource_type":    p.ResourceType,
		"initiator_type":   initiatorType,
	})
	m.publishEvent(EventNetworkRequest, events.DetailStandard, events.Source{Kind: events.KindCDP}, "Network.requestWillBeSent", data, sessionID)
	m.computed.onRequest()
}

func (m *Monitor) handleResponseReceived(params json.RawMessage, sessionID string) {
	var p cdpResponseReceivedParams
	if err := json.Unmarshal(params, &p); err != nil {
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
	go func() {
		body := m.fetchResponseBody(p.RequestID, sessionID, state)
		data, _ := json.Marshal(map[string]any{
			"method":        state.method,
			"url":           state.url,
			"status":        state.status,
			"status_text":   state.statusText,
			"headers":       state.resHeaders,
			"mime_type":     state.mimeType,
			"resource_type": state.resourceType,
			"body":          body,
		})
		detail := events.DetailStandard
		if body != "" {
			detail = events.DetailVerbose
		}
		m.publishEvent(EventNetworkResponse, detail, events.Source{Kind: events.KindCDP}, "Network.loadingFinished", data, sessionID)
	}()
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
	return truncateBody(resp.Body, bodyCapFor(state.mimeType))
}

func (m *Monitor) handleLoadingFailed(params json.RawMessage, sessionID string) {
	var p struct {
		RequestID string `json:"requestId"`
		ErrorText string `json:"errorText"`
		Canceled  bool   `json:"canceled"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
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
	m.publishEvent(EventNetworkLoadingFailed, events.DetailStandard, events.Source{Kind: events.KindCDP}, "Network.loadingFailed", data, sessionID)
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
		return
	}
	data, _ := json.Marshal(map[string]any{
		"url":             p.Frame.URL,
		"frame_id":        p.Frame.ID,
		"parent_frame_id": p.Frame.ParentID,
	})
	// Only track top-level frame navigations (no parent).
	if p.Frame.ParentID == "" {
		m.currentURL.Store(p.Frame.URL)
	}
	m.publishEvent(EventNavigation, events.DetailStandard, events.Source{Kind: events.KindCDP}, "Page.frameNavigated", data, sessionID)

	// Only reset state for top-level navigations; subframe (iframe) navigations
	// should not disrupt main-page tracking.
	if p.Frame.ParentID == "" {
		m.pendReqMu.Lock()
		for id, req := range m.pendingRequests {
			if req.sessionID == sessionID {
				delete(m.pendingRequests, id)
			}
		}
		m.pendReqMu.Unlock()

		m.computed.resetOnNavigation()
	}
}

func (m *Monitor) handleDOMContentLoaded(params json.RawMessage, sessionID string) {
	m.publishEvent(EventDOMContentLoaded, events.DetailMinimal, events.Source{Kind: events.KindCDP}, "Page.domContentEventFired", params, sessionID)
	m.computed.onDOMContentLoaded()
}

func (m *Monitor) handleLoadEventFired(params json.RawMessage, sessionID string) {
	m.publishEvent(EventPageLoad, events.DetailMinimal, events.Source{Kind: events.KindCDP}, "Page.loadEventFired", params, sessionID)
	m.computed.onPageLoad()
	go m.tryScreenshot(m.getLifecycleCtx())
}

// handleAttachedToTarget stores the new session then enables domains and injects script.
func (m *Monitor) handleAttachedToTarget(msg cdpMessage) {
	var params cdpAttachedToTargetParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return
	}
	m.sessionsMu.Lock()
	m.sessions[params.SessionID] = targetInfo{
		targetID:   params.TargetInfo.TargetID,
		url:        params.TargetInfo.URL,
		targetType: params.TargetInfo.Type,
	}
	m.sessionsMu.Unlock()

	// Async to avoid blocking the readLoop.
	go func() {
		m.enableDomains(m.getLifecycleCtx(), params.SessionID)
		_ = m.injectScript(m.getLifecycleCtx(), params.SessionID)
	}()
}

func (m *Monitor) handleTargetCreated(params json.RawMessage, sessionID string) {
	var p cdpTargetCreatedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{
		"target_id":   p.TargetInfo.TargetID,
		"target_type": p.TargetInfo.Type,
		"url":         p.TargetInfo.URL,
	})
	m.publishEvent(EventTargetCreated, events.DetailMinimal, events.Source{Kind: events.KindCDP}, "Target.targetCreated", data, sessionID)
}

func (m *Monitor) handleTargetDestroyed(params json.RawMessage, sessionID string) {
	var p struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{
		"target_id": p.TargetID,
	})
	m.publishEvent(EventTargetDestroyed, events.DetailMinimal, events.Source{Kind: events.KindCDP}, "Target.targetDestroyed", data, sessionID)
}
