package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/webmcpclient"
	"github.com/stretchr/testify/require"
)

func webMCPTelemetryHandler(client *fakeWebMCPClient, publish func(events.Event) (events.Envelope, bool)) http.Handler {
	r := chi.NewRouter()
	r.Use(chiMiddleware.RequestID, TelemetryHTTPMiddleware(publish), WebMCPRequestSizeMiddleware)
	strict := oapi.NewStrictHandlerWithOptions(&ApiService{webmcp: client}, []oapi.StrictMiddlewareFunc{
		TelemetryStrictMiddleware(),
	}, oapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  StrictRequestErrorHandler,
		ResponseErrorHandlerFunc: StrictResponseErrorHandler,
	})
	return oapi.HandlerFromMux(strict, r)
}

func TestWebMCPTelemetryDiscoveryIsMetadataOnly(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	for _, client := range []*fakeWebMCPClient{
		{tools: []webmcpclient.Tool{{Ref: "wmcp_test", Name: "private_tool"}}},
		{toolsErr: webmcpclient.ErrNoPageTarget},
	} {
		rp := &recordingPublisher{}
		rec := httptest.NewRecorder()
		webMCPTelemetryHandler(client, rp.publish).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webmcp/tools", nil))
		captured := rp.snapshot()
		require.Len(t, captured, 1)
		require.Equal(t, "api_call", captured[0].Type)
		require.Equal(t, events.Control, captured[0].Category)
		require.Equal(t, oapi.KernelApi, captured[0].Source.Kind)
		var data map[string]any
		require.NoError(t, json.Unmarshal(captured[0].Data, &data))
		require.Len(t, data, 4)
		require.Equal(t, "GetWebMCPTools", data["operation_id"])
		require.NotEmpty(t, data["request_id"])
		require.Equal(t, float64(rec.Code), data["status"])
		require.GreaterOrEqual(t, data["duration_ms"].(float64), 0.0)
	}
}

func TestWebMCPTelemetryInvocationOutcomes(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	for _, test := range []struct {
		name       string
		result     webmcpclient.InvocationResult
		err        error
		httpStatus int
		status     string
		errorCode  string
	}{
		{name: "completed", result: webmcpclient.InvocationResult{Status: "Completed", InvocationID: "inv_1"}, httpStatus: 200, status: "completed"},
		{name: "canceled", result: webmcpclient.InvocationResult{Status: "Canceled", InvocationID: "inv_1"}, httpStatus: 200, status: "canceled"},
		{name: "tool error", result: webmcpclient.InvocationResult{Status: "Error", InvocationID: "inv_1", ErrorText: "invalid quantity"}, httpStatus: 200, status: "error"},
		{name: "form populated", result: webmcpclient.InvocationResult{Status: "awaiting_submission", InvocationID: "inv_1"}, httpStatus: 200, status: "awaiting_submission"},
		{name: "unknown with id", result: webmcpclient.InvocationResult{InvocationID: "inv_1"}, err: webmcpclient.ErrOutcomeUnknown, httpStatus: 504, status: "outcome_unknown", errorCode: "outcome_unknown"},
		{name: "unknown without id", err: webmcpclient.ErrOutcomeUnknown, httpStatus: 504, status: "outcome_unknown", errorCode: "outcome_unknown"},
		{name: "stale reference", err: webmcpclient.ErrToolNotFound, httpStatus: 404},
		{name: "transport error", err: errors.New("private transport detail"), httpStatus: 500},
		{name: "invalid status", result: webmcpclient.InvocationResult{Status: "Unexpected", InvocationID: "inv_1"}, httpStatus: 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			rp := &recordingPublisher{}
			client := &fakeWebMCPClient{result: test.result, invokeErr: test.err}
			rec := httptest.NewRecorder()
			webMCPTelemetryHandler(client, rp.publish).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webmcp/invoke", strings.NewReader(`{"tool_ref":"wmcp_test","input":{"quantity":2},"timeout_sec":30}`)))
			require.Equal(t, test.httpStatus, rec.Code)
			captured := rp.snapshot()
			require.Len(t, captured, 1)
			require.Equal(t, "api_call", captured[0].Type)
			require.Equal(t, events.Control, captured[0].Category)
			var data oapi.BrowserApiCallEventData
			require.NoError(t, json.Unmarshal(captured[0].Data, &data))
			require.Equal(t, "InvokeWebMCPTool", data.OperationId)
			require.Equal(t, test.httpStatus, data.Status)
			require.Equal(t, "wmcp_test", *data.ToolRef)
			require.JSONEq(t, `{"quantity":2}`, *data.Input)
			require.Equal(t, 30, *data.TimeoutSec)
			require.Equal(t, nonEmptyString(test.result.InvocationID), data.InvocationId)
			require.Equal(t, nonEmptyString(test.result.ErrorText), data.ErrorText)
			require.Equal(t, nonEmptyString(test.errorCode), data.ErrorCode)
			if test.status == "" {
				require.Nil(t, data.InvocationStatus)
			} else {
				require.Equal(t, test.status, string(*data.InvocationStatus))
			}
			require.Nil(t, data.Code)
			require.Nil(t, data.ToolSource)
			require.Nil(t, data.ToolName)
			require.NotContains(t, string(captured[0].Data), "private transport detail")
		})
	}
}

func TestWebMCPTelemetryClipsInputAndSource(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	oversized := strings.Repeat("é", events.CapturedFieldCap)
	client := &fakeWebMCPClient{result: webmcpclient.InvocationResult{
		ToolName: oversized,
		Source: &webmcpclient.ToolSource{
			WindowID: 2, TabID: 3, PageURL: oversized,
			Frame: &webmcpclient.ToolFrame{FrameID: 7, URL: oversized},
		},
		InvocationID: oversized, Status: "Error", ErrorText: oversized,
		Output: map[string]any{"private_output": "not captured"},
	}}
	body, err := json.Marshal(oapi.WebMCPInvokeRequest{ToolRef: "wmcp_test", Input: map[string]any{"value": oversized}})
	require.NoError(t, err)
	rp := &recordingPublisher{}
	webMCPTelemetryHandler(client, rp.publish).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webmcp/invoke", strings.NewReader(string(body))))
	captured := rp.snapshot()
	require.Len(t, captured, 1)
	var data oapi.BrowserApiCallEventData
	require.NoError(t, json.Unmarshal(captured[0].Data, &data))
	require.NotNil(t, data.ToolSource)
	require.Equal(t, 2, data.ToolSource.WindowId)
	require.Equal(t, 3, data.ToolSource.TabId)
	require.Equal(t, 7, data.ToolSource.Frame.FrameId)
	for _, value := range []string{*data.Input, *data.ToolName, *data.InvocationId, *data.ErrorText, data.ToolSource.PageUrl, data.ToolSource.Frame.Url} {
		require.LessOrEqual(t, len(value), events.CapturedFieldCap)
		require.True(t, utf8.ValidString(value))
		require.True(t, strings.HasSuffix(value, events.TruncatedSuffix))
	}
	require.Nil(t, data.TimeoutSec)
	require.NotContains(t, string(captured[0].Data), "private_output")
	require.Equal(t, oversized, client.input["value"], "telemetry clipping must not modify the invocation")
}

func TestWebMCPTelemetryValidationFailure(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	rec := httptest.NewRecorder()
	webMCPTelemetryHandler(&fakeWebMCPClient{}, rp.publish).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webmcp/invoke", strings.NewReader(`{"tool_ref":"wmcp_test","input":{},"timeout_sec":0}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	captured := rp.snapshot()
	require.Len(t, captured, 1)
	var data oapi.BrowserApiCallEventData
	require.NoError(t, json.Unmarshal(captured[0].Data, &data))
	require.Equal(t, 400, data.Status)
	require.Equal(t, 0, *data.TimeoutSec)
	require.Equal(t, "{}", *data.Input)
	require.Nil(t, data.InvocationId)
	require.Nil(t, data.InvocationStatus)
}

func TestWebMCPTelemetryDisabled(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	DisableTelemetryMiddleware()
	rp := &recordingPublisher{}
	handler := webMCPTelemetryHandler(&fakeWebMCPClient{result: webmcpclient.InvocationResult{Status: "Completed"}}, rp.publish)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/webmcp/tools", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webmcp/invoke", strings.NewReader(`{"tool_ref":"wmcp_test","input":{}}`)))
	require.Empty(t, rp.snapshot())
}

func TestWebMCPTelemetryNestedExecuteEmitsOncePerRequest(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	server := httptest.NewServer(webMCPTelemetryHandler(&fakeWebMCPClient{result: webmcpclient.InvocationResult{Status: "Completed"}}, rp.publish))
	defer server.Close()
	code := `await webmcp.invokeTool("wmcp_test", {});`
	outer := chiHandler(t, rp.publish, "ExecutePlaywrightCode", http.StatusOK, func(ctx context.Context) {
		RecordTelemetryCode(ctx, code)
		resp, err := http.Post(server.URL+"/webmcp/invoke", "application/json", strings.NewReader(`{"tool_ref":"wmcp_test","input":{}}`))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/playwright/execute", nil))
	captured := rp.snapshot()
	require.Len(t, captured, 2)
	var innerData, outerData oapi.BrowserApiCallEventData
	require.NoError(t, json.Unmarshal(captured[0].Data, &innerData))
	require.NoError(t, json.Unmarshal(captured[1].Data, &outerData))
	require.Equal(t, "InvokeWebMCPTool", innerData.OperationId)
	require.Equal(t, "ExecutePlaywrightCode", outerData.OperationId)
	require.NotEqual(t, innerData.RequestId, outerData.RequestId)
	require.Equal(t, code, *outerData.Code)
	require.Nil(t, outerData.ToolRef)
	require.Nil(t, innerData.Code)
}
