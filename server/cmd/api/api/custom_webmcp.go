package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	customWebMCPOperationTimeout = 60 * time.Second
	maxCustomWebMCPSourceBytes   = 8_000_000
	maxCustomWebMCPRequestBytes  = maxCustomWebMCPSourceBytes + (4 << 10)
	customWebMCPListOperation    = "custom_tools_list"
	customWebMCPAddOperation     = "custom_tools_add"
	customWebMCPRemoveOperation  = "custom_tool_remove"
)

type customWebMCPExecutionError struct {
	code    string
	message string
}

func (e *customWebMCPExecutionError) Error() string { return e.message }

func (s *ApiService) ListCustomWebMCPTools(ctx context.Context, _ oapi.ListCustomWebMCPToolsRequestObject) (oapi.ListCustomWebMCPToolsResponseObject, error) {
	tools, err := s.browserRepl.customWebMCPOperation(ctx, customWebMCPListOperation, "", "", "")
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

	tools, err := s.browserRepl.customWebMCPOperation(
		ctx,
		customWebMCPAddOperation,
		request.Body.Source,
		request.Body.Namespace,
		"",
	)
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
	result, err := m.customWebMCPOperationRaw(ctx, customWebMCPRemoveOperation, "", "", id)
	if err != nil {
		return false, err
	}
	var removed bool
	if err := json.Unmarshal(result, &removed); err != nil {
		return false, fmt.Errorf("decode custom WebMCP removal: %w", err)
	}
	return removed, nil
}

func (m *browserReplManager) customWebMCPOperation(
	ctx context.Context,
	operation, source, namespace, customToolID string,
) ([]oapi.CustomWebMCPDefinition, error) {
	result, err := m.customWebMCPOperationRaw(ctx, operation, source, namespace, customToolID)
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

func (m *browserReplManager) customWebMCPOperationRaw(
	ctx context.Context,
	operation, source, namespace, customToolID string,
) (json.RawMessage, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer m.release()

	operationCtx, cancelOperation, stopPropagation := m.operationContext(ctx)
	defer func() {
		stopPropagation()
		cancelOperation(nil)
	}()
	if err := context.Cause(operationCtx); err != nil {
		return nil, err
	}
	ctx = operationCtx

	request, err := prepareBrowserReplOperationWithCustomTools(
		source,
		operation,
		namespace,
		customToolID,
		customWebMCPOperationTimeout,
	)
	if err != nil {
		return nil, &customWebMCPExecutionError{message: err.Error()}
	}
	if err := m.ensureLocked(ctx); err != nil {
		return nil, fmt.Errorf("start Browser REPL: %w", err)
	}
	replID := m.child.id
	response, err := m.executeLocked(ctx, request, customWebMCPOperationTimeout)
	if err != nil {
		var notDispatched *browserReplNotDispatchedError
		if errors.As(err, &notDispatched) {
			return nil, notDispatched.cause
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
	if len(response.Result) == 0 {
		return nil, errors.New("Browser REPL returned no custom WebMCP result")
	}
	return response.Result, nil
}
