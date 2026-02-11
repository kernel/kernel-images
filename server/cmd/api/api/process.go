package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/ptyio"
)

type processHandle struct {
	id       openapi_types.UUID
	pid      int
	cmd      *exec.Cmd
	started  time.Time
	exitCode *int
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	ptyFile  *os.File
	isTTY    bool
	outCh    chan oapi.ProcessStreamEvent
	doneCh   chan struct{}
	mu       sync.RWMutex
	// attachActive guards PTY attach sessions; only one client may be attached at a time.
	attachActive bool
}

func (h *processHandle) state() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.exitCode != nil {
		return "exited"
	}
	return "running"
}

func (h *processHandle) setExited(code int) {
	h.mu.Lock()
	if h.exitCode == nil {
		h.exitCode = &code
	}
	h.mu.Unlock()
}

func buildCmd(body *oapi.ProcessExecRequest) (*exec.Cmd, error) {
	if body == nil || body.Command == "" {
		return nil, errors.New("command required")
	}
	var args []string
	if body.Args != nil {
		args = append(args, (*body.Args)...)
	}
	cmd := exec.Command(body.Command, args...)
	if body.Cwd != nil && *body.Cwd != "" {
		cmd.Dir = *body.Cwd
		// Ensure absolute if provided
		if !filepath.IsAbs(cmd.Dir) {
			// make relative to current working directory
			wd, _ := os.Getwd()
			cmd.Dir = filepath.Join(wd, cmd.Dir)
		}
	}
	// Build environment
	envMap := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	if body.Env != nil {
		for k, v := range *body.Env {
			envMap[k] = v
		}
	}
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	// Configure user if requested
	if body.AsRoot != nil && *body.AsRoot && body.AsUser != nil && *body.AsUser != "" {
		return nil, errors.New("cannot specify both as_root and as_user")
	}
	if body.AsRoot != nil && *body.AsRoot {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: 0, Gid: 0},
		}
	} else if body.AsUser != nil && *body.AsUser != "" {
		spec := *body.AsUser
		// support forms: "username" or "uid" or "uid:gid"
		var uidStr, gidStr string
		if i := strings.IndexByte(spec, ':'); i >= 0 {
			uidStr = spec[:i]
			gidStr = spec[i+1:]
		} else {
			uidStr = spec
		}

		var u *user.User
		var err error
		if _, errNum := strconv.Atoi(uidStr); errNum == nil {
			u, err = user.LookupId(uidStr)
		} else {
			u, err = user.Lookup(uidStr)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to lookup user %q: %w", spec, err)
		}
		uid64, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid uid for user %q: %w", spec, err)
		}
		gid64, err := strconv.ParseUint(u.Gid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid gid for user %q: %w", spec, err)
		}
		// If gid override provided, require it to be numeric
		if gidStr != "" {
			if gOverride, err := strconv.ParseUint(gidStr, 10, 32); err == nil {
				gid64 = gOverride
			} else {
				return nil, fmt.Errorf("gid override must be numeric, got %q", gidStr)
			}
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(uid64), Gid: uint32(gid64)},
		}
	}
	return cmd, nil
}

// Execute a command synchronously
// (POST /process/exec)
func (s *ApiService) ProcessExec(ctx context.Context, request oapi.ProcessExecRequestObject) (oapi.ProcessExecResponseObject, error) {
	log := logger.FromContext(ctx)
	if request.Body == nil {
		return oapi.ProcessExec400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	cmd, err := buildCmd((*oapi.ProcessExecRequest)(request.Body))
	if err != nil {
		return oapi.ProcessExec400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Handle timeout if provided
	start := time.Now()
	var cancel context.CancelFunc
	if request.Body.TimeoutSec != nil && *request.Body.TimeoutSec > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*request.Body.TimeoutSec)*time.Second)
		defer cancel()
	}
	if err := cmd.Start(); err != nil {
		log.Error("failed to start process", "err", err)
		return oapi.ProcessExec500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start process"}}, nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done // ensure wait returns
		return oapi.ProcessExec500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "process timed out"}}, nil
	case err := <-done:
		// proceed
		_ = err
	}
	durationMs := int(time.Since(start) / time.Millisecond)
	exitCode := 0
	if cmd.ProcessState != nil {
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			exitCode = status.ExitStatus()
		}
	}

	resp := oapi.ProcessExec200JSONResponse{
		ExitCode:   &exitCode,
		StdoutB64:  ptrOf(base64.StdEncoding.EncodeToString(stdoutBuf.Bytes())),
		StderrB64:  ptrOf(base64.StdEncoding.EncodeToString(stderrBuf.Bytes())),
		DurationMs: &durationMs,
	}
	return resp, nil
}

// Execute a command asynchronously
// (POST /process/spawn)
func (s *ApiService) ProcessSpawn(ctx context.Context, request oapi.ProcessSpawnRequestObject) (oapi.ProcessSpawnResponseObject, error) {
	log := logger.FromContext(ctx)
	if request.Body == nil {
		return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	// Build from ProcessExecRequest shape
	execReq := oapi.ProcessExecRequest{
		Command:    request.Body.Command,
		Args:       request.Body.Args,
		Cwd:        request.Body.Cwd,
		Env:        request.Body.Env,
		AsUser:     request.Body.AsUser,
		AsRoot:     request.Body.AsRoot,
		TimeoutSec: request.Body.TimeoutSec,
	}
	cmd, err := buildCmd(&execReq)
	if err != nil {
		return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}

	var (
		stdout  io.ReadCloser
		stderr  io.ReadCloser
		stdin   io.WriteCloser
		ptyFile *os.File
		isTTY   bool
	)
	// PTY mode when requested
	if request.Body.AllocateTty != nil && *request.Body.AllocateTty {
		// Validate rows/cols before starting the process
		const maxUint16 = 65535
		if request.Body.Rows != nil && *request.Body.Rows > maxUint16 {
			return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "rows must be <= 65535"}}, nil
		}
		if request.Body.Cols != nil && *request.Body.Cols > maxUint16 {
			return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "cols must be <= 65535"}}, nil
		}
		// Ensure TERM and initial size env
		hasTerm := false
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, "TERM=") {
				hasTerm = true
				break
			}
		}
		if !hasTerm {
			cmd.Env = append(cmd.Env, "TERM=xterm-256color")
		}
		// Start with PTY
		var errStart error
		ptyFile, errStart = pty.Start(cmd)
		if errStart != nil {
			log.Error("failed to start PTY process", "err", errStart)
			return oapi.ProcessSpawn500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start process"}}, nil
		}
		// Set initial size if provided
		var rows, cols uint16
		if request.Body.Rows != nil && *request.Body.Rows > 0 {
			rows = uint16(*request.Body.Rows)
		}
		if request.Body.Cols != nil && *request.Body.Cols > 0 {
			cols = uint16(*request.Body.Cols)
		}
		if rows > 0 && cols > 0 {
			_ = pty.Setsize(ptyFile, &pty.Winsize{Rows: rows, Cols: cols})
		}
		stdout = ptyFile
		stdin = ptyFile
		isTTY = true
	} else {
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return oapi.ProcessSpawn500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to open stdout"}}, nil
		}
		stderr, err = cmd.StderrPipe()
		if err != nil {
			return oapi.ProcessSpawn500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to open stderr"}}, nil
		}
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return oapi.ProcessSpawn500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to open stdin"}}, nil
		}
		if err := cmd.Start(); err != nil {
			log.Error("failed to start process", "err", err)
			return oapi.ProcessSpawn500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start process"}}, nil
		}
	}

	// Disable scale-to-zero while the process is running.
	// Track success so we only re-enable if disable succeeded.
	stzDisabled := false
	if err := s.stz.Disable(ctx); err != nil {
		log.Error("failed to disable scale-to-zero", "err", err)
	} else {
		stzDisabled = true
	}

	id := openapi_types.UUID(uuid.New())
	h := &processHandle{
		id:      id,
		pid:     cmd.Process.Pid,
		cmd:     cmd,
		started: time.Now(),
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		ptyFile: ptyFile,
		isTTY:   isTTY,
		outCh:   make(chan oapi.ProcessStreamEvent, 256),
		doneCh:  make(chan struct{}),
	}

	// Store handle
	s.procMu.Lock()
	if s.procs == nil {
		s.procs = make(map[string]*processHandle)
	}
	s.procs[id.String()] = h
	s.procMu.Unlock()

	// Reader goroutines
	// In PTY mode, do NOT read from the PTY here to avoid competing with the /attach endpoint.
	// In non‑PTY mode, stdout and stderr are separate pipes, so we run two readers and tag chunks accordingly.
	if !isTTY {
		go func() {
			reader := bufio.NewReader(stdout)
			buf := make([]byte, 4096)
			for {
				n, err := reader.Read(buf)
				if n > 0 {
					data := base64.StdEncoding.EncodeToString(buf[:n])
					stream := oapi.ProcessStreamEventStream("stdout")
					h.outCh <- oapi.ProcessStreamEvent{Stream: &stream, DataB64: &data}
				}
				if err != nil {
					break
				}
			}
		}()
		go func() {
			reader := bufio.NewReader(stderr)
			buf := make([]byte, 4096)
			for {
				n, err := reader.Read(buf)
				if n > 0 {
					data := base64.StdEncoding.EncodeToString(buf[:n])
					stream := oapi.ProcessStreamEventStream("stderr")
					h.outCh <- oapi.ProcessStreamEvent{Stream: &stream, DataB64: &data}
				}
				if err != nil {
					break
				}
			}
		}()
	}

	// Waiter goroutine - use context without cancel since HTTP request may complete
	// before the process exits
	stzCtx := context.WithoutCancel(ctx)
	go func(stzWasDisabled bool) {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					code = status.ExitStatus()
				}
			}
		} else if cmd.ProcessState != nil {
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
				code = status.ExitStatus()
			}
		}
		h.setExited(code)
		// Ensure all related FDs are closed to avoid leaking descriptors.
		// In PTY mode, close the PTY master; in non-PTY mode, close individual pipes.
		if h.isTTY {
			if h.ptyFile != nil {
				_ = h.ptyFile.Close()
			}
		} else {
			if h.stdin != nil {
				_ = h.stdin.Close()
			}
			if h.stdout != nil {
				_ = h.stdout.Close()
			}
			if h.stderr != nil {
				_ = h.stderr.Close()
			}
		}

		// Re-enable scale-to-zero now that the process has exited,
		// but only if we successfully disabled it earlier
		if stzWasDisabled {
			if err := s.stz.Enable(stzCtx); err != nil {
				log.Error("failed to enable scale-to-zero", "err", err)
			}
		}

		// Send exit event
		evt := oapi.ProcessStreamEventEvent("exit")
		h.outCh <- oapi.ProcessStreamEvent{Event: &evt, ExitCode: &code}
		close(h.doneCh)
		// Retain the handle for a short period so clients can observe the
		// final "exited" status via ProcessStatus before it disappears.
		// This avoids races where the process exits immediately after spawn
		// and status polling returns 404.
		retention := 10 * time.Second
		go func(procID string) {
			time.Sleep(retention)
			s.procMu.Lock()
			delete(s.procs, procID)
			s.procMu.Unlock()
		}(id.String())
	}(stzDisabled)

	startedAt := h.started
	pid := h.pid
	return oapi.ProcessSpawn200JSONResponse{
		ProcessId: &id,
		Pid:       &pid,
		StartedAt: &startedAt,
	}, nil
}

// Send signal to process
// (POST /process/{process_id}/kill)
func (s *ApiService) ProcessKill(ctx context.Context, request oapi.ProcessKillRequestObject) (oapi.ProcessKillResponseObject, error) {
	log := logger.FromContext(ctx)
	id := request.ProcessId.String()
	s.procMu.RLock()
	h, ok := s.procs[id]
	s.procMu.RUnlock()
	if !ok {
		return oapi.ProcessKill404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "process not found"}}, nil
	}
	if request.Body == nil {
		return oapi.ProcessKill400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	// Map signal
	var sig syscall.Signal
	switch request.Body.Signal {
	case "TERM":
		sig = syscall.SIGTERM
	case "KILL":
		sig = syscall.SIGKILL
	case "INT":
		sig = syscall.SIGINT
	case "HUP":
		sig = syscall.SIGHUP
	default:
		return oapi.ProcessKill400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid signal"}}, nil
	}
	if h.cmd.Process == nil {
		return oapi.ProcessKill404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "process not running"}}, nil
	}
	if err := h.cmd.Process.Signal(sig); err != nil {
		log.Error("failed to signal process", "err", err)
		return oapi.ProcessKill500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to signal process"}}, nil
	}
	return oapi.ProcessKill200JSONResponse(oapi.OkResponse{Ok: true}), nil
}

// Get process status
// (GET /process/{process_id}/status)
func (s *ApiService) ProcessStatus(ctx context.Context, request oapi.ProcessStatusRequestObject) (oapi.ProcessStatusResponseObject, error) {
	id := request.ProcessId.String()
	s.procMu.RLock()
	h, ok := s.procs[id]
	s.procMu.RUnlock()
	if !ok {
		return oapi.ProcessStatus404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "process not found"}}, nil
	}
	stateStr := h.state()
	state := oapi.ProcessStatusState(stateStr)
	var exitCode *int
	h.mu.RLock()
	if h.exitCode != nil {
		v := *h.exitCode
		exitCode = &v
	}
	pid := h.pid
	h.mu.RUnlock()
	// Best-effort memory stats via /proc
	var memBytes int
	if stateStr == "running" && pid > 0 {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status"); err == nil {
			// Parse VmRSS:   123 kB
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if v, err := strconv.Atoi(fields[1]); err == nil {
							// fields[2] is likely kB
							memBytes = v * 1024
						}
					}
					break
				}
			}
		}
	}
	cpuPct := float32(0)
	resp := oapi.ProcessStatus200JSONResponse{State: &state, ExitCode: exitCode, CpuPct: &cpuPct}
	if memBytes > 0 {
		resp.MemBytes = ptrOf(memBytes)
	}
	return resp, nil
}

// Write to process stdin
// (POST /process/{process_id}/stdin)
func (s *ApiService) ProcessStdin(ctx context.Context, request oapi.ProcessStdinRequestObject) (oapi.ProcessStdinResponseObject, error) {
	id := request.ProcessId.String()
	s.procMu.RLock()
	h, ok := s.procs[id]
	s.procMu.RUnlock()
	if !ok {
		return oapi.ProcessStdin404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "process not found"}}, nil
	}
	if request.Body == nil {
		return oapi.ProcessStdin400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	data, err := base64.StdEncoding.DecodeString(request.Body.DataB64)
	if err != nil {
		return oapi.ProcessStdin400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid base64"}}, nil
	}
	n, err := h.stdin.Write(data)
	if err != nil {
		return oapi.ProcessStdin500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to write to stdin"}}, nil
	}
	return oapi.ProcessStdin200JSONResponse{WrittenBytes: ptrOf(n)}, nil
}

// Stream process stdout/stderr (SSE)
// (GET /process/{process_id}/stdout/stream)
func (s *ApiService) ProcessStdoutStream(ctx context.Context, request oapi.ProcessStdoutStreamRequestObject) (oapi.ProcessStdoutStreamResponseObject, error) {
	log := logger.FromContext(ctx)
	id := request.ProcessId.String()
	s.procMu.RLock()
	h, ok := s.procs[id]
	s.procMu.RUnlock()
	if !ok {
		return oapi.ProcessStdoutStream404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "process not found"}}, nil
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			select {
			case evt := <-h.outCh:
				// Write SSE: data: <json>\n\n
				var buf bytes.Buffer
				if err := json.NewEncoder(&buf).Encode(evt); err != nil {
					log.Error("failed to marshal event", "err", err)
					return
				}
				line := bytes.TrimRight(buf.Bytes(), "\n")
				if _, err := pw.Write([]byte("data: ")); err != nil {
					return
				}
				if _, err := pw.Write(line); err != nil {
					return
				}
				if _, err := pw.Write([]byte("\n\n")); err != nil {
					return
				}
			case <-h.doneCh:
				return
			}
		}
	}()

	headers := oapi.ProcessStdoutStream200ResponseHeaders{XSSEContentType: "application/json"}
	return oapi.ProcessStdoutStream200TexteventStreamResponse{Body: pr, Headers: headers, ContentLength: 0}, nil
}

func ptrOf[T any](v T) *T { return &v }

// Resize PTY-backed process
// (POST /process/{process_id}/resize)
func (s *ApiService) ProcessResize(ctx context.Context, request oapi.ProcessResizeRequestObject) (oapi.ProcessResizeResponseObject, error) {
	id := request.ProcessId.String()
	if request.Body == nil {
		return oapi.ProcessResize400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	rows := request.Body.Rows
	cols := request.Body.Cols
	if rows <= 0 || cols <= 0 {
		return oapi.ProcessResize400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "rows and cols must be > 0"}}, nil
	}
	const maxUint16 = 65535
	if rows > maxUint16 || cols > maxUint16 {
		return oapi.ProcessResize400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "rows and cols must be <= 65535"}}, nil
	}
	s.procMu.RLock()
	h, ok := s.procs[id]
	s.procMu.RUnlock()
	if !ok {
		return oapi.ProcessResize404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "process not found"}}, nil
	}
	if !h.isTTY || h.ptyFile == nil {
		return oapi.ProcessResize400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "process is not PTY-backed"}}, nil
	}
	ws := &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
	if err := pty.Setsize(h.ptyFile, ws); err != nil {
		return oapi.ProcessResize500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to resize PTY"}}, nil
	}
	return oapi.ProcessResize200JSONResponse(oapi.OkResponse{Ok: true}), nil
}

// writeJSON writes a JSON response with the given status code.
// Unlike http.Error, this sets the correct Content-Type for JSON.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// HandleProcessAttachWS handles PTY attach via WebSocket for bidirectional streaming.
// Protocol:
//   - Client sends BinaryMessage for stdin data
//   - Server sends BinaryMessage for stdout data
//   - Client sends TextMessage with JSON for control (e.g., resize)
//   - Server sends TextMessage with JSON for events (e.g., exit code)
//
// This endpoint is intentionally not defined in OpenAPI.
func (s *ApiService) HandleProcessAttachWS(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	log := logger.FromContext(ctx)

	s.procMu.RLock()
	h, ok := s.procs[id]
	s.procMu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, `{"type":"error","message":"process not found"}`)
		return
	}
	if !h.isTTY || h.ptyFile == nil {
		writeJSON(w, http.StatusBadRequest, `{"type":"error","message":"process is not PTY-backed"}`)
		return
	}
	// Enforce single concurrent attach per PTY-backed process to avoid I/O corruption.
	h.mu.Lock()
	if h.attachActive {
		h.mu.Unlock()
		writeJSON(w, http.StatusConflict, `{"type":"error","message":"process already has an active attach session"}`)
		return
	}
	h.attachActive = true
	h.mu.Unlock()

	// Accept WebSocket connection.
	// OriginPatterns allows all origins because this endpoint uses token-based auth
	// (not cookies), so CSWSH attacks are not a concern.
	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		log.Error("websocket accept failed", "err", err)
		// Send error response for non-WebSocket clients
		http.Error(w, "websocket upgrade failed", http.StatusBadRequest)
		// Clear attachActive since we're returning early
		h.mu.Lock()
		h.attachActive = false
		h.mu.Unlock()
		return
	}
	defer wsConn.CloseNow()

	// Set a generous read limit for PTY data
	wsConn.SetReadLimit(1024 * 1024) // 1MB

	log.Info("websocket attach started", "process_id", id)

	// WaitGroup to track all goroutines for clean shutdown
	var wg sync.WaitGroup

	// Coordinate shutdown so that all pumps exit when any side closes.
	done := make(chan struct{})
	var doneOnce sync.Once
	shutdown := func() {
		doneOnce.Do(func() {
			close(done)
		})
	}

	// Channel for write operations - we serialize writes through this channel
	// since WebSocket writes are not concurrent-safe.
	// writerDone signals when the writer has finished draining.
	writeCh := make(chan wsWriteOp, 64)
	writerDone := make(chan struct{})

	// Writer goroutine - serializes all writes to the WebSocket.
	// After done is closed, drains any remaining messages before exiting.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		for {
			select {
			case op := <-writeCh:
				// Use context.Background() instead of request context because we want to
				// complete pending writes even if the HTTP request context is cancelled.
				writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := wsConn.Write(writeCtx, op.msgType, op.data)
				cancel()
				if err != nil {
					log.Error("websocket write failed", "err", err)
					shutdown()
					return
				}
			case <-done:
				// Drain any remaining messages in the channel before exiting
				for {
					select {
					case op := <-writeCh:
						writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						err := wsConn.Write(writeCtx, op.msgType, op.data)
						cancel()
						if err != nil {
							log.Error("websocket write failed during drain", "err", err)
							return
						}
					default:
						// Channel is empty, we're done
						return
					}
				}
			}
		}
	}()

	// Goroutine: read from WebSocket, write to PTY (stdin)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			msgType, data, err := wsConn.Read(ctx)
			if err != nil {
				// Normal close or error - trigger shutdown
				if websocket.CloseStatus(err) != -1 {
					log.Debug("websocket closed by client", "status", websocket.CloseStatus(err))
				} else {
					log.Error("websocket read error", "err", err)
				}
				shutdown()
				return
			}

			switch msgType {
			case websocket.MessageBinary:
				// Binary data goes to PTY stdin
				if _, werr := h.ptyFile.Write(data); werr != nil {
					log.Error("pty write error", "err", werr)
					shutdown()
					return
				}
			case websocket.MessageText:
				// Text message is a control message (JSON)
				var ctrl ptyio.AttachControlMessage
				if err := json.Unmarshal(data, &ctrl); err != nil {
					// Truncate data for logging to avoid spam
					logData := string(data)
					if len(logData) > 100 {
						logData = logData[:100] + "..."
					}
					log.Error("invalid control message", "err", err, "data", logData)
					continue
				}
				switch ctrl.Type {
				case ptyio.AttachMessageResize:
					if ctrl.Rows > 0 && ctrl.Cols > 0 && ctrl.Rows <= ptyio.MaxTerminalDimension && ctrl.Cols <= ptyio.MaxTerminalDimension {
						ws := &pty.Winsize{Rows: uint16(ctrl.Rows), Cols: uint16(ctrl.Cols)}
						if err := pty.Setsize(h.ptyFile, ws); err != nil {
							log.Error("pty resize failed", "err", err)
						} else {
							log.Debug("pty resized", "rows", ctrl.Rows, "cols", ctrl.Cols)
						}
					} else {
						log.Warn("resize rejected: dimensions out of range", "rows", ctrl.Rows, "cols", ctrl.Cols)
					}
				default:
					log.Warn("unknown control message type", "type", ctrl.Type)
				}
			}
		}
	}()

	// Goroutine: read from PTY (stdout), write to WebSocket
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := ptyio.ReadPTYToWriter(h.ptyFile, func(data []byte) error {
			select {
			case writeCh <- wsWriteOp{msgType: websocket.MessageBinary, data: data}:
				return nil
			case <-done:
				return io.EOF // Signal to stop reading
			}
		}, done)
		if err != nil {
			log.Error("pty read error", "err", err)
		}
		shutdown()
	}()

	// Goroutine: watch for process exit and send exit code.
	// This must send the exit code BEFORE triggering shutdown so the writer can deliver it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-h.doneCh:
			// Process exited - send exit code before shutdown
			h.mu.RLock()
			exitCode := h.exitCode
			h.mu.RUnlock()

			exitMsg := ptyio.AttachControlMessage{Type: ptyio.AttachMessageExit, ExitCode: exitCode}
			data, _ := json.Marshal(exitMsg)

			// Send exit message to write channel
			select {
			case writeCh <- wsWriteOp{msgType: websocket.MessageText, data: data}:
				// Wait for writer to finish draining (ensures exit message is sent)
				shutdown()
				<-writerDone
			case <-done:
				// Already shutting down from another source
			}
		case <-done:
			// Shutdown triggered by another goroutine
		}
	}()

	// Wait for shutdown signal
	<-done

	// Wait for all goroutines to complete before clearing attachActive.
	// This prevents a race where a new client attaches while goroutines are still running.
	wg.Wait()

	// Close WebSocket gracefully
	wsConn.Close(websocket.StatusNormalClosure, "")

	// Now safe to clear attachActive
	h.mu.Lock()
	h.attachActive = false
	h.mu.Unlock()

	log.Info("websocket attach ended", "process_id", id)
}

// wsWriteOp represents a write operation to be performed on the WebSocket.
type wsWriteOp struct {
	msgType websocket.MessageType
	data    []byte
}
