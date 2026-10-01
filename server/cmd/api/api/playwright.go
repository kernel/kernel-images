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

const playwrightDaemonScript = "/usr/local/lib/playwright-daemon.js"

// Long enough that a busy box cannot make a healthy daemon look absent. The old
// 100ms declared one dead under load, and the spawn that followed unlinked the
// live socket out from under it.
const playwrightDaemonDial = 2 * time.Second

// Overridden in tests.
var (
	playwrightDaemonSocket = "/tmp/playwright-daemon.sock"
	// How long to wait for the daemon's socket. In the image supervisord owns
	// the daemon and restarts it, so this covers a restart rather than a cold
	// start; the old 5s was a cold-start budget and a loaded host beat it.
	playwrightDaemonStartup = 30 * time.Second
)

// Set by the image's supervisord, which starts the daemon after chromium and
// restarts it if it dies. Outside the image — a local `go run` — nothing
// supervises it, so the API starts it itself.
func playwrightDaemonSupervised() bool {
	return os.Getenv("PLAYWRIGHT_DAEMON_SUPERVISED") == "true"
}

func playwrightDaemonReachable(timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", playwrightDaemonSocket, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func waitForPlaywrightDaemon(deadline time.Time) bool {
	for time.Now().Before(deadline) {
		if playwrightDaemonReachable(playwrightDaemonDial) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

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

	if playwrightDaemonReachable(playwrightDaemonDial) {
		return nil
	}

	deadline := time.Now().Add(playwrightDaemonStartup)

	// Supervisord owns the daemon here. Spawning a second copy would race it,
	// and the loser unlinks the winner's socket, so wait for the restart.
	if playwrightDaemonSupervised() {
		log.Info("waiting for supervised playwright daemon")
		if waitForPlaywrightDaemon(deadline) {
			return nil
		}
		return fmt.Errorf("supervised playwright daemon did not come back within %v", playwrightDaemonStartup)
	}

	if !atomic.CompareAndSwapInt32(&s.playwrightDaemonStarting, 0, 1) {
		if waitForPlaywrightDaemon(deadline) {
			return nil
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
	// Nothing reads the exit status, and without this every daemon that dies
	// stays a zombie for the life of the API.
	go func() { _ = cmd.Wait() }()

	if waitForPlaywrightDaemon(deadline) {
		log.Info("playwright daemon started successfully")
		return nil
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
