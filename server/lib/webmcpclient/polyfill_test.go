package webmcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type polyfillInvocation struct {
	windowID string
	name     string
	input    map[string]any
}

// servePolyfill answers the Runtime and DOM commands used to read a page's
// navigator.modelContext polyfill. Window handles are "window:<session>" for
// roots and "window:<session>:page-child" for the same-process child frame.
func (f *fakeCDP) servePolyfill(request wireRequest, respond func(any), write func(any)) {
	fail := func(message string) {
		write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": message}})
	}
	var params struct {
		ObjectID            string `json:"objectId"`
		FunctionDeclaration string `json:"functionDeclaration"`
		FrameID             string `json:"frameId"`
		Arguments           []struct {
			Value json.RawMessage `json:"value"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(request.Params, &params)
	f.mu.Lock()
	windowUnavailable := f.windowUnavailable
	tools := f.polyfillTools[params.ObjectID]
	invoke := f.polyfillInvoke
	invokeError := f.polyfillInvokeError
	invokeHang := f.polyfillInvokeHang
	navigateDuringList := f.polyfillNavigateDuringList && params.ObjectID == "window:page-session"
	f.polyfillNavigateDuringList = f.polyfillNavigateDuringList && !navigateDuringList
	f.mu.Unlock()

	switch request.Method {
	case "Runtime.evaluate":
		if windowUnavailable {
			fail("Cannot find context with specified id")
			return
		}
		respond(map[string]any{"result": map[string]any{"type": "object", "className": "Window", "objectId": "window:" + request.SessionID}})
	case "DOM.getFrameOwner":
		if params.FrameID != "page-child" {
			fail("Frame with the given id was not found.")
			return
		}
		respond(map[string]any{"backendNodeId": 7})
	case "DOM.resolveNode":
		respond(map[string]any{"object": map[string]any{"type": "object", "subtype": "node", "objectId": "iframe:" + request.SessionID}})
	case "Runtime.releaseObjectGroup":
		respond(map[string]any{})
	case "Runtime.callFunctionOn":
		switch {
		case strings.Contains(params.FunctionDeclaration, "contentWindow"):
			respond(map[string]any{"result": map[string]any{"type": "object", "className": "Window", "objectId": "window:" + request.SessionID + ":page-child"}})
		case strings.Contains(params.FunctionDeclaration, ").list("):
			if navigateDuringList {
				// The document is replaced while its tools are being read.
				write(map[string]any{
					"method": "Page.frameNavigated", "sessionId": request.SessionID,
					"params": map[string]any{"frame": map[string]any{"id": "page-frame", "loaderId": "next-loader", "url": "https://merchant.example/next"}},
				})
				time.Sleep(200 * time.Millisecond)
			}
			if tools == nil {
				respond(map[string]any{"result": map[string]any{"type": "object", "subtype": "null", "value": nil}})
				return
			}
			respond(map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"tools": tools}}})
		case strings.Contains(params.FunctionDeclaration, ").invoke("):
			if invokeHang {
				// Chromium never answers once the document navigates away.
				return
			}
			if invokeError != "" {
				fail(invokeError)
				return
			}
			var name string
			var input map[string]any
			if len(params.Arguments) != 2 ||
				json.Unmarshal(params.Arguments[0].Value, &name) != nil ||
				json.Unmarshal(params.Arguments[1].Value, &input) != nil {
				fail("unexpected invoke arguments")
				return
			}
			f.mu.Lock()
			f.polyfillInvocations = append(f.polyfillInvocations, polyfillInvocation{windowID: params.ObjectID, name: name, input: input})
			f.mu.Unlock()
			respond(map[string]any{"result": map[string]any{"type": "object", "value": invoke(params.ObjectID, name, input)}})
		default:
			fail("unexpected function")
		}
	}
}

func newPolyfillFakeCDP(t *testing.T) *fakeCDP {
	t.Helper()
	fake := newFakeCDP(t, false)
	fake.childFrameOpen = true
	fake.polyfillTools = map[string][]map[string]any{
		"window:page-session": {
			{"name": "merchant_tool", "description": "polyfill copy", "inputSchema": map[string]any{"type": "object"}},
			{
				"name": "poly_search", "title": "Search", "description": "Search the catalog.",
				"inputSchema":  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
				"outputSchema": map[string]any{"type": "object"},
				"annotations":  map[string]any{"readOnlyHint": true},
			},
		},
		"window:page-session:page-child": {
			{"name": "child_poly", "description": "child", "inputSchema": map[string]any{"type": "object"}},
		},
		"window:iframe-session": {
			{"name": "payment_poly", "description": "payment", "inputSchema": map[string]any{"type": "object"}},
		},
	}
	fake.polyfillInvoke = func(_ string, name string, input map[string]any) map[string]any {
		if name != "poly_search" {
			return map[string]any{"ok": false, "error": "Tool not found: " + name}
		}
		output, _ := json.Marshal(map[string]any{"results": []any{input["query"]}})
		return map[string]any{"ok": true, "output": string(output)}
	}
	return fake
}

func toolsByName(tools []Tool) map[string]Tool {
	byName := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	return byName
}

func TestPolyfillToolsAreDiscoveredNextToNativeTools(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 7)
	byName := toolsByName(tools)

	// The native registration wins over the polyfill copy of the same name.
	require.False(t, byName["merchant_tool"].Polyfill)
	require.Equal(t, "merchant_tool description", byName["merchant_tool"].Description)

	search := byName["poly_search"]
	require.True(t, search.Polyfill)
	require.Empty(t, search.CustomID)
	require.Equal(t, "Search", search.Title)
	require.Equal(t, "Search the catalog.", search.Description)
	require.Equal(t, map[string]any{"type": "object"}, search.OutputSchema)
	require.Equal(t, map[string]bool{"readOnlyHint": true}, search.Hints)
	require.Equal(t, 1, search.Source.TabID)
	require.Equal(t, "https://merchant.example/", search.Source.PageURL)
	require.Nil(t, search.Source.Frame)

	child := byName["child_poly"]
	require.True(t, child.Polyfill)
	require.Equal(t, 1, child.Source.TabID)
	require.NotNil(t, child.Source.Frame)
	require.Equal(t, "https://merchant.example/child", child.Source.Frame.URL)

	// An out-of-process iframe is read through its own session.
	payment := byName["payment_poly"]
	require.True(t, payment.Polyfill)
	require.NotNil(t, payment.Source.Frame)
	require.Equal(t, "https://payments.example/element", payment.Source.Frame.URL)

	// Discovery never enables the Runtime or DOM domains, which pages can detect.
	fake.mu.Lock()
	require.Zero(t, fake.methods["Runtime.enable"])
	require.Zero(t, fake.methods["DOM.enable"])
	require.Positive(t, fake.methods["Runtime.callFunctionOn"])
	fake.mu.Unlock()

	customID, targetID, err := manager.CustomTool(context.Background(), search.Ref)
	require.NoError(t, err)
	require.Empty(t, customID)
	require.Empty(t, targetID)

	// References stay stable while the registration lives.
	again, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Equal(t, search.Ref, toolsByName(again)["poly_search"].Ref)
	require.Equal(t, child.Ref, toolsByName(again)["child_poly"].Ref)

	// A tool the polyfill unregisters disappears on the next listing.
	fake.mu.Lock()
	fake.polyfillTools["window:page-session"] = fake.polyfillTools["window:page-session"][:1]
	fake.mu.Unlock()
	after, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.NotContains(t, toolsByName(after), "poly_search")
	require.Equal(t, child.Ref, toolsByName(after)["child_poly"].Ref)

	// Navigation replaces the document, so surviving names get new references.
	fake.mu.Lock()
	fake.polyfillTools["window:page-session"] = append(fake.polyfillTools["window:page-session"], map[string]any{
		"name": "poly_search", "description": "Search the catalog.", "inputSchema": map[string]any{"type": "object"},
	})
	fake.mu.Unlock()
	fake.emit(map[string]any{
		"method": "Page.frameNavigated", "sessionId": "page-session",
		"params": map[string]any{"frame": map[string]any{"id": "page-frame", "loaderId": "next-loader", "url": "https://merchant.example/next"}},
	})
	var navigated map[string]Tool
	require.Eventually(t, func() bool {
		tools, err := manager.Tools(context.Background())
		if err != nil {
			return false
		}
		navigated = toolsByName(tools)
		_, ok := navigated["poly_search"]
		return ok && navigated["poly_search"].Ref != search.Ref
	}, 3*time.Second, 20*time.Millisecond)
	require.NotContains(t, navigated, "child_poly")
}

func TestPolyfillInvocationRunsInTheRegisteringFrame(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	byName := toolsByName(tools)

	result, err := manager.Invoke(context.Background(), byName["poly_search"].Ref, map[string]any{"query": "lamp"})
	require.NoError(t, err)
	require.NotEmpty(t, result.InvocationID)
	require.Equal(t, "completed", result.Status)
	require.Equal(t, map[string]any{"results": []any{"lamp"}}, result.Output)
	require.Empty(t, result.ErrorText)

	failed, err := manager.Invoke(context.Background(), byName["child_poly"].Ref, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "error", failed.Status)
	require.Equal(t, "Tool not found: child_poly", failed.ErrorText)
	require.Nil(t, failed.Output)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, []polyfillInvocation{
		{windowID: "window:page-session", name: "poly_search", input: map[string]any{"query": "lamp"}},
		{windowID: "window:page-session:page-child", name: "child_poly", input: map[string]any{}},
	}, fake.polyfillInvocations)
}

func TestPolyfillInvocationThatNavigatesCompletesLikeNativeTools(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	ref := toolsByName(tools)["poly_search"].Ref

	fake.mu.Lock()
	fake.polyfillInvokeHang = true
	fake.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		fake.emit(map[string]any{
			"method": "Page.frameNavigated", "sessionId": "page-session",
			"params": map[string]any{"frame": map[string]any{"id": "page-frame", "loaderId": "next-loader", "url": "https://merchant.example/next"}},
		})
	}()
	started := time.Now()
	result, err := manager.Invoke(context.Background(), ref, map[string]any{})
	require.NoError(t, err)
	require.Less(t, time.Since(started), 3*time.Second)
	require.Equal(t, "completed", result.Status)
	require.Equal(t, []any{}, result.Output)
	require.NotEmpty(t, result.InvocationID)
}

func TestPolyfillInvocationProtocolFailureHasUnknownOutcome(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	ref := toolsByName(tools)["poly_search"].Ref

	fake.mu.Lock()
	fake.polyfillInvokeError = "Target closed."
	fake.mu.Unlock()
	result, err := manager.Invoke(context.Background(), ref, map[string]any{})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.NotEmpty(t, result.InvocationID)
}

func TestPolyfillInvocationTimeoutHasUnknownOutcome(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	fake.polyfillInvokeHang = true
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	ref := toolsByName(tools)["poly_search"].Ref

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := manager.Invoke(ctx, ref, map[string]any{})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.NotEmpty(t, result.InvocationID)
}

func TestPolyfillInvocationReportsMissingDocument(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	ref := toolsByName(tools)["poly_search"].Ref

	fake.mu.Lock()
	fake.windowUnavailable = true
	fake.mu.Unlock()
	_, err = manager.Invoke(context.Background(), ref, map[string]any{})
	require.ErrorIs(t, err, ErrToolNotFound)

	// An unreadable frame keeps its previous registrations until the surface
	// reports that the document changed.
	again, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Equal(t, ref, toolsByName(again)["poly_search"].Ref)
}

func TestPolyfillToolsShareTheSessionLimit(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	oversized := make([]map[string]any, 0, maxToolsPerSession+10)
	for i := range cap(oversized) {
		oversized = append(oversized, map[string]any{
			"name": fmt.Sprintf("poly_%d", i), "description": "polyfill", "inputSchema": map[string]any{"type": "object"},
		})
	}
	fake.polyfillTools["window:page-session"] = oversized
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	pageTools := 0
	for _, tool := range tools {
		if tool.Source.TabID == 1 && tool.Source.Frame == nil {
			pageTools++
		}
	}
	require.Equal(t, maxToolsPerSession, pageTools)
}

func TestPolyfillReadOfReplacedDocumentIsDiscarded(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	fake.polyfillNavigateDuringList = true
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.NotContains(t, toolsByName(tools), "poly_search")

	// The next listing reads the new document.
	tools, err = manager.Tools(context.Background())
	require.NoError(t, err)
	require.Contains(t, toolsByName(tools), "poly_search")
}
