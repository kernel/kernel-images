package api

import (
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
	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/ptyio"
	"github.com/kernel/kernel-images/server/lib/wsdrain"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

type processHandle struct {
	id         openapi_types.UUID
	pid        int
	cmd        *exec.Cmd
	started    time.Time
	exitCode   *int
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	ptyFile    *os.File
	isTTY      bool
	output     *processOutput
	reason     string
	truncated  bool
	durationMs int
	doneCh     chan struct{}
	mu         sync.RWMutex
	// attachActive guards PTY attach sessions; only one client may be attached at a time.
	attachActive bool
}

func isUserCmdError(err error) bool {
	return errors.Is(err, exec.ErrNotFound) ||
		errors.Is(err, exec.ErrDot) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOEXEC) ||
		errors.Is(err, syscall.ENOTDIR)
}

func buildCmd(body *oapi.ProcessExecRequest) (*exec.Cmd, error) {
	if body == nil || body.Command == "" {
		return nil, errors.New("command required")
	}
	var args []string
	if body.Args != nil {
		args = append(args, (*body.Args)...)
	}
	if body.TimeoutSec != nil && (*body.TimeoutSec < 0 || int64(*body.TimeoutSec) > int64((1<<63-1)/time.Second)) {
		return nil, errors.New("timeout_sec must be a nonnegative duration in seconds")
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
	cmd, err := buildCmd(request.Body)
	if err != nil {
		return oapi.ProcessExec400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}
	h, err := s.startProcess(ctx, cmd, false, false, request.Body.TimeoutSec)
	if err != nil {
		if isUserCmdError(err) {
			return oapi.ProcessExec400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		logger.FromContext(ctx).Error("failed to start process", "err", err)
		return oapi.ProcessExec500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start process"}}, nil
	}
	defer s.releaseProcess(h)
	<-h.doneCh
	var stdout, stderr bytes.Buffer
	var offset int64
	for {
		stream, data, next, err := h.output.next(context.Background(), offset)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.outputFailed(err)
			break
		}
		if stream == 0 {
			stdout.Write(data)
		} else {
			stderr.Write(data)
		}
		offset = next
	}
	h.mu.RLock()
	code, reason, truncated, duration := *h.exitCode, h.reason, h.truncated, h.durationMs
	h.mu.RUnlock()
	out, errOut := base64.StdEncoding.EncodeToString(stdout.Bytes()), base64.StdEncoding.EncodeToString(stderr.Bytes())
	if reason != "" {
		return oapi.ProcessExec422JSONResponse{
			Code: oapi.ProcessTerminationReason(reason), Message: processTerminationMessage(reason),
			ExitCode: &code, StdoutB64: &out, StderrB64: &errOut, DurationMs: &duration, OutputTruncated: &truncated,
		}, nil
	}
	return oapi.ProcessExec200JSONResponse{ExitCode: &code, StdoutB64: &out, StderrB64: &errOut, DurationMs: &duration, OutputTruncated: &truncated}, nil
}

func processTerminationMessage(reason string) string {
	switch reason {
	case "process_timeout":
		return "process timed out"
	case "output_limit":
		return "process output limit exceeded"
	case "process_canceled":
		return "process canceled"
	default:
		return "failed to capture process output"
	}
}

// Execute a command asynchronously.
func (s *ApiService) ProcessSpawn(ctx context.Context, request oapi.ProcessSpawnRequestObject) (oapi.ProcessSpawnResponseObject, error) {
	if request.Body == nil {
		return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	body := request.Body
	execReq := oapi.ProcessExecRequest{
		Command: body.Command, Args: body.Args, Cwd: body.Cwd, Env: body.Env,
		AsRoot: body.AsRoot, AsUser: body.AsUser, TimeoutSec: body.TimeoutSec,
	}
	cmd, err := buildCmd(&execReq)
	if err != nil {
		return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}
	tty := body.AllocateTty != nil && *body.AllocateTty
	if tty {
		if body.Rows != nil && *body.Rows > 65535 {
			return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "rows must be <= 65535"}}, nil
		}
		if body.Cols != nil && *body.Cols > 65535 {
			return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "cols must be <= 65535"}}, nil
		}
		hasTerm := false
		for _, env := range cmd.Env {
			if strings.HasPrefix(env, "TERM=") {
				hasTerm = true
				break
			}
		}
		if !hasTerm {
			cmd.Env = append(cmd.Env, "TERM=xterm-256color")
		}
	}
	h, err := s.startProcess(context.WithoutCancel(ctx), cmd, tty, true, body.TimeoutSec)
	if err != nil {
		if isUserCmdError(err) {
			return oapi.ProcessSpawn400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		logger.FromContext(ctx).Error("failed to start process", "err", err)
		return oapi.ProcessSpawn500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start process"}}, nil
	}
	if tty && body.Rows != nil && *body.Rows > 0 && body.Cols != nil && *body.Cols > 0 {
		_ = pty.Setsize(h.ptyFile, &pty.Winsize{Rows: uint16(*body.Rows), Cols: uint16(*body.Cols)})
	}
	go s.retainProcess(h)
	return oapi.ProcessSpawn200JSONResponse{ProcessId: &h.id, Pid: &h.pid, StartedAt: &h.started}, nil
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exitCode != nil {
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
	state := oapi.ProcessStatusState("running")
	var exitCode *int
	h.mu.RLock()
	if h.exitCode != nil {
		state = "exited"
		v := *h.exitCode
		exitCode = &v
	}
	pid := h.pid
	reason, truncated := h.reason, h.truncated
	h.mu.RUnlock()
	// Best-effort memory stats via /proc
	var memBytes int
	if state == "running" && pid > 0 {
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
	resp := oapi.ProcessStatus200JSONResponse{State: &state, ExitCode: exitCode, CpuPct: &cpuPct, OutputTruncated: &truncated}
	if reason != "" {
		resp.TerminationReason = ptrOf(oapi.ProcessTerminationReason(reason))
	}
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
	s.procMu.RLock()
	h, ok := s.procs[request.ProcessId.String()]
	s.procMu.RUnlock()
	if !ok {
		return oapi.ProcessStdoutStream404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "process not found"}}, nil
	}
	var offset int64
	var err error
	if request.Params.LastEventID != nil {
		offset, err = strconv.ParseInt(*request.Params.LastEventID, 10, 64)
	} else if request.Params.Offset != nil {
		offset = *request.Params.Offset
	}
	if err != nil || h.output.validateOffset(offset) != nil {
		return oapi.ProcessStdoutStream400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid output offset"}}, nil
	}
	return processStreamResponse{
		ctx: ctx, process: h, offset: offset,
		replay: request.Params.Offset != nil || request.Params.LastEventID != nil,
	}, nil
}

type processStreamResponse struct {
	ctx     context.Context
	process *processHandle
	offset  int64
	replay  bool
}

func (r processStreamResponse) VisitProcessStdoutStreamResponse(w http.ResponseWriter) error {
	finished, stopped := make(chan struct{}), make(chan struct{})
	controller := http.NewResponseController(w)
	go func() {
		defer close(stopped)
		select {
		case <-r.process.output.closed:
		case <-r.ctx.Done():
		case <-finished:
			return
		}
		// Interrupt a network write already in progress on expiry or disconnect.
		_ = controller.SetWriteDeadline(time.Now())
	}()
	defer func() {
		close(finished)
		<-stopped
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-SSE-Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		return err
	}
	for {
		var stream byte
		var data []byte
		var next int64
		var err error
		if r.replay {
			stream, data, next, err = r.process.output.next(r.ctx, r.offset)
		} else {
			stream, data, next, err = r.process.output.nextUnread(r.ctx)
		}
		if errors.Is(err, errProcessOutputConsumed) {
			return nil
		}
		var event oapi.ProcessStreamEvent
		if errors.Is(err, io.EOF) {
			h := r.process
			<-h.doneCh
			h.mu.RLock()
			code, reason, truncated := *h.exitCode, h.reason, h.truncated
			h.mu.RUnlock()
			event = oapi.ProcessStreamEvent{Event: ptrOf(oapi.Exit), ExitCode: &code, OutputTruncated: &truncated}
			if reason != "" {
				event.TerminationReason = ptrOf(oapi.ProcessTerminationReason(reason))
			}
		} else if err != nil {
			return err
		} else {
			source := oapi.ProcessStreamEventStream("stdout")
			if stream == 1 {
				source = "stderr"
			}
			event = oapi.ProcessStreamEvent{Stream: &source, DataB64: ptrOf(base64.StdEncoding.EncodeToString(data)), Offset: &next}
		}
		if err := writeProcessEvent(w, event); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		if event.Event != nil {
			return nil
		}
		r.offset = next
	}
}

func writeProcessEvent(w io.Writer, event oapi.ProcessStreamEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if event.Offset != nil {
		if _, err := fmt.Fprintf(w, "id: %d\n", *event.Offset); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
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
func (s *ApiService) HandleProcessAttachWS(w http.ResponseWriter, r *http.Request, id string, reg *wsdrain.Registry) {
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

	untrack := reg.Track(wsConn)
	defer untrack()

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
