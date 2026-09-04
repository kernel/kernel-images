package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

func (f *fakeWebMCPClient) Tools(_ context.Context) ([]webmcpclient.Tool, error) {
	return f.tools, f.toolsErr
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
	require.Empty(t, tool.InputSchema)
	require.True(t, tool.Annotations.Consequential)
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
	require.JSONEq(t, `{"tools":[{"tool_ref":"wmcp_test","name":"search","description":"","input_schema":{},"source":{"frame":null,"page_title":"Travel","page_url":"https://travel.example/","tab_id":1,"window_id":1}}]}`, string(payload))
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
