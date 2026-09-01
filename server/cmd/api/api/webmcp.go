package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/webmcpclient"
)

const (
	defaultWebMCPInvocationTimeout = 60 * time.Second
	maxWebMCPInvocationTimeoutSec  = 120
	maxWebMCPInputBytes            = 1 << 20
	maxWebMCPRequestBytes          = maxWebMCPInputBytes + (4 << 10)
)

func (s *ApiService) GetWebMCPTools(ctx context.Context, _ oapi.GetWebMCPToolsRequestObject) (oapi.GetWebMCPToolsResponseObject, error) {
	tools, err := s.webmcp.Tools(ctx)
	if err != nil {
		if errors.Is(err, webmcpclient.ErrNoPageTarget) {
			return oapi.GetWebMCPTools404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: err.Error()}}, nil
		}
		logger.FromContext(ctx).Error("failed to discover WebMCP tools", "err", err)
		return oapi.GetWebMCPTools500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to discover WebMCP tools"}}, nil
	}

	responseTools := make([]oapi.WebMCPTool, 0, len(tools))
	for _, tool := range tools {
		inputSchema := tool.InputSchema
		if inputSchema == nil {
			inputSchema = make(map[string]any)
		}
		responseTool := oapi.WebMCPTool{
			ToolRef:     tool.Ref,
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: inputSchema,
			Source: oapi.WebMCPToolSource{
				WindowId:  tool.Source.WindowID,
				TabId:     tool.Source.TabID,
				PageTitle: tool.Source.PageTitle,
				PageUrl:   tool.Source.PageURL,
			},
		}
		if tool.Source.Frame != nil {
			responseTool.Source.Frame = &oapi.WebMCPToolFrame{
				FrameId: tool.Source.Frame.FrameID,
				Url:     tool.Source.Frame.URL,
			}
		}
		if tool.Annotations != nil {
			responseTool.Annotations = &oapi.WebMCPToolAnnotations{
				ReadOnly:         tool.Annotations.ReadOnly,
				UntrustedContent: tool.Annotations.UntrustedContent,
				Consequential:    tool.Annotations.Consequential,
				Autosubmit:       tool.Annotations.Autosubmit,
			}
		}
		responseTools = append(responseTools, responseTool)
	}
	return oapi.GetWebMCPTools200JSONResponse{Tools: responseTools}, nil
}

func (s *ApiService) InvokeWebMCPTool(ctx context.Context, request oapi.InvokeWebMCPToolRequestObject) (oapi.InvokeWebMCPToolResponseObject, error) {
	if request.Body == nil {
		return oapi.InvokeWebMCPTool400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body is required"}}, nil
	}
	toolRefLength := utf8.RuneCountInString(request.Body.ToolRef)
	if toolRefLength < 1 || toolRefLength > 128 {
		return oapi.InvokeWebMCPTool400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "tool_ref must be between 1 and 128 characters"}}, nil
	}
	if request.Body.Input == nil {
		return oapi.InvokeWebMCPTool400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "input is required and must be an object"}}, nil
	}
	inputJSON, err := json.Marshal(request.Body.Input)
	if err != nil || len(inputJSON) > maxWebMCPInputBytes {
		return oapi.InvokeWebMCPTool400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "input must be valid JSON no larger than 1 MiB"}}, nil
	}
	timeout := defaultWebMCPInvocationTimeout
	if request.Body.TimeoutSec != nil {
		if *request.Body.TimeoutSec < 1 || *request.Body.TimeoutSec > maxWebMCPInvocationTimeoutSec {
			return oapi.InvokeWebMCPTool400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "timeout_sec must be between 1 and 120"}}, nil
		}
		timeout = time.Duration(*request.Body.TimeoutSec) * time.Second
	}
	invokeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := s.webmcp.Invoke(invokeCtx, request.Body.ToolRef, request.Body.Input)
	if err != nil {
		switch {
		case errors.Is(err, webmcpclient.ErrToolNotFound):
			return oapi.InvokeWebMCPTool404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "WebMCP tool is no longer available; discover tools again"}}, nil
		case errors.Is(err, webmcpclient.ErrOutcomeUnknown):
			failure := oapi.WebMCPInvocationFailure{
				Code:    oapi.OutcomeUnknown,
				Message: "the invocation started, but its final outcome could not be observed; do not retry automatically",
			}
			failure.InvocationId = nonEmptyString(result.InvocationID)
			return oapi.InvokeWebMCPTool504JSONResponse(failure), nil
		default:
			logger.FromContext(ctx).Error("failed to invoke WebMCP tool", "err", err)
			return oapi.InvokeWebMCPTool500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to invoke WebMCP tool"}}, nil
		}
	}

	status := oapi.WebMCPInvocationResultStatus(strings.ToLower(result.Status))
	if !status.Valid() {
		logger.FromContext(ctx).Error("WebMCP tool returned unknown status", "status", result.Status)
		return oapi.InvokeWebMCPTool500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "WebMCP tool returned an unknown status"}}, nil
	}
	response := oapi.InvokeWebMCPTool200JSONResponse{
		InvocationId: result.InvocationID,
		Status:       status,
		Output:       result.Output,
	}
	response.ErrorText = nonEmptyString(result.ErrorText)
	return response, nil
}

func nonEmptyString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
