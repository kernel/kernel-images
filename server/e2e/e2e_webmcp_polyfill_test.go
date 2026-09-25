package e2e

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWebMCPPolyfill covers tools that a page registers through its own
// navigator.modelContext polyfill instead of the native document.modelContext
// registry: late installation, same-origin embedded frames, native precedence
// for a shared name, invocation results, unregistration, and navigation.
func testWebMCPPolyfill(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()

	fixture, err := os.ReadFile("testdata/webmcp/polyfill.html")
	require.NoError(t, err)
	written, err := client.WriteFileWithBodyWithResponse(ctx,
		&instanceoapi.WriteFileParams{Path: "/tmp/polyfill.html"}, "text/html", bytes.NewReader(fixture))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, written.StatusCode(), "%s", written.Body)

	const pageURL = "file:///tmp/polyfill.html"
	var readyState string
	executeWebMCPPlaywright(t, ctx, client, `
		await page.goto('file:///tmp/polyfill.html', { waitUntil: 'load' });
		return page.evaluate(() => document.readyState);
	`, &readyState)
	require.Equal(t, "complete", readyState)

	toolsByName := func(collect *assert.CollectT) map[string]instanceoapi.WebMCPTool {
		rsp, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
		if !assert.NoError(collect, err) || !assert.Equal(collect, http.StatusOK, rsp.StatusCode(), "%s", rsp.Body) || rsp.JSON200 == nil {
			return nil
		}
		tools := make(map[string]instanceoapi.WebMCPTool)
		for _, tool := range rsp.JSON200.Tools {
			if tool.Source.PageUrl == pageURL {
				tools[tool.Tool.Name] = tool
			}
		}
		return tools
	}

	// The polyfill installs 1.5s after load; discovery picks it up on a later listing.
	var tools map[string]instanceoapi.WebMCPTool
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		tools = toolsByName(collect)
		for _, name := range []string{"search_items", "shared_name", "failing_tool", "navigate_away", "frame_tool"} {
			assert.Contains(collect, tools, name)
		}
	}, 15*time.Second, 250*time.Millisecond)
	t.Logf("GET /webmcp/tools: %d tools on %s", len(tools), pageURL)

	search := tools["search_items"]
	require.NotNil(t, search.Source.Polyfill)
	require.True(t, *search.Source.Polyfill)
	require.Nil(t, search.Source.Frame)
	require.Nil(t, search.Source.Custom)
	require.Equal(t, "Search the catalog.", search.Tool.Description)
	require.Equal(t, []any{"query"}, search.Tool.InputSchema["required"])

	// The same name registered natively lists once, as the native tool.
	shared := tools["shared_name"]
	require.Nil(t, shared.Source.Polyfill)
	require.Equal(t, "Native copy.", shared.Tool.Description)

	frameTool := tools["frame_tool"]
	require.NotNil(t, frameTool.Source.Polyfill)
	require.NotNil(t, frameTool.Source.Frame)
	require.Equal(t, "about:srcdoc", frameTool.Source.Frame.Url)

	timeout := 10
	invoke := func(ref string, input map[string]any) *instanceoapi.InvokeWebMCPToolResponse {
		rsp, err := client.InvokeWebMCPToolWithResponse(ctx, instanceoapi.WebMCPInvokeRequest{
			ToolRef: ref, Input: input, TimeoutSec: &timeout,
		})
		require.NoError(t, err)
		t.Logf("POST /webmcp/invoke %s: %s", ref, rsp.Body)
		return rsp
	}

	searched := invoke(search.ToolRef, map[string]any{"query": "lamp"})
	require.Equal(t, http.StatusOK, searched.StatusCode(), "%s", searched.Body)
	require.NotNil(t, searched.JSON200)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusCompleted, searched.JSON200.Status)
	require.NotEmpty(t, searched.JSON200.InvocationId)
	require.Equal(t, map[string]any{"results": []any{"match for lamp"}, "page": "/tmp/polyfill.html"}, searched.JSON200.Output)

	var logText string
	executeWebMCPPlaywright(t, ctx, client, `return page.evaluate(() => document.querySelector('#log').textContent);`, &logText)
	require.Equal(t, "searched lamp", logText)

	framed := invoke(frameTool.ToolRef, map[string]any{"item": 1})
	require.Equal(t, http.StatusOK, framed.StatusCode(), "%s", framed.Body)
	require.Equal(t, map[string]any{"frame": "Embedded polyfill catalog", "input": map[string]any{"item": float64(1)}}, framed.JSON200.Output)

	native := invoke(shared.ToolRef, map[string]any{})
	require.Equal(t, http.StatusOK, native.StatusCode(), "%s", native.Body)
	require.Equal(t, map[string]any{"source": "native"}, native.JSON200.Output)

	failed := invoke(tools["failing_tool"].ToolRef, map[string]any{})
	require.Equal(t, http.StatusOK, failed.StatusCode(), "%s", failed.Body)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusError, failed.JSON200.Status)
	require.NotNil(t, failed.JSON200.ErrorText)
	require.Equal(t, "nothing to do", *failed.JSON200.ErrorText)

	// Unregistration through the polyfill drops the tool on the next listing,
	// and the surviving tool keeps its reference.
	var clicked bool
	executeWebMCPPlaywright(t, ctx, client, `await page.click('#unregister'); return true;`, &clicked)
	require.True(t, clicked)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		tools = toolsByName(collect)
		assert.NotContains(collect, tools, "failing_tool")
		assert.Equal(collect, search.ToolRef, tools["search_items"].ToolRef)
	}, 10*time.Second, 250*time.Millisecond)

	// A tool that navigates its document completes like a native one and the
	// old registrations disappear with the document.
	navigated := invoke(tools["navigate_away"].ToolRef, map[string]any{})
	require.Equal(t, http.StatusOK, navigated.StatusCode(), "%s", navigated.Body)
	require.Equal(t, instanceoapi.WebMCPInvocationResultStatusCompleted, navigated.JSON200.Status)
	require.Equal(t, []any{}, navigated.JSON200.Output)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Empty(collect, toolsByName(collect))
	}, 10*time.Second, 250*time.Millisecond)
	stale := invoke(search.ToolRef, map[string]any{"query": "x"})
	require.Equal(t, http.StatusNotFound, stale.StatusCode(), "%s", stale.Body)
}
