package cdpmonitor

import (
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/onkernel/kernel-images/server/lib/events"
)

// publishEvent stamps common fields and publishes an Event.
func (m *Monitor) publishEvent(eventType string, source events.Source, sourceEvent string, data json.RawMessage, sessionID string) {
	src := source
	src.Event = sourceEvent
	if sessionID != "" {
		if src.Metadata == nil {
			src.Metadata = make(map[string]string)
		}
		src.Metadata["cdp_session_id"] = sessionID
	}
	m.publish(events.Event{
		Ts:          time.Now().UnixMilli(),
		Type:        eventType,
		Category:    events.CategoryFor(eventType),
		Source:      src,
		DetailLevel: events.DetailStandard,
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
	case "DOM.documentUpdated":
		m.handleDOMUpdated(msg.Params, msg.SessionID)
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
		text = p.Args[0].Value
	}
	argValues := make([]string, 0, len(p.Args))
	for _, a := range p.Args {
		argValues = append(argValues, a.Value)
	}
	data, _ := json.Marshal(map[string]any{
		"level":       p.Type,
		"text":        text,
		"args":        argValues,
		"stack_trace": p.StackTrace,
	})
	m.publishEvent("console_log", events.Source{Kind: events.KindCDP}, "Runtime.consoleAPICalled", data, sessionID)
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
	m.publishEvent("console_error", events.Source{Kind: events.KindCDP}, "Runtime.exceptionThrown", data, sessionID)
	go m.maybeScreenshot(m.lifecycleCtx)
}

// handleBindingCalled processes __kernelEvent binding calls.
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
	case "interaction_click", "interaction_key", "scroll_settled":
		m.publishEvent(header.Type, events.Source{Kind: events.KindCDP}, "Runtime.bindingCalled", payload, sessionID)
	}
}

// handleTimelineEvent processes layout-shift events from PerformanceTimeline.
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
	m.publishEvent("layout_shift", events.Source{Kind: events.KindCDP}, "PerformanceTimeline.timelineEventAdded", params, sessionID)
	m.computed.onLayoutShift()
}

func (m *Monitor) handleNetworkRequest(params json.RawMessage, sessionID string) {
	var p cdpNetworkRequestParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m.pendReqMu.Lock()
	m.pendingRequests[p.RequestID] = networkReqState{
		method:       p.Request.Method,
		url:          p.Request.URL,
		headers:      p.Request.Headers,
		postData:     p.Request.PostData,
		resourceType: p.ResourceType,
		initiator:    p.Initiator,
	}
	m.pendReqMu.Unlock()
	data, _ := json.Marshal(map[string]any{
		"method":        p.Request.Method,
		"url":           p.Request.URL,
		"headers":       p.Request.Headers,
		"post_data":     p.Request.PostData,
		"resource_type": p.ResourceType,
		"initiator":     p.Initiator,
	})
	m.publishEvent("network_request", events.Source{Kind: events.KindCDP}, "Network.requestWillBeSent", data, sessionID)
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
	// Fetch response body async to avoid blocking readLoop.
	go func() {
		ctx := m.lifecycleCtx
		body := ""
		result, err := m.send(ctx, "Network.getResponseBody", map[string]any{
			"requestId": p.RequestID,
		}, sessionID)
		if err == nil {
			var resp struct {
				Body         string `json:"body"`
				Base64Encoded bool   `json:"base64Encoded"`
			}
			if json.Unmarshal(result, &resp) == nil {
				body = truncateBody(resp.Body)
			}
		}
		data, _ := json.Marshal(map[string]any{
			"method":      state.method,
			"url":         state.url,
			"status":      state.status,
			"status_text": state.statusText,
			"headers":     state.resHeaders,
			"mime_type":   state.mimeType,
			"body":        body,
		})
		m.publishEvent("network_response", events.Source{Kind: events.KindCDP}, "Network.loadingFinished", data, sessionID)
		m.computed.onLoadingFinished()
	}()
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
	m.publishEvent("network_loading_failed", events.Source{Kind: events.KindCDP}, "Network.loadingFailed", data, sessionID)
	m.computed.onLoadingFinished()
}

// truncateBody caps body at ~900KB on a valid UTF-8 boundary.
func truncateBody(body string) string {
	const maxBody = 900 * 1024
	if len(body) <= maxBody {
		return body
	}
	// Back up to a valid rune boundary.
	truncated := body[:maxBody]
	for !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
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
	m.publishEvent("navigation", events.Source{Kind: events.KindCDP}, "Page.frameNavigated", data, sessionID)

	m.pendReqMu.Lock()
	clear(m.pendingRequests)
	m.pendReqMu.Unlock()

	m.computed.resetOnNavigation()
}

func (m *Monitor) handleDOMContentLoaded(params json.RawMessage, sessionID string) {
	m.publishEvent("dom_content_loaded", events.Source{Kind: events.KindCDP}, "Page.domContentEventFired", params, sessionID)
	m.computed.onDOMContentLoaded()
}

func (m *Monitor) handleLoadEventFired(params json.RawMessage, sessionID string) {
	m.publishEvent("page_load", events.Source{Kind: events.KindCDP}, "Page.loadEventFired", params, sessionID)
	m.computed.onPageLoad()
	go m.maybeScreenshot(m.lifecycleCtx)
}

func (m *Monitor) handleDOMUpdated(params json.RawMessage, sessionID string) {
	m.publishEvent("dom_updated", events.Source{Kind: events.KindCDP}, "DOM.documentUpdated", params, sessionID)
}

// handleAttachedToTarget stores the session and enables domains + injects script.
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

	// Async to avoid blocking readLoop.
	go func() {
		m.enableDomains(m.lifecycleCtx, params.SessionID)
		_ = m.injectScript(m.lifecycleCtx, params.SessionID)
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
	m.publishEvent("target_created", events.Source{Kind: events.KindCDP}, "Target.targetCreated", data, sessionID)
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
	m.publishEvent("target_destroyed", events.Source{Kind: events.KindCDP}, "Target.targetDestroyed", data, sessionID)
}
