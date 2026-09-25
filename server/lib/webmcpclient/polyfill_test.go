package webmcpclient

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Window handles are "window:<session>" for roots and
// "window:<session>:page-child" for the same-process child frame; bridge
// handles are "bridge|<window handle>".
var polyfillWindowFrames = map[string]string{
	"window:page-session":            "page-frame",
	"window:page-session:page-child": "page-child",
	"window:iframe-session":          "iframe-frame",
}

// servePolyfill answers the Runtime and DOM commands used to bridge a page's
// navigator.modelContext polyfill. A bridge sync emits WebMCP.toolsAdded and
// toolsRemoved for the difference, the way Chromium reports registrations in
// the native registry.
func (f *fakeCDP) servePolyfill(request wireRequest, respond func(any), write func(any)) {
	fail := func(message string) {
		write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": message}})
	}
	var params struct {
		ObjectID            string `json:"objectId"`
		FunctionDeclaration string `json:"functionDeclaration"`
		FrameID             string `json:"frameId"`
	}
	_ = json.Unmarshal(request.Params, &params)

	switch request.Method {
	case "Runtime.evaluate":
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
		case params.FunctionDeclaration == polyfillBridgeSource:
			f.mu.Lock()
			f.bridgesCreated++
			f.mu.Unlock()
			respond(map[string]any{"result": map[string]any{"type": "object", "objectId": "bridge|" + params.ObjectID}})
		case strings.Contains(params.FunctionDeclaration, "this.sync()"):
			windowID := strings.TrimPrefix(params.ObjectID, "bridge|")
			f.mu.Lock()
			if f.staleBridges[windowID] {
				delete(f.staleBridges, windowID)
				f.mu.Unlock()
				fail("Could not find object with given id")
				return
			}
			desired := make(map[string]bool)
			for _, name := range f.polyfillTools[windowID] {
				desired[name] = true
			}
			bridged := f.bridgedTools[windowID]
			var added, removed []map[string]any
			for name := range bridged {
				if !desired[name] {
					removed = append(removed, map[string]any{"name": name, "frameId": polyfillWindowFrames[windowID]})
				}
			}
			for name := range desired {
				if !bridged[name] {
					added = append(added, map[string]any{
						"name": name, "description": name + " description", "frameId": polyfillWindowFrames[windowID],
						"inputSchema": map[string]any{"type": "object"},
					})
				}
			}
			f.bridgedTools[windowID] = desired
			f.mu.Unlock()
			if len(removed) > 0 {
				write(map[string]any{"method": "WebMCP.toolsRemoved", "sessionId": request.SessionID, "params": map[string]any{"tools": removed}})
			}
			if len(added) > 0 {
				write(map[string]any{"method": "WebMCP.toolsAdded", "sessionId": request.SessionID, "params": map[string]any{"tools": added}})
			}
			respond(map[string]any{"result": map[string]any{"type": "boolean", "value": len(added)+len(removed) > 0}})
		default:
			fail("unexpected function")
		}
	}
}

func newPolyfillFakeCDP(t *testing.T) *fakeCDP {
	t.Helper()
	fake := newFakeCDP(t, false)
	fake.childFrameOpen = true
	fake.polyfillTools = map[string][]string{
		"window:page-session":            {"poly_search"},
		"window:page-session:page-child": {"child_poly"},
		"window:iframe-session":          {"payment_poly"},
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

func toolNames(tools []Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestPolyfillToolsAreBridgedIntoTheNativeRegistry(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })

	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{
		"bank_tool", "child_poly", "merchant_tool", "payment_poly", "payment_tool", "poly_search", "search_flights",
	}, toolNames(tools))
	byName := toolsByName(tools)
	require.Nil(t, byName["poly_search"].Source.Frame)
	require.Equal(t, "https://merchant.example/child", byName["child_poly"].Source.Frame.URL)
	// An out-of-process iframe is bridged through its own session.
	require.Equal(t, "https://payments.example/element", byName["payment_poly"].Source.Frame.URL)

	// Bridging never enables the Runtime or DOM domains, which pages can detect.
	fake.mu.Lock()
	require.Zero(t, fake.methods["Runtime.enable"])
	require.Zero(t, fake.methods["DOM.enable"])
	// Every web frame gets a bridge, including the two without a polyfill.
	require.Equal(t, 5, fake.bridgesCreated)
	fake.mu.Unlock()

	// Bridges and references are reused while the documents live.
	again, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.Equal(t, byName["poly_search"].Ref, toolsByName(again)["poly_search"].Ref)
	fake.mu.Lock()
	require.Equal(t, 5, fake.bridgesCreated)
	fake.mu.Unlock()

	// Bridged tools invoke through the native WebMCP domain.
	result, err := manager.Invoke(context.Background(), byName["poly_search"].Ref, map[string]any{"query": "lamp"})
	require.NoError(t, err)
	require.Equal(t, "Completed", result.Status)
}

func TestPolyfillUnregistrationRemovesBridgedTools(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	childRef := toolsByName(tools)["child_poly"].Ref

	fake.mu.Lock()
	fake.polyfillTools["window:page-session"] = nil
	fake.mu.Unlock()
	after, err := manager.Tools(context.Background())
	require.NoError(t, err)
	require.NotContains(t, toolsByName(after), "poly_search")
	require.Equal(t, childRef, toolsByName(after)["child_poly"].Ref)
}

func TestPolyfillBridgeIsRecreatedForANewDocument(t *testing.T) {
	fake := newPolyfillFakeCDP(t)
	manager := NewManager(staticUpstream{url: fake.url})
	t.Cleanup(func() { _ = manager.Close() })
	tools, err := manager.Tools(context.Background())
	require.NoError(t, err)
	oldRef := toolsByName(tools)["poly_search"].Ref

	// Navigation drops the document's registrations and invalidates the
	// bridge handle; the next listing bridges the new document.
	fake.mu.Lock()
	fake.staleBridges["window:page-session"] = true
	delete(fake.bridgedTools, "window:page-session")
	fake.mu.Unlock()
	fake.emit(map[string]any{
		"method": "Page.frameNavigated", "sessionId": "page-session",
		"params": map[string]any{"frame": map[string]any{"id": "page-frame", "loaderId": "next-loader", "url": "https://merchant.example/next"}},
	})
	require.Eventually(t, func() bool {
		tools, err := manager.Tools(context.Background())
		if err != nil {
			return false
		}
		tool, ok := toolsByName(tools)["poly_search"]
		return ok && tool.Ref != oldRef
	}, 3*time.Second, 20*time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 6, fake.bridgesCreated)
}
