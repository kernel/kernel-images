package cdpmonitor

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
)

// networkReqState holds request + response metadata for the
// getResponseBody-after-loadingFinished pattern.
type networkReqState struct {
	method       string
	url          string
	headers      json.RawMessage
	postData     string
	resourceType string
	initiator    json.RawMessage
	// populated on responseReceived
	status     int
	statusText string
	resHeaders json.RawMessage
	mimeType   string
}

const kernelEventPrefix = "[KERNEL_EVENT] "

// cdpConsoleArg is the shape of a single Runtime.consoleAPICalled arg.
type cdpConsoleArg struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// isKernelEvent checks if the first console arg starts with the sentinel prefix.
// Returns the JSON payload after the prefix and true if it is a kernel event.
func isKernelEvent(args []cdpConsoleArg) (json.RawMessage, bool) {
	if len(args) == 0 {
		return nil, false
	}
	// args[0].Value may be a quoted JSON string if type=="string"
	val := args[0].Value
	if !strings.HasPrefix(val, kernelEventPrefix) {
		return nil, false
	}
	payload := strings.TrimPrefix(val, kernelEventPrefix)
	if !json.Valid([]byte(payload)) {
		return nil, false
	}
	return json.RawMessage(payload), true
}

// publishEvent stamps common envelope fields and calls m.publish.
func (m *Monitor) publishEvent(eventType string, sourceKind events.Source, sourceEvent string, data json.RawMessage, sessionID string) {
	m.publish(events.BrowserEvent{
		Ts:           time.Now().UnixMilli(),
		Type:         eventType,
		Category:     events.CategoryFor(eventType),
		Source:       sourceKind,
		SourceEvent:  sourceEvent,
		DetailLevel:  events.DetailDefault,
		CDPSessionID: sessionID,
		Data:         data,
	})
}

// dispatchEvent routes incoming CDP events to the appropriate handler.
func (m *Monitor) dispatchEvent(msg cdpMessage) {
	switch msg.Method {
	case "Runtime.consoleAPICalled":
		m.handleConsole(msg.Params, msg.SessionID)
	case "Runtime.exceptionThrown":
		m.handleExceptionThrown(msg.Params, msg.SessionID)
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
	case "Target.attachedToTarget":
		m.handleAttachedToTarget(msg)
	case "Target.targetCreated":
		m.handleTargetCreated(msg.Params, msg.SessionID)
	case "Target.targetDestroyed":
		m.handleTargetDestroyed(msg.Params, msg.SessionID)
	}
}

// --- Console ---

func (m *Monitor) handleConsole(params json.RawMessage, sessionID string) {
	var p struct {
		Type       string          `json:"type"`
		Args       []cdpConsoleArg `json:"args"`
		StackTrace json.RawMessage `json:"stackTrace"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}

	if payload, ok := isKernelEvent(p.Args); ok {
		m.handleKernelEvent(payload, sessionID)
		return
	}

	// Build console_log data.
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
	m.publishEvent("console_log", events.SourceCDP, "Runtime.consoleAPICalled", data, sessionID)
}

func (m *Monitor) handleExceptionThrown(params json.RawMessage, sessionID string) {
	var p struct {
		ExceptionDetails struct {
			Text         string          `json:"text"`
			LineNumber   int             `json:"lineNumber"`
			ColumnNumber int             `json:"columnNumber"`
			URL          string          `json:"url"`
			StackTrace   json.RawMessage `json:"stackTrace"`
		} `json:"exceptionDetails"`
	}
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
	m.publishEvent("console_error", events.SourceCDP, "Runtime.exceptionThrown", data, sessionID)
	go m.maybeScreenshot(context.Background())
}

func (m *Monitor) handleKernelEvent(payload json.RawMessage, sessionID string) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return
	}
	switch header.Type {
	case "interaction_click", "interaction_key", "interaction_scroll",
		"layout_shift", "scroll_settled":
		m.publishEvent(header.Type, events.SourceCDP, "Runtime.consoleAPICalled", payload, sessionID)
		if header.Type == "layout_shift" {
			m.computed.onLayoutShift()
		}
	}
}

// --- Network ---

func (m *Monitor) handleNetworkRequest(params json.RawMessage, sessionID string) {
	var p struct {
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
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m.pendingRequests.Store(p.RequestID, networkReqState{
		method:       p.Request.Method,
		url:          p.Request.URL,
		headers:      p.Request.Headers,
		postData:     p.Request.PostData,
		resourceType: p.ResourceType,
		initiator:    p.Initiator,
	})
	data, _ := json.Marshal(map[string]any{
		"method":        p.Request.Method,
		"url":           p.Request.URL,
		"headers":       p.Request.Headers,
		"post_data":     p.Request.PostData,
		"resource_type": p.ResourceType,
		"initiator":     p.Initiator,
	})
	m.publishEvent("network_request", events.SourceCDP, "Network.requestWillBeSent", data, sessionID)
	m.computed.onRequest()
}

func (m *Monitor) handleResponseReceived(params json.RawMessage, sessionID string) {
	var p struct {
		RequestID string `json:"requestId"`
		Response  struct {
			Status     int             `json:"status"`
			StatusText string          `json:"statusText"`
			URL        string          `json:"url"`
			Headers    json.RawMessage `json:"headers"`
			MimeType   string          `json:"mimeType"`
		} `json:"response"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if v, ok := m.pendingRequests.Load(p.RequestID); ok {
		state := v.(networkReqState)
		state.status = p.Response.Status
		state.statusText = p.Response.StatusText
		state.resHeaders = p.Response.Headers
		state.mimeType = p.Response.MimeType
		m.pendingRequests.Store(p.RequestID, state)
	}
}

func (m *Monitor) handleLoadingFinished(params json.RawMessage, sessionID string) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	v, ok := m.pendingRequests.LoadAndDelete(p.RequestID)
	if !ok {
		return
	}
	state := v.(networkReqState)
	// Call getResponseBody in a separate goroutine — MUST NOT block the readLoop.
	go func() {
		ctx := context.Background()
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
		m.publishEvent("network_response", events.SourceCDP, "Network.loadingFinished", data, sessionID)
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
	if v, ok := m.pendingRequests.Load(p.RequestID); ok {
		state := v.(networkReqState)
		m.pendingRequests.Delete(p.RequestID)
		data, _ := json.Marshal(map[string]any{
			"url":        state.url,
			"error_text": p.ErrorText,
			"canceled":   p.Canceled,
		})
		m.publishEvent("network_loading_failed", events.SourceCDP, "Network.loadingFailed", data, sessionID)
		m.computed.onLoadingFinished()
		return
	}
	// No stored state — publish with what we have.
	data, _ := json.Marshal(map[string]any{
		"error_text": p.ErrorText,
		"canceled":   p.Canceled,
	})
	m.publishEvent("network_loading_failed", events.SourceCDP, "Network.loadingFailed", data, sessionID)
	m.computed.onLoadingFinished()
}

// truncateBody returns the body truncated to 900KB if it exceeds that.
func truncateBody(body string) string {
	const maxBody = 900 * 1024
	if len(body) > maxBody {
		return body[:maxBody]
	}
	return body
}

// --- Page / Navigation ---

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
	m.publishEvent("navigation", events.SourceCDP, "Page.frameNavigated", data, sessionID)
	m.computed.resetOnNavigation()
}

func (m *Monitor) handleDOMContentLoaded(params json.RawMessage, sessionID string) {
	m.publishEvent("dom_content_loaded", events.SourceCDP, "Page.domContentEventFired", params, sessionID)
	m.computed.onDOMContentLoaded()
}

func (m *Monitor) handleLoadEventFired(params json.RawMessage, sessionID string) {
	m.publishEvent("page_load", events.SourceCDP, "Page.loadEventFired", params, sessionID)
	m.computed.onPageLoad()
	go m.maybeScreenshot(context.Background())
}

func (m *Monitor) handleDOMUpdated(params json.RawMessage, sessionID string) {
	m.publishEvent("dom_updated", events.SourceCDP, "DOM.documentUpdated", params, sessionID)
}

// --- Target ---

// handleAttachedToTarget processes Target.attachedToTarget events.
// It stores the session and (asynchronously) enables domains + injects script.
func (m *Monitor) handleAttachedToTarget(msg cdpMessage) {
	var params struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfo"`
	}
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

	// Enable domains and inject script asynchronously to avoid blocking the readLoop.
	go func() {
		ctx := context.Background()
		m.enableDomains(ctx, params.SessionID)
		_ = m.injectScript(ctx, params.SessionID)
	}()
}

func (m *Monitor) handleTargetCreated(params json.RawMessage, sessionID string) {
	var p struct {
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfo"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{
		"target_id":   p.TargetInfo.TargetID,
		"target_type": p.TargetInfo.Type,
		"url":         p.TargetInfo.URL,
	})
	m.publishEvent("target_created", events.SourceCDP, "Target.targetCreated", data, sessionID)
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
	m.publishEvent("target_destroyed", events.SourceCDP, "Target.targetDestroyed", data, sessionID)
}
