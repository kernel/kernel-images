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
	// The first execute against a session can arrive before the VM has finished
	// coming up, and a single 5s window is not enough to cover that: the daemon
	// binds its socket in ~100ms once the VM is ready, so a miss means the VM was
	// not ready yet rather than that the daemon is slow. Re-spawn a few times
	// instead of failing the caller's request outright.
	playwrightDaemonAttempts = 4
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

	// A concurrent caller owns the spawn; wait out its whole budget, not one
	// attempt of it, or this returns while that caller is still trying.
	if !atomic.CompareAndSwapInt32(&s.playwrightDaemonStarting, 0, 1) {
		if waitForPlaywrightDaemon(ctx, playwrightDaemonStartup*playwrightDaemonAttempts) {
			return nil
		}
		return fmt.Errorf("timeout waiting for daemon to start")
	}
	defer atomic.StoreInt32(&s.playwrightDaemonStarting, 0)

	var lastErr error
	for attempt := 1; attempt <= playwrightDaemonAttempts; attempt++ {
		log.Info("starting playwright daemon", "attempt", attempt)

		cmd := exec.Command("node", playwrightDaemonScript)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = os.Environ()

		if err := cmd.Start(); err != nil {
			lastErr = fmt.Errorf("failed to start playwright daemon: %w", err)
			continue
		}

		s.playwrightDaemonCmd = cmd

		if waitForPlaywrightDaemon(ctx, playwrightDaemonStartup) {
			log.Info("playwright daemon started successfully", "attempt", attempt)
			return nil
		}

		cmd.Process.Kill()
		lastErr = fmt.Errorf("playwright daemon failed to start within %v", playwrightDaemonStartup)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return fmt.Errorf("playwright daemon failed to start after %d attempts: %w", playwrightDaemonAttempts, lastErr)
}

// waitForPlaywrightDaemon reports whether the daemon socket accepts a
// connection before the budget expires.
func waitForPlaywrightDaemon(ctx context.Context, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", playwrightDaemonSocket, 100*time.Millisecond); err == nil {
			conn.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
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

	RecordTelemetryCode(ctx, request.Body.Code)

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
