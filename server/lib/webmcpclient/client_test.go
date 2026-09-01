package webmcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/require"
)

type staticUpstream struct {
	url string
}

func (u staticUpstream) Current() string { return u.url }

type mutableUpstream struct {
	mu  sync.RWMutex
	url string
}

func (u *mutableUpstream) Current() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.url
}

func (u *mutableUpstream) Set(url string) {
	u.mu.Lock()
	u.url = url
	u.mu.Unlock()
}

type wireRequest struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

type fakeCDP struct {
	server *httptest.Server
	url    string

	mu                      sync.Mutex
	connections             int
	targetAttaches          int
	invocationCount         int
	enabledSessions         map[string]int
	omitResponse            bool
	closeOnInvoke           bool
	detachAfterResponse     bool
	navigateBeforeResult    bool
	navigationDoesNotCommit bool
	invokeResponseDelay     time.Duration
	toolCount               int
	popupOpen               bool
	write                   func(any)
}

func newFakeCDP(t *testing.T, omitResponse bool) *fakeCDP {
	t.Helper()
	fake := &fakeCDP{enabledSessions: make(map[string]int), omitResponse: omitResponse, toolCount: 1}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	fake.url = "ws" + strings.TrimPrefix(fake.server.URL, "http")
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeCDP) emit(value any) {
	f.mu.Lock()
	write := f.write
	f.mu.Unlock()
	if write != nil {
		write(value)
	}
}

func (f *fakeCDP) openPopup() {
	f.mu.Lock()
	f.popupOpen = true
	f.mu.Unlock()
	f.emit(map[string]any{
		"method": "Target.targetCreated",
		"params": map[string]any{"targetInfo": map[string]any{
			"targetId": "popup-target", "type": "page", "title": "Popup", "url": "https://popup.example/",
		}},
	})
}

func (f *fakeCDP) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	f.mu.Lock()
	f.connections++
	f.mu.Unlock()

	var writeMu sync.Mutex
	write := func(value any) {
		payload, err := json.Marshal(value)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.Write(r.Context(), websocket.MessageText, payload)
	}
	f.mu.Lock()
	f.write = write
	f.mu.Unlock()
	for {
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var request wireRequest
		if json.Unmarshal(payload, &request) != nil {
			continue
		}
		respond := func(result any) {
			write(map[string]any{"id": request.ID, "result": result})
		}
		switch request.Method {
		case "Target.setDiscoverTargets":
			respond(map[string]any{})
		case "Target.getTargets":
			targets := []map[string]any{
				{"targetId": "page-target", "type": "page", "title": "Store", "url": "https://merchant.example/"},
				{"targetId": "other-page", "type": "page", "title": "Travel", "url": "https://travel.example/"},
			}
			f.mu.Lock()
			if f.popupOpen {
				targets = append(targets, map[string]any{"targetId": "popup-target", "type": "page", "title": "Popup", "url": "https://popup.example/"})
			}
			f.mu.Unlock()
			respond(map[string]any{"targetInfos": targets})
		case "Browser.getWindowForTarget":
			var params struct {
				TargetID string `json:"targetId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			windowID := 10
			if params.TargetID == "other-page" {
				windowID = 20
			}
			respond(map[string]any{"windowId": windowID})
		case "Target.attachToTarget":
			f.mu.Lock()
			f.targetAttaches++
			f.mu.Unlock()
			var params struct {
				TargetID string `json:"targetId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			sessionID := map[string]string{
				"page-target": "page-session", "iframe-target": "iframe-session", "nested-target": "nested-session",
				"other-page": "other-session", "popup-target": "popup-session",
			}[params.TargetID]
			respond(map[string]any{"sessionId": sessionID})
			switch params.TargetID {
			case "page-target":
				write(map[string]any{
					"method": "Target.targetCreated",
					"params": map[string]any{"targetInfo": map[string]any{
						"targetId": "iframe-target", "type": "iframe", "url": "https://payments.example/element#private-state",
						"parentFrameId": "page-frame",
					}},
				})
			case "iframe-target":
				write(map[string]any{
					"method": "Target.targetCreated",
					"params": map[string]any{"targetInfo": map[string]any{
						"targetId": "nested-target", "type": "iframe", "url": "https://bank.example/challenge",
						"parentFrameId": "iframe-frame",
					}},
				})
			}
		case "Page.enable":
			respond(map[string]any{})
		case "Page.getFrameTree":
			respond(map[string]any{"frameTree": frameTreeForSession(request.SessionID)})
		case "WebMCP.enable":
			f.mu.Lock()
			f.enabledSessions[request.SessionID]++
			toolCount := f.toolCount
			f.mu.Unlock()
			respond(map[string]any{})
			name, frameID := "merchant_tool", "page-frame"
			switch request.SessionID {
			case "iframe-session":
				name, frameID = "payment_tool", "iframe-frame"
			case "nested-session":
				name, frameID = "bank_tool", "nested-frame"
			case "other-session":
				name, frameID = "search_flights", "other-frame"
			case "popup-session":
				name, frameID = "popup_tool", "popup-frame"
			}
			tools := make([]map[string]any, toolCount)
			for i := range tools {
				toolName := name
				if toolCount > 1 {
					toolName = fmt.Sprintf("%s_%d", name, i)
				}
				tools[i] = map[string]any{
					"name": toolName, "description": toolName + " description", "frameId": frameID,
					"inputSchema": map[string]any{"type": "object"},
				}
			}
			write(map[string]any{
				"method": "WebMCP.toolsAdded", "sessionId": request.SessionID,
				"params": map[string]any{"tools": tools},
			})
		case "WebMCP.invokeTool":
			f.mu.Lock()
			f.invocationCount++
			invocationID := fmt.Sprintf("invocation-%d", f.invocationCount)
			closeOnInvoke := f.closeOnInvoke
			responseDelay := f.invokeResponseDelay
			omitResponse := f.omitResponse
			navigateBeforeResult := f.navigateBeforeResult
			navigationDoesNotCommit := f.navigationDoesNotCommit
			detachAfterResponse := f.detachAfterResponse
			f.mu.Unlock()
			if closeOnInvoke {
				conn.CloseNow()
				return
			}
			if responseDelay > 0 {
				time.Sleep(responseDelay)
			}
			respond(map[string]any{"invocationId": invocationID})
			if !omitResponse {
				if navigateBeforeResult {
					write(map[string]any{
						"method": "Page.frameStartedLoading", "sessionId": request.SessionID,
						"params": map[string]any{"frameId": "iframe-frame"},
					})
				}
				output := any(map[string]any{"content": []map[string]any{{"type": "text", "text": request.SessionID}}})
				if navigateBeforeResult {
					output = []any{}
				}
				write(map[string]any{
					"method": "WebMCP.toolResponded", "sessionId": request.SessionID,
					"params": map[string]any{
						"invocationId": invocationID, "status": "Completed", "output": output,
					},
				})
				if !navigateBeforeResult {
					write(map[string]any{
						"method": "Page.frameStartedLoading", "sessionId": request.SessionID,
						"params": map[string]any{"frameId": "iframe-frame"},
					})
				}
				if !navigationDoesNotCommit {
					write(map[string]any{
						"method": "Page.frameNavigated", "sessionId": request.SessionID,
						"params": map[string]any{"frame": map[string]any{
							"id": "iframe-frame", "loaderId": "next-loader", "url": "https://payments.example/success",
						}},
					})
				}
				if detachAfterResponse {
					write(map[string]any{
						"method": "Target.detachedFromTarget",
						"params": map[string]any{"sessionId": request.SessionID},
					})
				}
			}
		default:
			write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
}

func frameTreeForSession(sessionID string) map[string]any {
	switch sessionID {
	case "page-session":
		return map[string]any{
			"frame": map[string]any{"id": "page-frame", "loaderId": "page-loader", "url": "https://merchant.example/"},
		}
	case "iframe-session":
		return map[string]any{
			"frame": map[string]any{"id": "iframe-frame", "parentId": "page-frame", "loaderId": "iframe-loader", "url": "https://payments.example/element#private-state"},
		}
	case "nested-session":
		return map[string]any{"frame": map[string]any{"id": "nested-frame", "parentId": "iframe-frame", "loaderId": "nested-loader", "url": "https://bank.example/challenge"}}
	case "other-session":
		return map[string]any{"frame": map[string]any{"id": "other-frame", "loaderId": "other-loader", "url": "https://travel.example/"}}
	case "popup-session":
		return map[string]any{"frame": map[string]any{"id": "popup-frame", "loaderId": "popup-loader", "url": "https://popup.example/"}}
	default:
		return map[string]any{"frame": map[string]any{}}
	}
}

func TestManagerDiscoversToolsAcrossWindowsTabsAndNestedFrames(t *testing.T) {
	fake := newFakeCDP(t, false)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 4)

	byName := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	require.Equal(t, 1, byName["merchant_tool"].Source.WindowID)
	require.Equal(t, 1, byName["merchant_tool"].Source.TabID)
	require.Nil(t, byName["merchant_tool"].Source.Frame)
	require.Equal(t, "Store", byName["payment_tool"].Source.PageTitle)
	require.Equal(t, "https://merchant.example/", byName["payment_tool"].Source.PageURL)
	require.Equal(t, 1, byName["payment_tool"].Source.Frame.FrameID)
	require.Equal(t, "https://payments.example/element", byName["payment_tool"].Source.Frame.URL)
	require.Equal(t, 2, byName["bank_tool"].Source.Frame.FrameID)
	require.Equal(t, "https://bank.example/challenge", byName["bank_tool"].Source.Frame.URL)
	require.Equal(t, 2, byName["search_flights"].Source.WindowID)
	require.Equal(t, 2, byName["search_flights"].Source.TabID)
	require.Nil(t, byName["search_flights"].Source.Frame)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 1, fake.connections)
	require.Equal(t, 4, fake.targetAttaches)
	for _, sessionID := range []string{"page-session", "iframe-session", "nested-session", "other-session"} {
		require.GreaterOrEqual(t, fake.enabledSessions[sessionID], 1)
	}
}

func TestManagerTracksTabsOpenedAfterDiscovery(t *testing.T) {
	fake := newFakeCDP(t, false)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	_, err := manager.Tools(context.Background())
	require.NoError(t, err)

	fake.openPopup()
	var popup Tool
	require.Eventually(t, func() bool {
		tools, toolsErr := manager.Tools(context.Background())
		if toolsErr != nil {
			return false
		}
		for _, tool := range tools {
			if tool.Name == "popup_tool" {
				popup = tool
				return true
			}
		}
		return false
	}, 3*time.Second, 20*time.Millisecond)
	require.Equal(t, 1, popup.Source.WindowID)
	require.Equal(t, 3, popup.Source.TabID)
}

func TestManagerReusesConnectionAndToolReferences(t *testing.T) {
	fake := newFakeCDP(t, false)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	first, err := manager.Tools(context.Background())
	require.NoError(t, err)
	second, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Equal(t, first, second)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 1, fake.connections)
	require.Equal(t, 4, fake.targetAttaches)
}

func paymentToolRef(t *testing.T, manager *Manager) string {
	t.Helper()
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	for _, tool := range tools {
		if tool.Name == "payment_tool" {
			return tool.Ref
		}
	}
	t.Fatal("payment tool not found")
	return ""
}

func TestInvocationPreservesResponseObservedBeforeFrameNavigation(t *testing.T) {
	fake := newFakeCDP(t, false)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	toolRef := paymentToolRef(t, manager)

	result, err := manager.Invoke(context.Background(), toolRef, map[string]any{"amount": 2900})
	require.NoError(t, err)
	require.Equal(t, "invocation-1", result.InvocationID)
	require.Equal(t, "Completed", result.Status)
	require.Equal(t, "iframe-session", result.Output.(map[string]any)["content"].([]any)[0].(map[string]any)["text"])

	require.Eventually(t, func() bool {
		tools, toolsErr := manager.Tools(context.Background())
		if toolsErr != nil {
			return false
		}
		for _, tool := range tools {
			if tool.Ref == toolRef {
				return false
			}
		}
		return true
	}, time.Second, 10*time.Millisecond)
	_, err = manager.Invoke(context.Background(), toolRef, map[string]any{})
	require.ErrorIs(t, err, ErrToolNotFound)
}

func TestInvocationNavigationBeforeResponseHasUnknownOutcome(t *testing.T) {
	fake := newFakeCDP(t, false)
	fake.navigateBeforeResult = true
	fake.navigationDoesNotCommit = true
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	toolRef := paymentToolRef(t, manager)

	result, err := manager.Invoke(context.Background(), toolRef, map[string]any{})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, "invocation-1", result.InvocationID)

	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	toolStillRegistered := false
	for _, tool := range tools {
		if tool.Ref == toolRef {
			toolStillRegistered = true
			break
		}
	}
	require.True(t, toolStillRegistered)

	fake.mu.Lock()
	fake.navigateBeforeResult = false
	fake.navigationDoesNotCommit = false
	fake.mu.Unlock()
	result, err = manager.Invoke(context.Background(), toolRef, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "Completed", result.Status)
}

func TestInvocationReturnsCompletedResponseBeforeTargetDetach(t *testing.T) {
	fake := newFakeCDP(t, false)
	fake.detachAfterResponse = true
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	result, err := manager.Invoke(context.Background(), paymentToolRef(t, manager), map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "Completed", result.Status)
}

func TestInvocationTimeoutHasUnknownOutcome(t *testing.T) {
	fake := newFakeCDP(t, true)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	toolRef := paymentToolRef(t, manager)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result, err := manager.Invoke(ctx, toolRef, map[string]any{})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, "invocation-1", result.InvocationID)
}

func TestInvokeCancellationBeforeCommandResponseKeepsConnection(t *testing.T) {
	fake := newFakeCDP(t, true)
	fake.invokeResponseDelay = 100 * time.Millisecond
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	toolRef := paymentToolRef(t, manager)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := manager.Invoke(ctx, toolRef, map[string]any{})
	require.ErrorIs(t, err, ErrOutcomeUnknown)

	_, err = manager.Tools(context.Background())
	require.NoError(t, err)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 1, fake.connections)
}

func TestConnectionDeathMidInvokeHasUnknownOutcome(t *testing.T) {
	fake := newFakeCDP(t, false)
	fake.closeOnInvoke = true
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	_, err := manager.Invoke(context.Background(), paymentToolRef(t, manager), map[string]any{})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
}

func TestToolResponsesAreScopedBySession(t *testing.T) {
	client := &connection{
		invocations:          make(map[invocationKey]invocationResponse),
		waitingInvocations:   make(map[invocationKey]string),
		abandonedInvocations: make(map[invocationKey]time.Time),
		stateChangedCh:       make(chan struct{}, 1),
	}
	for _, sessionID := range []string{"page-session", "iframe-session"} {
		params, err := json.Marshal(invocationResponse{InvocationID: "invocation-1", Status: "Completed", Output: sessionID})
		require.NoError(t, err)
		client.handleProtocolEvent(cdpclient.Message{Method: "WebMCP.toolResponded", SessionID: sessionID, Params: params})
	}

	require.Len(t, client.invocations, 2)
	require.Equal(t, "page-session", client.invocations[invocationKey{sessionID: "page-session", invocationID: "invocation-1"}].Output)
	require.Equal(t, "iframe-session", client.invocations[invocationKey{sessionID: "iframe-session", invocationID: "invocation-1"}].Output)
}

func TestManagerReconnectsWhenChromiumUpstreamChanges(t *testing.T) {
	first := newFakeCDP(t, false)
	second := newFakeCDP(t, false)
	upstream := &mutableUpstream{url: first.url}
	manager := NewManager(upstream)
	t.Cleanup(func() { _ = manager.Close() })
	firstTools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	upstream.Set(second.url)

	secondTools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, firstTools[0].Ref, secondTools[0].Ref)
}

func TestToolRegistryIsBoundedPerSession(t *testing.T) {
	fake := newFakeCDP(t, false)
	fake.toolCount = maxToolsPerSession + 10
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, maxToolsPerSession*4)
}
