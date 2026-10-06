package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	playwrightDaemonScript  = "/usr/local/lib/playwright-daemon.js"
	playwrightDaemonStartup = 5 * time.Second
)

type playwrightDaemonRequest struct {
	ID         string `json:"id"`
	Code       string `json:"code"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
	Executor   string `json:"executor,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	TabCreated bool   `json:"tab_created,omitempty"`
}

type playwrightDaemonResponse struct {
	ID         string      `json:"id"`
	Success    bool        `json:"success"`
	Result     interface{} `json:"result,omitempty"`
	Error      string      `json:"error,omitempty"`
	Stack      string      `json:"stack,omitempty"`
	TargetID   string      `json:"target_id,omitempty"`
	TabCreated bool        `json:"tab_created,omitempty"`
	TimedOut   bool        `json:"timed_out,omitempty"`
	TabMissing bool        `json:"tab_missing,omitempty"`
}

func (s *ApiService) ExecutePlaywrightCode(ctx context.Context, request oapi.ExecutePlaywrightCodeRequestObject) (oapi.ExecutePlaywrightCodeResponseObject, error) {
	if request.Body == nil || request.Body.Code == "" {
		return oapi.ExecutePlaywrightCode400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "code is required",
			},
		}, nil
	}

	RecordTelemetryCode(ctx, request.Body.Code)

	timeout := 60 * time.Second
	if request.Body.TimeoutSec != nil && *request.Body.TimeoutSec > 0 {
		timeout = time.Duration(*request.Body.TimeoutSec) * time.Second
	}

	name := defaultPlaywrightExecutor
	if request.Body.Executor != nil {
		name = *request.Body.Executor
	}
	return s.executePlaywrightOnExecutor(ctx, name, request.Body.Code, timeout)
}

func playwrightResult(resp *playwrightDaemonResponse) oapi.ExecutePlaywrightResult {
	result := oapi.ExecutePlaywrightResult{Success: resp.Success}
	if resp.TargetID != "" {
		result.Tab = &oapi.PlaywrightTab{TargetId: resp.TargetID, Created: resp.TabCreated}
	}
	if resp.Success {
		result.Result = &resp.Result
	} else {
		result.Error = &resp.Error
		result.Stderr = &resp.Stack
	}
	return result
}

func (s *ApiService) executePlaywrightOnExecutor(ctx context.Context, name, code string, timeout time.Duration) (oapi.ExecutePlaywrightCodeResponseObject, error) {
	log := logger.FromContext(ctx)

	if !playwrightExecutorNamePattern.MatchString(name) {
		return oapi.ExecutePlaywrightCode400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "executor name must match " + playwrightExecutorNamePattern.String(),
			},
		}, nil
	}

	resp, err := s.playwrightExecutors.Execute(ctx, name, code, timeout)
	var limitErr *playwrightExecutorLimitError
	if errors.As(err, &limitErr) {
		return oapi.ExecutePlaywrightCode409JSONResponse{
			Message:   limitErr.Error(),
			Executors: s.playwrightExecutorsJSON(ctx, limitErr.executors),
		}, nil
	}
	if errors.Is(err, errPlaywrightExecutorSetup) {
		log.Error("failed to set up playwright executor", "executor", name, "error", err)
		return oapi.ExecutePlaywrightCode500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
				Message: err.Error(),
			},
		}, nil
	}
	if err != nil {
		log.Error("playwright executor execution failed", "executor", name, "error", err)
		errorMsg := fmt.Sprintf("execution failed: %v", err)
		return oapi.ExecutePlaywrightCode200JSONResponse{
			Success: false,
			Error:   &errorMsg,
		}, nil
	}
	return oapi.ExecutePlaywrightCode200JSONResponse(playwrightResult(resp)), nil
}
