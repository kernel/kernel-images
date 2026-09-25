package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/webmcpclient"
	"github.com/stretchr/testify/require"
)

type fakeWebMCPClient struct {
	tools     []webmcpclient.Tool
	toolsErr  error
	result    webmcpclient.InvocationResult
	invokeErr error
	toolRef   string
	input     map[string]any
	customID  string
	targetID  string
}

func (f *fakeWebMCPClient) Tools(_ context.Context) ([]webmcpclient.Tool, error) {
	return f.tools, f.toolsErr
}

func (f *fakeWebMCPClient) CustomTool(_ context.Context, _ string) (string, string, error) {
	return f.customID, f.targetID, nil
}

func (f *fakeWebMCPClient) Invoke(_ context.Context, toolRef string, input map[string]any) (webmcpclient.InvocationResult, error) {
	f.toolRef = toolRef
	f.input = input
	return f.result, f.invokeErr
}

func (f *fakeWebMCPClient) Close() error { return nil }

func TestGetWebMCPToolsMapsRegistrationContext(t *testing.T) {
	client := &fakeWebMCPClient{tools: []webmcpclient.Tool{{
		Ref:         "wmcp_test",
		Name:        "pay",
		Description: "Pay for the order",
		Annotations: &webmcpclient.Annotations{Consequential: true},
		Source: webmcpclient.ToolSource{
			WindowID:  2,
			TabID:     3,
			PageTitle: "Store",
			PageURL:   "https://merchant.example/cart",
			Frame:     &webmcpclient.ToolFrame{FrameID: 7, URL: "https://payments.example/element"},
		},
	}}}
	service := &ApiService{webmcp: client}

	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{})
	require.NoError(t, err)
	body := response.(oapi.GetWebMCPTools200JSONResponse)
	require.Len(t, body.Tools, 1)
	tool := body.Tools[0]
	require.Equal(t, "wmcp_test", tool.ToolRef)
	require.Equal(t, 2, tool.Source.WindowId)
	require.Equal(t, 3, tool.Source.TabId)
	require.Equal(t, "Store", tool.Source.PageTitle)
	require.Equal(t, 7, tool.Source.Frame.FrameId)
	require.Equal(t, "https://payments.example/element", tool.Source.Frame.Url)
	require.Empty(t, tool.Tool.InputSchema)
	require.True(t, *tool.Tool.Annotations.ConsequentialHint)
}

func writeCustomToolsSnapshot(t *testing.T, manager *browserReplManager, tools []oapi.CustomWebMCPDefinition) {
	t.Helper()
	t.Setenv("BROWSER_REPL_SOCKET", filepath.Join(t.TempDir(), "browser-repl.sock"))
	manager.setCustomToolsReplID("test-repl")
	data, err := json.Marshal(struct {
		ReplID string                        `json:"repl_id"`
		Tools  []oapi.CustomWebMCPDefinition `json:"tools"`
	}{ReplID: "test-repl", Tools: tools})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(browserReplCustomToolsPath(), data, 0o600))
}

func TestGetWebMCPToolsMarksPolyfillTools(t *testing.T) {
	client := &fakeWebMCPClient{tools: []webmcpclient.Tool{{
		Ref:          "wmcp_poly",
		Name:         "search_items",
		Title:        "Search",
		Description:  "Search the catalog.",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Hints:        map[string]bool{"readOnlyHint": true, "destructiveHint": false},
		Polyfill:     true,
		Source: webmcpclient.ToolSource{
			WindowID:  1,
			TabID:     1,
			PageTitle: "Catalog",
			PageURL:   "https://shop.example/",
		},
	}}}
	service := &ApiService{webmcp: client}

	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{})
	require.NoError(t, err)
	body := response.(oapi.GetWebMCPTools200JSONResponse)
	require.Len(t, body.Tools, 1)
	tool := body.Tools[0]
	require.NotNil(t, tool.Source.Polyfill)
	require.True(t, *tool.Source.Polyfill)
	require.Nil(t, tool.Source.Custom)
	require.Nil(t, tool.Source.TargetId)
	require.Equal(t, "Search", *tool.Tool.Title)
	require.Equal(t, map[string]any{"type": "object"}, *tool.Tool.OutputSchema)
	require.True(t, *tool.Tool.Annotations.ReadOnlyHint)
	require.False(t, *tool.Tool.Annotations.DestructiveHint)
	require.Nil(t, tool.Tool.Annotations.ConsequentialHint)

	encoded, err := json.Marshal(tool)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"polyfill":true`)
}

func TestGetWebMCPToolsRejectsUnreadableSnapshot(t *testing.T) {
	manager := newBrowserReplManager()
	writeCustomToolsSnapshot(t, manager, []oapi.CustomWebMCPDefinition{{Id: "ct_abcdefghijklmnopqrstuvwx"}})
	service := &ApiService{webmcp: &fakeWebMCPClient{tools: []webmcpclient.Tool{{Ref: "native", Name: "search"}}}, browserRepl: manager}
	require.NoError(t, os.WriteFile(browserReplCustomToolsPath(), []byte(`{invalid`), 0o600))
	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{})
	require.NoError(t, err)
	_, ok := response.(oapi.GetWebMCPTools500JSONResponse)
	require.True(t, ok, "expected snapshot failure, got %T", response)

	exclude := true
	response, err = service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{
		Params: oapi.GetWebMCPToolsParams{ExcludeCustom: &exclude},
	})
	require.NoError(t, err)
	require.Len(t, response.(oapi.GetWebMCPTools200JSONResponse).Tools, 1)
}

func TestGetWebMCPToolsAddsCustomMetadataAndFiltersCustomTools(t *testing.T) {
	outputSchema := map[string]any{"type": "object"}
	manager := newBrowserReplManager()
	writeCustomToolsSnapshot(t, manager, []oapi.CustomWebMCPDefinition{{
		Id:        "ct_abcdefghijklmnopqrstuvwx",
		Namespace: "stripe.com",
		Kind:      "cdp",
		Match:     oapi.CustomWebMCPMatch{UrlPatterns: []string{"https://checkout.stripe.com/*"}},
		Tool: oapi.WebMCPToolMetadata{
			Name: "fill_payment_form", Description: "Fill payment fields",
			InputSchema: map[string]any{"type": "object"}, OutputSchema: &outputSchema,
		},
	}})
	client := &fakeWebMCPClient{tools: []webmcpclient.Tool{{
		Ref: "wmcp_custom", Name: "fill_payment_form", CustomID: "ct_abcdefghijklmnopqrstuvwx",
		Source: webmcpclient.ToolSource{WindowID: 1, TabID: 2, TargetID: "target-2", PageTitle: "Checkout", PageURL: "https://checkout.stripe.com/"},
	}}}
	service := &ApiService{webmcp: client, browserRepl: manager}

	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{})
	require.NoError(t, err)
	tools := response.(oapi.GetWebMCPTools200JSONResponse).Tools
	require.Len(t, tools, 1)
	require.Equal(t, "stripe.com", tools[0].Source.Custom.Namespace)
	require.Equal(t, "ct_abcdefghijklmnopqrstuvwx", tools[0].Source.Custom.Id)
	require.Equal(t, "target-2", *tools[0].Source.TargetId)
	require.Equal(t, outputSchema, *tools[0].Tool.OutputSchema)

	exclude := true
	response, err = service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{
		Params: oapi.GetWebMCPToolsParams{ExcludeCustom: &exclude},
	})
	require.NoError(t, err)
	require.Empty(t, response.(oapi.GetWebMCPTools200JSONResponse).Tools)
}

func TestGetWebMCPToolsOmitsOptionalCustomOutputSchema(t *testing.T) {
	manager := newBrowserReplManager()
	writeCustomToolsSnapshot(t, manager, []oapi.CustomWebMCPDefinition{{
		Id: "ct_abcdefghijklmnopqrstuvwx", Namespace: "example.com", Kind: "cdp",
		Tool: oapi.WebMCPToolMetadata{
			Name: "read_title", Description: "Read a title", InputSchema: map[string]any{"type": "object"},
		},
	}})
	service := &ApiService{browserRepl: manager, webmcp: &fakeWebMCPClient{tools: []webmcpclient.Tool{{
		Ref: "wmcp_custom", CustomID: "ct_abcdefghijklmnopqrstuvwx",
		Source: webmcpclient.ToolSource{WindowID: 1, TabID: 1},
	}}}}
	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{})
	require.NoError(t, err)
	tool := response.(oapi.GetWebMCPTools200JSONResponse).Tools[0]
	require.Nil(t, tool.Tool.OutputSchema)
	payload, err := json.Marshal(tool)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "outputSchema")
}

func TestGetWebMCPToolsSerializesNullFrameForTopLevelTool(t *testing.T) {
	client := &fakeWebMCPClient{tools: []webmcpclient.Tool{{
		Ref: "wmcp_test", Name: "search", Source: webmcpclient.ToolSource{
			WindowID: 1, TabID: 1, PageTitle: "Travel", PageURL: "https://travel.example/",
		},
	}}}
	service := &ApiService{webmcp: client}
	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{})
	require.NoError(t, err)
	payload, err := json.Marshal(response.(oapi.GetWebMCPTools200JSONResponse))
	require.NoError(t, err)
	require.JSONEq(t, `{"tools":[{"tool_ref":"wmcp_test","tool":{"name":"search","description":"","inputSchema":{}},"source":{"frame":null,"page_title":"Travel","page_url":"https://travel.example/","tab_id":1,"window_id":1}}]}`, string(payload))
}

func TestInvokeWebMCPToolReturnsPageResult(t *testing.T) {
	client := &fakeWebMCPClient{result: webmcpclient.InvocationResult{
		InvocationID: "invocation-1",
		Status:       "Completed",
		Output:       map[string]any{"ok": true},
	}}
	service := &ApiService{webmcp: client}

	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_test", Input: map[string]any{"amount": 2900}},
	})
	require.NoError(t, err)
	body := response.(oapi.InvokeWebMCPTool200JSONResponse)
	require.Equal(t, "wmcp_test", client.toolRef)
	require.Equal(t, 2900, client.input["amount"])
	require.Equal(t, oapi.WebMCPInvocationResultStatusCompleted, body.Status)
	require.Equal(t, true, body.Output.(map[string]any)["ok"])
}

func TestInvokePageCustomToolWithoutResolvedLocation(t *testing.T) {
	manager := newBrowserReplManager()
	writeCustomToolsSnapshot(t, manager, []oapi.CustomWebMCPDefinition{{Id: "ct_abcdefghijklmnopqrstuvwx", Kind: "page"}})
	client := &fakeWebMCPClient{customID: "ct_abcdefghijklmnopqrstuvwx", result: webmcpclient.InvocationResult{
		InvocationID: "invocation-1", Status: "completed", Output: map[string]any{"ok": true},
	}}
	service := &ApiService{webmcp: client, browserRepl: manager}
	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_page", Input: map[string]any{}},
	})
	require.NoError(t, err)
	require.Equal(t, oapi.WebMCPInvocationResultStatusCompleted, response.(oapi.InvokeWebMCPTool200JSONResponse).Status)
	require.Equal(t, "wmcp_page", client.toolRef)
}

func TestInvokeCDPCustomToolRequiresResolvedTarget(t *testing.T) {
	manager := newBrowserReplManager()
	writeCustomToolsSnapshot(t, manager, []oapi.CustomWebMCPDefinition{{Id: "ct_abcdefghijklmnopqrstuvwx", Kind: "cdp"}})
	client := &fakeWebMCPClient{customID: "ct_abcdefghijklmnopqrstuvwx"}
	service := &ApiService{webmcp: client, browserRepl: manager}
	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_cdp", Input: map[string]any{}},
	})
	require.NoError(t, err)
	_, ok := response.(oapi.InvokeWebMCPTool404JSONResponse)
	require.True(t, ok)
	require.Empty(t, client.toolRef)
}

func TestInvokeWebMCPToolReturnsAwaitingSubmission(t *testing.T) {
	client := &fakeWebMCPClient{result: webmcpclient.InvocationResult{
		InvocationID: "invocation-1",
		Status:       "awaiting_submission",
		Output: map[string]any{
			"form_populated": true,
			"submitted":      false,
		},
	}}
	service := &ApiService{webmcp: client}

	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_fill", Input: map[string]any{"email": "buyer@example.com"}},
	})
	require.NoError(t, err)
	body := response.(oapi.InvokeWebMCPTool200JSONResponse)
	require.Equal(t, oapi.WebMCPInvocationResultStatusAwaitingSubmission, body.Status)
	require.Equal(t, true, body.Output.(map[string]any)["form_populated"])
	require.Equal(t, false, body.Output.(map[string]any)["submitted"])
}

func TestInvokeWebMCPToolReportsUnknownOutcome(t *testing.T) {
	client := &fakeWebMCPClient{
		result:    webmcpclient.InvocationResult{InvocationID: "invocation-1"},
		invokeErr: webmcpclient.ErrOutcomeUnknown,
	}
	service := &ApiService{webmcp: client}

	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_test", Input: map[string]any{}},
	})
	require.NoError(t, err)
	body := response.(oapi.InvokeWebMCPTool504JSONResponse)
	require.Equal(t, oapi.OutcomeUnknown, body.Code)
	require.Equal(t, "invocation-1", *body.InvocationId)
}

func TestGetWebMCPToolsReturnsNotFoundWithoutPage(t *testing.T) {
	service := &ApiService{webmcp: &fakeWebMCPClient{toolsErr: webmcpclient.ErrNoPageTarget}}
	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{})
	require.NoError(t, err)
	_, ok := response.(oapi.GetWebMCPTools404JSONResponse)
	require.True(t, ok)
}

func TestInvokeWebMCPToolReturnsNotFoundForStaleReference(t *testing.T) {
	service := &ApiService{webmcp: &fakeWebMCPClient{invokeErr: webmcpclient.ErrToolNotFound}}
	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_stale", Input: map[string]any{}},
	})
	require.NoError(t, err)
	_, ok := response.(oapi.InvokeWebMCPTool404JSONResponse)
	require.True(t, ok)
}

func TestInvokeWebMCPToolRejectsMissingInputAndInvalidReference(t *testing.T) {
	for _, body := range []*oapi.WebMCPInvokeRequest{
		{ToolRef: "wmcp_test"},
		{ToolRef: "", Input: map[string]any{}},
		{ToolRef: strings.Repeat("x", 129), Input: map[string]any{}},
	} {
		client := &fakeWebMCPClient{}
		service := &ApiService{webmcp: client}
		response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{Body: body})
		require.NoError(t, err)
		_, ok := response.(oapi.InvokeWebMCPTool400JSONResponse)
		require.True(t, ok)
		require.Empty(t, client.toolRef)
	}
}

func TestInvokeWebMCPToolRejectsTimeoutOutsideBounds(t *testing.T) {
	for _, timeoutSec := range []int{0, -1, 121} {
		t.Run(fmt.Sprintf("timeout_%d", timeoutSec), func(t *testing.T) {
			client := &fakeWebMCPClient{}
			service := &ApiService{webmcp: client}
			response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
				Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_test", Input: map[string]any{}, TimeoutSec: &timeoutSec},
			})
			require.NoError(t, err)
			_, ok := response.(oapi.InvokeWebMCPTool400JSONResponse)
			require.True(t, ok)
			require.Empty(t, client.toolRef)
		})
	}
}

func TestInvokeWebMCPToolRejectsOversizedInput(t *testing.T) {
	client := &fakeWebMCPClient{}
	service := &ApiService{webmcp: client}
	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{
			ToolRef: "wmcp_test",
			Input:   map[string]any{"value": strings.Repeat("a", maxWebMCPInputBytes)},
		},
	})
	require.NoError(t, err)
	_, ok := response.(oapi.InvokeWebMCPTool400JSONResponse)
	require.True(t, ok)
	require.Empty(t, client.toolRef)
}

func TestInvokeWebMCPToolRejectsUnexpectedClientError(t *testing.T) {
	service := &ApiService{webmcp: &fakeWebMCPClient{invokeErr: errors.New("CDP failed")}}
	response, err := service.InvokeWebMCPTool(context.Background(), oapi.InvokeWebMCPToolRequestObject{
		Body: &oapi.WebMCPInvokeRequest{ToolRef: "wmcp_test", Input: map[string]any{}},
	})
	require.NoError(t, err)
	_, ok := response.(oapi.InvokeWebMCPTool500JSONResponse)
	require.True(t, ok)
}
