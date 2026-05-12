package api

import (
	"bufio"
	"context"
	"encoding/json"
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
	ID        string `json:"id"`
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

type playwrightDaemonResponse struct {
	ID      string      `json:"id"`
	Success bool        `json:"success"`
	Result  interface{} `json:"result,omitempty"`
	Error   string      `json:"error,omitempty"`
	Stack   string      `json:"stack,omitempty"`
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
	s.playwrightMu.Lock()
	defer s.playwrightMu.Unlock()

	log := logger.FromContext(ctx)

	if request.Body == nil || request.Body.Code == "" {
		return oapi.ExecutePlaywrightCode400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "code is required",
			},
		}, nil
	}

	timeout := 60 * time.Second
	if request.Body.TimeoutSec != nil && *request.Body.TimeoutSec > 0 {
		timeout = time.Duration(*request.Body.TimeoutSec) * time.Second
	}

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

	if !resp.Success {
		errorMsg := resp.Error
		stderr := resp.Stack
		return oapi.ExecutePlaywrightCode200JSONResponse{
			Success: false,
			Error:   &errorMsg,
			Stderr:  &stderr,
		}, nil
	}

	return oapi.ExecutePlaywrightCode200JSONResponse{
		Success: true,
		Result:  &resp.Result,
	}, nil
}
