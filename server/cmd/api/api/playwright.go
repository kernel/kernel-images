package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	playwrightDaemonSocket  = "/tmp/playwright-daemon.sock"
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

func (s *ApiService) ensurePlaywrightDaemon(ctx context.Context) error {
	log := logger.FromContext(ctx)

	if conn, err := net.DialTimeout("unix", playwrightDaemonSocket, 100*time.Millisecond); err == nil {
		conn.Close()
		return nil
	}

	if !atomic.CompareAndSwapInt32(&s.playwrightDaemonStarting, 0, 1) {
		deadline := time.Now().Add(playwrightDaemonStartup)
		for time.Now().Before(deadline) {
			if conn, err := net.DialTimeout("unix", playwrightDaemonSocket, 100*time.Millisecond); err == nil {
				conn.Close()
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("timeout waiting for daemon to start")
	}
	defer atomic.StoreInt32(&s.playwrightDaemonStarting, 0)

	log.Info("starting playwright daemon")

	cmd := exec.Command("node", playwrightDaemonScript)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	// Exit with the API, like the executor and REPL children. Otherwise a
	// daemon left over from a previous API keeps serving the socket and fails
	// the next API's first call.
	configureBrowserReplCmd(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start playwright daemon: %w", err)
	}

	s.playwrightDaemonCmd = cmd

	deadline := time.Now().Add(playwrightDaemonStartup)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", playwrightDaemonSocket, 100*time.Millisecond); err == nil {
			conn.Close()
			log.Info("playwright daemon started successfully")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	cmd.Process.Kill()
	return fmt.Errorf("playwright daemon failed to start within %v", playwrightDaemonStartup)
}

func (s *ApiService) executeViaUnixSocket(ctx context.Context, code string, timeout time.Duration) (*playwrightDaemonResponse, error) {
	conn, err := net.DialTimeout("unix", playwrightDaemonSocket, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout + 5*time.Second)); err != nil {
		return nil, fmt.Errorf("failed to set deadline: %w", err)
	}

	reqID := uuid.New().String()
	req := playwrightDaemonRequest{
		ID:        reqID,
		Code:      code,
		TimeoutMs: int(timeout.Milliseconds()),
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	reqBytes = append(reqBytes, '\n')

	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	reader := bufio.NewReader(conn)
	respLine, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var resp playwrightDaemonResponse
	if err := json.Unmarshal(respLine, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if resp.ID != reqID {
		return nil, fmt.Errorf("response ID mismatch: expected %s, got %s", reqID, resp.ID)
	}

	return &resp, nil
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

	if request.Body.Executor != nil {
		return s.executePlaywrightOnExecutor(ctx, *request.Body.Executor, request.Body.Code, timeout)
	}

	s.playwrightMu.Lock()
	defer s.playwrightMu.Unlock()

	log := logger.FromContext(ctx)

	if err := s.ensurePlaywrightDaemon(ctx); err != nil {
		log.Error("failed to ensure playwright daemon", "error", err)
		return oapi.ExecutePlaywrightCode500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
				Message: fmt.Sprintf("failed to start playwright daemon: %v", err),
			},
		}, nil
	}

	resp, err := s.executeViaUnixSocket(ctx, request.Body.Code, timeout)
	if err != nil {
		log.Error("playwright execution failed", "error", err)
		errorMsg := fmt.Sprintf("execution failed: %v", err)
		return oapi.ExecutePlaywrightCode200JSONResponse{
			Success: false,
			Error:   &errorMsg,
		}, nil
	}

	return oapi.ExecutePlaywrightCode200JSONResponse(playwrightResult(resp)), nil
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
