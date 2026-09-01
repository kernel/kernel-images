package api

import (
	"context"
	"errors"
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
	targetID  string
	toolRef   string
	input     map[string]any
}

func (f *fakeWebMCPClient) Tools(_ context.Context, targetID string) ([]webmcpclient.Tool, error) {
	f.targetID = targetID
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
		Ref:           "wmcp_test",
		Name:          "pay",
		Description:   "Pay for the order",
		Annotations:   &webmcpclient.Annotations{Consequential: true},
		PageTargetID:  "page-target",
		TargetID:      "iframe-target",
		TargetType:    "iframe",
		TargetURL:     "https://payments.example/element",
		FrameID:       "iframe-frame",
		FrameURL:      "https://payments.example/element",
		ParentFrameID: "page-frame",
		DocumentRef:   "iframe-target:loader-1",
	}}}
	service := &ApiService{webmcp: client}
	targetID := "page-target"

	response, err := service.GetWebMCPTools(context.Background(), oapi.GetWebMCPToolsRequestObject{
		Params: oapi.GetWebMCPToolsParams{TargetId: &targetID},
	})
	require.NoError(t, err)
	body := response.(oapi.GetWebMCPTools200JSONResponse)
	require.Equal(t, targetID, client.targetID)
	require.Len(t, body.Tools, 1)
	tool := body.Tools[0]
	require.Equal(t, "wmcp_test", tool.ToolRef)
	require.Equal(t, "iframe-target", tool.TargetId)
	require.Equal(t, "iframe-frame", tool.FrameId)
	require.Equal(t, "iframe-target:loader-1", *tool.DocumentRef)
	require.Empty(t, tool.InputSchema)
	require.True(t, tool.Annotations.Consequential)
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
