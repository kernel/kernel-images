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
	customWebMCPGetOperation     = "custom_tools_get"
	customWebMCPReplaceOperation = "custom_tools_replace"
)

type customWebMCPRuntimeSnapshot struct {
	ReplID        string                          `json:"repl_id"`
	Revision      int                             `json:"revision"`
	Source        string                          `json:"source"`
	Tools         []customWebMCPRuntimeDefinition `json:"tools"`
	Installations []oapi.CustomWebMCPInstallation `json:"installations"`
}

type customWebMCPRuntimeDefinition struct {
	ID           string                 `json:"id"`
	Kind         string                 `json:"kind"`
	Match        oapi.CustomWebMCPMatch `json:"match"`
	Tool         map[string]any         `json:"tool"`
	OutputSchema *map[string]any        `json:"outputSchema,omitempty"`
	Revision     int                    `json:"revision"`
}

func (s *ApiService) GetCustomWebMCPTools(ctx context.Context, _ oapi.GetCustomWebMCPToolsRequestObject) (oapi.GetCustomWebMCPToolsResponseObject, error) {
	snapshot, err := s.browserRepl.customWebMCPOperation(ctx, customWebMCPGetOperation, "")
	if err != nil {
		logger.FromContext(ctx).Error("failed to get custom WebMCP tools", "err", err)
		return oapi.GetCustomWebMCPTools500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to get custom WebMCP tools"}}, nil
	}
	return oapi.GetCustomWebMCPTools200JSONResponse(snapshot), nil
}

func (s *ApiService) ReplaceCustomWebMCPTools(ctx context.Context, request oapi.ReplaceCustomWebMCPToolsRequestObject) (oapi.ReplaceCustomWebMCPToolsResponseObject, error) {
	if request.Body == nil {
		return oapi.ReplaceCustomWebMCPTools400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body is required"}}, nil
	}
	if len(request.Body.Source) > maxCustomWebMCPSourceBytes {
		return oapi.ReplaceCustomWebMCPTools400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "source must not exceed 8000000 bytes"}}, nil
	}
	RecordTelemetryCode(ctx, request.Body.Source)

	snapshot, err := s.browserRepl.customWebMCPOperation(ctx, customWebMCPReplaceOperation, request.Body.Source)
	if err != nil {
		var executionErr *customWebMCPExecutionError
		if errors.As(err, &executionErr) {
			return oapi.ReplaceCustomWebMCPTools400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: executionErr.Error()}}, nil
		}
		logger.FromContext(ctx).Error("failed to replace custom WebMCP tools", "err", err)
		return oapi.ReplaceCustomWebMCPTools500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to replace custom WebMCP tools"}}, nil
	}
	return oapi.ReplaceCustomWebMCPTools200JSONResponse(snapshot), nil
}

type customWebMCPExecutionError struct {
	message string
}

func (e *customWebMCPExecutionError) Error() string { return e.message }

func (m *browserReplManager) customWebMCPOperation(ctx context.Context, operation, source string) (oapi.CustomWebMCPRegistry, error) {
	if err := m.acquire(ctx); err != nil {
		return oapi.CustomWebMCPRegistry{}, err
	}
	defer m.release()

	operationCtx, cancelOperation, stopPropagation := m.operationContext(ctx)
	defer func() {
		stopPropagation()
		cancelOperation(nil)
	}()
	if err := context.Cause(operationCtx); err != nil {
		return oapi.CustomWebMCPRegistry{}, err
	}
	ctx = operationCtx

	request, err := prepareBrowserReplOperation(source, operation, customWebMCPOperationTimeout)
	if err != nil {
		return oapi.CustomWebMCPRegistry{}, err
	}
	if err := m.ensureLocked(ctx); err != nil {
		return oapi.CustomWebMCPRegistry{}, fmt.Errorf("start Browser REPL: %w", err)
	}
	replID := m.child.id
	response, err := m.executeLocked(ctx, request, customWebMCPOperationTimeout)
	if err != nil {
		var notDispatched *browserReplNotDispatchedError
		if errors.As(err, &notDispatched) {
			return oapi.CustomWebMCPRegistry{}, notDispatched.cause
		}
		var timeoutErr *browserReplTimeoutError
		if errors.As(err, &timeoutErr) {
			m.killLocked(ctx, "custom WebMCP operation timeout")
		} else {
			m.terminateLocked(ctx, "custom WebMCP operation failure")
		}
		return oapi.CustomWebMCPRegistry{}, err
	}
	if response.TimedOut || response.Exiting {
		m.terminateLocked(ctx, "custom WebMCP operation terminated Browser REPL")
		return oapi.CustomWebMCPRegistry{}, fmt.Errorf("Browser REPL %s terminated: %s", replID, response.Error)
	}
	if !response.Success {
		return oapi.CustomWebMCPRegistry{}, &customWebMCPExecutionError{message: response.Error}
	}
	if len(response.Result) == 0 {
		return oapi.CustomWebMCPRegistry{}, errors.New("Browser REPL returned no custom WebMCP registry")
	}

	var runtime customWebMCPRuntimeSnapshot
	if err := json.Unmarshal(response.Result, &runtime); err != nil {
		m.terminateLocked(ctx, "invalid custom WebMCP response")
		return oapi.CustomWebMCPRegistry{}, fmt.Errorf("decode custom WebMCP registry: %w", err)
	}
	if runtime.ReplID != replID {
		m.terminateLocked(ctx, "custom WebMCP repl_id mismatch")
		return oapi.CustomWebMCPRegistry{}, fmt.Errorf("custom WebMCP repl_id mismatch: expected %s, got %s", replID, runtime.ReplID)
	}

	tools := make([]oapi.CustomWebMCPDefinition, 0, len(runtime.Tools))
	for _, tool := range runtime.Tools {
		if tool.Kind != "page" && tool.Kind != "cdp" {
			return oapi.CustomWebMCPRegistry{}, fmt.Errorf("custom WebMCP tool %s has invalid kind %q", tool.ID, tool.Kind)
		}
		tools = append(tools, oapi.CustomWebMCPDefinition{
			Id:           tool.ID,
			Kind:         tool.Kind,
			Match:        tool.Match,
			Tool:         tool.Tool,
			OutputSchema: tool.OutputSchema,
			Revision:     tool.Revision,
		})
	}
	if runtime.Installations == nil {
		runtime.Installations = make([]oapi.CustomWebMCPInstallation, 0)
	}
	return oapi.CustomWebMCPRegistry{
		ReplId:        runtime.ReplID,
		Revision:      runtime.Revision,
		Source:        runtime.Source,
		Tools:         tools,
		Installations: runtime.Installations,
	}, nil
}
