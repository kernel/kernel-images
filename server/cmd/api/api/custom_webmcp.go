package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/webmcpclient"
	"github.com/nrednav/cuid2"
)

const (
	customWebMCPOperationTimeout = 60 * time.Second
	maxCustomWebMCPSourceBytes   = 8_000_000
	maxCustomWebMCPRequestBytes  = maxCustomWebMCPSourceBytes + (4 << 10)
	maxCustomWebMCPResultBytes   = 240 << 10
)

type customWebMCPExecutionError struct {
	code    string
	message string
}

func (e *customWebMCPExecutionError) Error() string { return e.message }

func (s *ApiService) ListCustomWebMCPTools(ctx context.Context, _ oapi.ListCustomWebMCPToolsRequestObject) (oapi.ListCustomWebMCPToolsResponseObject, error) {
	tools, err := s.browserRepl.customWebMCPTools(ctx, `repl.write(JSON.stringify(webmcp.listCustomTools()))`)
	if err != nil {
		logger.FromContext(ctx).Error("failed to list custom WebMCP tools", "err", err)
		return oapi.ListCustomWebMCPTools500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to list custom WebMCP tools"}}, nil
	}
	return oapi.ListCustomWebMCPTools200JSONResponse{Tools: tools}, nil
}

func (s *ApiService) AddCustomWebMCPTools(ctx context.Context, request oapi.AddCustomWebMCPToolsRequestObject) (oapi.AddCustomWebMCPToolsResponseObject, error) {
	if request.Body == nil {
		return oapi.AddCustomWebMCPTools400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body is required"}}, nil
	}
	if len(request.Body.Source) > maxCustomWebMCPSourceBytes {
		return oapi.AddCustomWebMCPTools400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "source must not exceed 8000000 bytes"}}, nil
	}
	RecordTelemetryCode(ctx, request.Body.Source)

	namespace, _ := json.Marshal(request.Body.Namespace)
	forceOverwrite := request.Body.ForceOverwriteNamespace != nil && *request.Body.ForceOverwriteNamespace
	code := fmt.Sprintf(`repl.write(JSON.stringify(await webmcp.addCustomTools({namespace: %s, tools: await (%s), forceOverwriteNamespace: %t})))`, namespace, request.Body.Source, forceOverwrite)
	tools, err := s.browserRepl.customWebMCPTools(ctx, code)
	if err != nil {
		var executionErr *customWebMCPExecutionError
		if errors.As(err, &executionErr) {
			if executionErr.code == "custom_tool_conflict" {
				return oapi.AddCustomWebMCPTools409JSONResponse{ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{Message: executionErr.Error()}}, nil
			}
			return oapi.AddCustomWebMCPTools400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: executionErr.Error()}}, nil
		}
		logger.FromContext(ctx).Error("failed to add custom WebMCP tools", "err", err)
		return oapi.AddCustomWebMCPTools500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to add custom WebMCP tools"}}, nil
	}
	return oapi.AddCustomWebMCPTools201JSONResponse{Tools: tools}, nil
}

func (s *ApiService) RemoveCustomWebMCPTool(ctx context.Context, request oapi.RemoveCustomWebMCPToolRequestObject) (oapi.RemoveCustomWebMCPToolResponseObject, error) {
	removed, err := s.browserRepl.removeCustomWebMCPTool(ctx, request.Id)
	if err != nil {
		logger.FromContext(ctx).Error("failed to remove custom WebMCP tool", "err", err)
		return oapi.RemoveCustomWebMCPTool500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to remove custom WebMCP tool"}}, nil
	}
	if !removed {
		return oapi.RemoveCustomWebMCPTool404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "custom WebMCP tool not found"}}, nil
	}
	return oapi.RemoveCustomWebMCPTool204Response{}, nil
}

func (m *browserReplManager) removeCustomWebMCPTool(ctx context.Context, id string) (bool, error) {
	encodedID, _ := json.Marshal(id)
	result, err := m.executeCustomWebMCPCode(ctx, fmt.Sprintf(`repl.write(JSON.stringify(await webmcp.removeCustomTool(%s)))`, encodedID))
	if err != nil {
		return false, err
	}
	var removed bool
	if err := json.Unmarshal(result, &removed); err != nil {
		return false, fmt.Errorf("decode custom WebMCP removal: %w", err)
	}
	return removed, nil
}

func (m *browserReplManager) customWebMCPTools(ctx context.Context, code string) ([]oapi.CustomWebMCPDefinition, error) {
	result, err := m.executeCustomWebMCPCode(ctx, code)
	if err != nil {
		return nil, err
	}
	tools := make([]oapi.CustomWebMCPDefinition, 0)
	if err := json.Unmarshal(result, &tools); err != nil {
		return nil, fmt.Errorf("decode custom WebMCP tools: %w", err)
	}
	for _, tool := range tools {
		if tool.Kind != "page" && tool.Kind != "cdp" {
			return nil, fmt.Errorf("custom WebMCP tool %s has invalid kind %q", tool.Id, tool.Kind)
		}
	}
	return tools, nil
}

func (m *browserReplManager) invokeCustomCDPTool(ctx context.Context, id, targetID string, input map[string]any, timeout time.Duration) (webmcpclient.InvocationResult, error) {
	invocation := webmcpclient.InvocationResult{InvocationID: cuid2.Generate()}
	encodedID, _ := json.Marshal(id)
	encodedTarget, _ := json.Marshal(targetID)
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return invocation, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	code := fmt.Sprintf(`{ const remaining = %d - Date.now();
		if (remaining <= 0) {
			const error = new Error('custom WebMCP invocation deadline elapsed before dispatch');
			error.code = 'custom_tool_not_dispatched';
			throw error;
		}
		const controller = new AbortController();
		let timer;
		try {
			const result = JSON.stringify(await Promise.race([
				webmcp.invokeCustomCDPTool(%s, %s, %s, controller.signal),
				new Promise((_, reject) => {
					timer = setTimeout(() => {
						const error = new Error('custom WebMCP invocation timed out');
						error.code = 'custom_tool_outcome_unknown';
						reject(error);
						controller.abort();
					}, remaining);
				}),
			]));
			if (Buffer.byteLength(result) > %d) throw new Error('custom WebMCP output exceeds 240 KiB');
			repl.write(result);
		} finally { clearTimeout(timer); }
	}`, deadline.UnixMilli(), encodedID, encodedTarget, encodedInput, maxCustomWebMCPResultBytes)
	output, err := m.executeCustomWebMCPCodeRequest(ctx, code, timeout+browserReplResponseGrace, true)
	if err != nil {
		var executionErr *customWebMCPExecutionError
		if errors.As(err, &executionErr) {
			if executionErr.code == "custom_tool_not_dispatched" {
				return invocation, context.DeadlineExceeded
			}
			if executionErr.code == "custom_tool_not_found" {
				return invocation, webmcpclient.ErrToolNotFound
			}
			if executionErr.code == "custom_tool_outcome_unknown" {
				return invocation, webmcpclient.ErrOutcomeUnknown
			}
			invocation.Status = "error"
			invocation.ErrorText = executionErr.Error()
			return invocation, nil
		}
		var notDispatched *browserReplNotDispatchedError
		if errors.As(err, &notDispatched) {
			return invocation, notDispatched.cause
		}
		return invocation, webmcpclient.ErrOutcomeUnknown
	}
	if err := json.Unmarshal(output, &invocation.Output); err != nil {
		return invocation, fmt.Errorf("decode custom WebMCP invocation: %w", err)
	}
	invocation.Status = "completed"
	return invocation, nil
}

func (m *browserReplManager) executeCustomWebMCPCode(ctx context.Context, code string) (json.RawMessage, error) {
	return m.executeCustomWebMCPCodeWithTimeout(ctx, code, customWebMCPOperationTimeout)
}

func (m *browserReplManager) executeCustomWebMCPCodeWithTimeout(ctx context.Context, code string, timeout time.Duration) (json.RawMessage, error) {
	return m.executeCustomWebMCPCodeRequest(ctx, code, timeout, false)
}

func (m *browserReplManager) executeCustomWebMCPCodeRequest(ctx context.Context, code string, timeout time.Duration, preserveOnRequestTimeout bool) (json.RawMessage, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, &browserReplNotDispatchedError{cause: err}
	}
	defer m.release()

	operationCtx, cancelOperation, stopPropagation := m.operationContext(ctx)
	defer func() {
		stopPropagation()
		cancelOperation(nil)
	}()
	if err := context.Cause(operationCtx); err != nil {
		return nil, &browserReplNotDispatchedError{cause: err}
	}
	ctx = operationCtx

	request, err := prepareBrowserReplRequest(code, timeout)
	if err != nil {
		return nil, &customWebMCPExecutionError{message: err.Error()}
	}
	if err := m.ensureLocked(ctx); err != nil {
		return nil, &browserReplNotDispatchedError{cause: fmt.Errorf("start Browser REPL: %w", err)}
	}
	replID := m.child.id
	if preserveOnRequestTimeout {
		if err := context.Cause(ctx); err != nil {
			return nil, &browserReplNotDispatchedError{cause: err}
		}
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining < time.Millisecond {
				return nil, &browserReplNotDispatchedError{cause: context.DeadlineExceeded}
			}
			timeout = remaining + browserReplResponseGrace
			request, err = prepareBrowserReplRequest(code, timeout)
			if err != nil {
				return nil, &customWebMCPExecutionError{message: err.Error()}
			}
		}
		// Once admitted, leave time for the in-cell deadline to report without resetting the REPL.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(m.lifecycle, timeout+browserReplResponseGrace)
		defer cancel()
	}
	response, err := m.executeLocked(ctx, request, timeout)
	if err != nil {
		var notDispatched *browserReplNotDispatchedError
		if errors.As(err, &notDispatched) {
			return nil, notDispatched
		}
		var timeoutErr *browserReplTimeoutError
		if errors.As(err, &timeoutErr) {
			m.killLocked(ctx, "custom WebMCP operation timeout")
		} else {
			m.terminateLocked(ctx, "custom WebMCP operation failure")
		}
		return nil, err
	}
	if response.TimedOut || response.Exiting {
		m.terminateLocked(ctx, "custom WebMCP operation terminated Browser REPL")
		return nil, fmt.Errorf("Browser REPL %s terminated: %s", replID, response.Error)
	}
	if !response.Success {
		return nil, &customWebMCPExecutionError{code: response.ErrorCode, message: response.Error}
	}
	if response.ContentTruncated {
		return nil, errors.New("Browser REPL truncated the custom WebMCP result")
	}
	for i := len(response.Content) - 1; i >= 0; i-- {
		var item struct {
			Channel string          `json:"channel"`
			Text    json.RawMessage `json:"text"`
		}
		if json.Unmarshal(response.Content[i], &item) != nil || item.Channel != "write" {
			continue
		}
		var result string
		if err := json.Unmarshal(item.Text, &result); err != nil || !json.Valid([]byte(result)) {
			return nil, errors.New("Browser REPL returned an invalid custom WebMCP result")
		}
		return json.RawMessage(result), nil
	}
	return nil, errors.New("Browser REPL returned no custom WebMCP result")
}
