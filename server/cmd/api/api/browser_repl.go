package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/nrednav/cuid2"
)

const (
	defaultBrowserReplSocket = "/tmp/browser-repl.sock"
	defaultBrowserReplScript = "/usr/local/lib/browser-repl.js"
	defaultBrowserReplHeapMB = 512

	// browserReplStartupTimeout is how long the API waits for a freshly
	// spawned REPL child to begin accepting socket connections.
	browserReplStartupTimeout = 15 * time.Second

	// browserReplShutdownGrace is how long the API waits for SIGTERM to stop
	// the REPL process group before escalating to SIGKILL.
	browserReplShutdownGrace = 3 * time.Second

	// browserReplResponseGrace is added to the execution timeout when
	// setting the socket read deadline, giving the daemon a chance to answer
	// interruptible executions before the API kills the process. The daemon
	// reports daemon-side timeouts with timed_out: true at the requested
	// timeout, so this only covers unwind and transport time.
	browserReplResponseGrace = 2 * time.Second

	// browserReplMinTimeoutSec / browserReplMaxTimeoutSec bound timeout_sec
	// per the OpenAPI schema (minimum 1, maximum 300, default 60).
	browserReplMinTimeoutSec = 1
	browserReplMaxTimeoutSec = 300

	// browserReplMaxResponseBytes caps a single daemon response line. The
	// daemon caps image data at 16 MiB decoded (~21.3 MiB base64) plus text
	// and metadata, so 48 MiB leaves ample headroom while still bounding
	// memory on protocol corruption.
	browserReplMaxResponseBytes = 48 << 20
)

// browserReplChild tracks the owned REPL child process. The API process is
// the sole owner and supervisor: it starts the child lazily, never adopts
// orphaned processes, and always reaps the child via the wait goroutine.
type browserReplChild struct {
	id   string
	cmd  *exec.Cmd
	done chan error // receives the (single) cmd.Wait result
}

// browserReplSocketPath returns the Unix socket path for the REPL daemon.
// Overridable for tests.
func browserReplSocketPath() string {
	if p := os.Getenv("BROWSER_REPL_SOCKET"); p != "" {
		return p
	}
	return defaultBrowserReplSocket
}

// browserReplScriptPath returns the path to the bundled REPL daemon script.
// Overridable for tests.
func browserReplScriptPath() string {
	if p := os.Getenv("BROWSER_REPL_SCRIPT"); p != "" {
		return p
	}
	return defaultBrowserReplScript
}

func browserReplHeapMB() string {
	if v := os.Getenv("BROWSER_REPL_HEAP_MB"); v != "" {
		return v
	}
	return fmt.Sprint(defaultBrowserReplHeapMB)
}

// ensureBrowserReplLocked starts the REPL child if none is running. If the
// previous child died unexpectedly it is cleared and replaced with a fresh
// REPL (and fresh CUID2). Callers must hold s.browserReplMu.
func (s *ApiService) ensureBrowserReplLocked(ctx context.Context) error {
	log := logger.FromContext(ctx)

	if child := s.browserRepl; child != nil {
		select {
		case err := <-child.done:
			// The wait goroutine already reaped the child; do not consume the
			// result here. Replace the channel so the value remains observable.
			log.Warn("browser REPL child exited unexpectedly; starting a fresh REPL",
				"repl_id", child.id, "exit_err", err)
			child.done = closedWaitChannel(err)
			s.clearBrowserReplLocked(child)
		default:
			return nil
		}
	}

	return s.startBrowserReplLocked(ctx)
}

// closedWaitChannel returns a channel that has already received (and closed
// over) the given wait result.
func closedWaitChannel(err error) chan error {
	ch := make(chan error, 1)
	ch <- err
	return ch
}

// clearBrowserReplLocked detaches the child handle and removes the stale
// socket. Callers must hold s.browserReplMu.
func (s *ApiService) clearBrowserReplLocked(child *browserReplChild) {
	if s.browserRepl == child {
		s.browserRepl = nil
	}
	_ = os.Remove(browserReplSocketPath())
}

// startBrowserReplLocked spawns a new REPL child with a fresh CUID2 and
// waits for its socket to accept connections. Callers must hold
// s.browserReplMu.
func (s *ApiService) startBrowserReplLocked(ctx context.Context) error {
	log := logger.FromContext(ctx)
	socketPath := browserReplSocketPath()

	// Never adopt state from a previous process. If something we do not own
	// is still listening on the socket (an orphaned daemon started outside
	// this API process — pdeathsig covers children the API itself spawned),
	// kill it before removing the socket file so the orphan cannot leak for
	// the container lifetime.
	if conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond); err == nil {
		conn.Close()
		if killed := killOrphanedBrowserRepl(socketPath); len(killed) > 0 {
			log.Warn("killed orphaned browser REPL process(es) holding the socket",
				"pids", killed, "socket", socketPath)
		}
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove stale browser REPL socket", "path", socketPath, "err", err)
	}

	replID := cuid2.Generate()

	cmd := exec.Command("node", "--max-old-space-size="+browserReplHeapMB(), browserReplScriptPath())
	cmd.Stdout = os.Stderr // protocol lives on the socket; child diagnostics only
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"BROWSER_REPL_SOCKET="+socketPath,
		"BROWSER_REPL_ID="+replID,
	)
	configureBrowserReplCmd(cmd)

	log.Info("starting browser REPL", "repl_id", replID, "socket", socketPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start browser REPL: %w", err)
	}

	child := &browserReplChild{id: replID, cmd: cmd, done: make(chan error, 1)}
	go func() {
		child.done <- cmd.Wait()
	}()
	s.browserRepl = child

	deadline := time.Now().Add(browserReplStartupTimeout)
	for {
		conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			log.Info("browser REPL ready", "repl_id", replID)
			return nil
		}
		select {
		case waitErr := <-child.done:
			child.done = closedWaitChannel(waitErr)
			s.clearBrowserReplLocked(child)
			return fmt.Errorf("browser REPL exited during startup: %w", waitErr)
		default:
		}
		if time.Now().After(deadline) {
			s.terminateBrowserReplLocked(ctx, "startup timeout")
			return fmt.Errorf("browser REPL failed to start within %v", browserReplStartupTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// terminateBrowserReplLocked stops the REPL child's process group (SIGTERM,
// escalating to SIGKILL), waits for exit, removes the socket, and clears the
// in-memory handle. The next request lazily starts a fresh REPL with a new
// CUID2. Returns the child's exit error when observed (nil for a clean exit
// or when the exit could not be observed within the grace period). Callers
// must hold s.browserReplMu.
func (s *ApiService) terminateBrowserReplLocked(ctx context.Context, reason string) error {
	child := s.browserRepl
	if child == nil {
		return nil
	}
	log := logger.FromContext(ctx)
	log.Info("terminating browser REPL", "repl_id", child.id, "reason", reason)

	// SIGTERM the whole process group so any grandchildren go down too.
	_ = signalBrowserReplGroup(child.cmd, termSignal)

	select {
	case err := <-child.done:
		child.done = closedWaitChannel(err)
		s.clearBrowserReplLocked(child)
		return err
	case <-time.After(browserReplShutdownGrace):
	}

	log.Warn("browser REPL did not exit on SIGTERM; escalating to SIGKILL", "repl_id", child.id)
	_ = signalBrowserReplGroup(child.cmd, killSignal)

	var waitErr error
	select {
	case err := <-child.done:
		child.done = closedWaitChannel(err)
		waitErr = err
	case <-time.After(browserReplShutdownGrace):
		log.Error("browser REPL did not exit after SIGKILL", "repl_id", child.id)
	}
	s.clearBrowserReplLocked(child)
	return waitErr
}

// killBrowserReplLocked SIGKILLs the REPL process group without a SIGTERM
// grace period. Use when the daemon's event loop is known to be blocked
// (e.g. an uninterruptible execution that never answered before the socket
// read deadline): a graceful signal could never be handled and would only
// add browserReplShutdownGrace of dead time to every such timeout. Callers
// must hold s.browserReplMu.
func (s *ApiService) killBrowserReplLocked(ctx context.Context, reason string) {
	child := s.browserRepl
	if child == nil {
		return
	}
	log := logger.FromContext(ctx)
	log.Info("killing browser REPL", "repl_id", child.id, "reason", reason)
	_ = signalBrowserReplGroup(child.cmd, killSignal)

	select {
	case err := <-child.done:
		child.done = closedWaitChannel(err)
	case <-time.After(browserReplShutdownGrace):
		log.Error("browser REPL did not exit after SIGKILL", "repl_id", child.id)
	}
	s.clearBrowserReplLocked(child)
}

// browserReplDaemonRequest is the wire format sent to the REPL daemon.
type browserReplDaemonRequest struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// browserReplDaemonResponse is the wire format returned by the REPL daemon.
type browserReplDaemonResponse struct {
	ID               string            `json:"id"`
	ReplID           string            `json:"repl_id"`
	Success          bool              `json:"success"`
	Result           json.RawMessage   `json:"result,omitempty"`
	ResultRepr       *string           `json:"result_repr,omitempty"`
	Error            string            `json:"error,omitempty"`
	Stack            *string           `json:"stack,omitempty"`
	Content          []json.RawMessage `json:"content,omitempty"`
	ResultTruncated  bool              `json:"result_truncated"`
	ContentTruncated bool              `json:"content_truncated"`
	// TimedOut marks a daemon-side execution timeout. The daemon cannot
	// interrupt the abandoned execution, so the API must kill the child
	// before serving another request (destructive timeout semantics).
	TimedOut bool `json:"timed_out,omitempty"`
	// Exiting marks a deterministic daemon shutdown after an uncaught
	// exception: the daemon answered the in-flight execution with the
	// exception details and is exiting non-zero. The API treats it like a
	// timeout — terminate the handle and report repl_terminated — so the
	// state loss is explicit to the caller.
	Exiting    bool `json:"exiting,omitempty"`
	DurationMs int  `json:"duration_ms"`
}

// executeOnBrowserReplLocked sends one execution to the current child and
// reads its response. The returned error is a transport/protocol failure;
// execution failures are reported inside the response. Callers must hold
// s.browserReplMu.
func (s *ApiService) executeOnBrowserReplLocked(ctx context.Context, code string, timeout time.Duration) (*browserReplDaemonResponse, error) {
	child := s.browserRepl
	if child == nil {
		return nil, errors.New("no browser REPL child")
	}

	conn, err := net.DialTimeout("unix", browserReplSocketPath(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to browser REPL: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout + browserReplResponseGrace)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		// Leave enough room to return a structured response if possible.
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("failed to set deadline: %w", err)
	}

	reqID := uuid.New().String()
	reqBytes, err := json.Marshal(browserReplDaemonRequest{
		ID:        reqID,
		Code:      code,
		TimeoutMs: int(timeout.Milliseconds()),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if _, err := conn.Write(append(reqBytes, '\n')); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read in a goroutine so context cancellation can abandon the read; the
	// connection is closed on return which unblocks the goroutine.
	type readResult struct {
		line []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		reader := bufio.NewReader(io.LimitReader(conn, browserReplMaxResponseBytes+1))
		line, err := reader.ReadBytes('\n')
		readCh <- readResult{line: line, err: err}
	}()

	var line []byte
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("request context cancelled: %w", ctx.Err())
	case res := <-readCh:
		if res.err != nil {
			if len(res.line) > browserReplMaxResponseBytes {
				return nil, errors.New("browser REPL response exceeds maximum size")
			}
			if errors.Is(res.err, os.ErrDeadlineExceeded) || isTimeoutErr(res.err) {
				return nil, &browserReplTimeoutError{timeout: timeout}
			}
			return nil, fmt.Errorf("failed to read response: %w", res.err)
		}
		line = res.line
	}

	if len(line) > browserReplMaxResponseBytes {
		return nil, errors.New("browser REPL response exceeds maximum size")
	}

	var resp browserReplDaemonResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if resp.ID != reqID {
		return nil, fmt.Errorf("response ID mismatch: expected %s, got %s", reqID, resp.ID)
	}
	if resp.ReplID != child.id {
		return nil, fmt.Errorf("response repl_id mismatch: expected %s, got %s", child.id, resp.ReplID)
	}

	return &resp, nil
}

// browserReplTimeoutError reports an execution that never answered before
// the API's socket read deadline (an uninterruptible execution, e.g.
// `while (true) {}`). The message matches the daemon's own timeout wording
// so both timeout paths read identically to the caller.
type browserReplTimeoutError struct {
	timeout time.Duration
}

func (e *browserReplTimeoutError) Error() string {
	return fmt.Sprintf("execution timed out after %dms", e.timeout.Milliseconds())
}

func isTimeoutErr(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// browserReplTerminatedResponse builds the 200 response for a request that
// destroyed the REPL (timeout, crash, or protocol corruption). It populates
// the same optional fields as other failure paths (duration_ms and the
// truncation flags) so clients can read them unconditionally; partial
// content is never available here because the child died without answering.
func browserReplTerminatedResponse(replID string, err error, durationMs int) oapi.ExecuteBrowserCode200JSONResponse {
	errMsg := err.Error()
	terminated := true
	notTruncated := false
	return oapi.ExecuteBrowserCode200JSONResponse{
		Success:          false,
		ReplId:           replID,
		Error:            &errMsg,
		ReplTerminated:   &terminated,
		DurationMs:       &durationMs,
		ResultTruncated:  &notTruncated,
		ContentTruncated: &notTruncated,
	}
}

// StrictBrowserExecuteBodyMiddleware enforces additionalProperties: false on
// POST /browser/execute. The generated strict-server decoder silently drops
// unknown fields, so without this middleware a request like
// {"code":"1","bogus":1} would be accepted despite the published schema.
// Malformed JSON and type errors are left to the strict handler's own 400
// handling; only unknown fields are policed here.
func StrictBrowserExecuteBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/browser/execute" || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		var probe oapi.ExecuteBrowserCodeRequest
		if err := dec.Decode(&probe); err != nil && strings.HasPrefix(err.Error(), "json: unknown field") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(oapi.BadRequestError{
				Message: fmt.Sprintf("invalid request body: %s", err.Error()),
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ExecuteBrowserCode implements POST /browser/execute. The API process owns
// the REPL child directly: lazy startup, CUID2 repl_id, destructive timeout,
// explicit reset, and termination on API shutdown.
func (s *ApiService) ExecuteBrowserCode(ctx context.Context, request oapi.ExecuteBrowserCodeRequestObject) (oapi.ExecuteBrowserCodeResponseObject, error) {
	s.browserReplMu.Lock()
	defer s.browserReplMu.Unlock()

	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.ExecuteBrowserCode400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "request body is required",
			},
		}, nil
	}

	reset := request.Body.Reset != nil && *request.Body.Reset
	code := request.Body.Code
	if code == "" && !reset {
		return oapi.ExecuteBrowserCode400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "code is required (it may be empty only when reset is true)",
			},
		}, nil
	}

	timeout := 60 * time.Second
	if request.Body.TimeoutSec != nil {
		if *request.Body.TimeoutSec < browserReplMinTimeoutSec || *request.Body.TimeoutSec > browserReplMaxTimeoutSec {
			return oapi.ExecuteBrowserCode400JSONResponse{
				BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
					Message: fmt.Sprintf("timeout_sec must be between %d and %d", browserReplMinTimeoutSec, browserReplMaxTimeoutSec),
				},
			}, nil
		}
		timeout = time.Duration(*request.Body.TimeoutSec) * time.Second
	}

	if reset {
		s.terminateBrowserReplLocked(ctx, "explicit reset")
	}

	if err := s.ensureBrowserReplLocked(ctx); err != nil {
		log.Error("failed to start browser REPL", "error", err)
		return oapi.ExecuteBrowserCode500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
				Message: fmt.Sprintf("failed to start browser REPL: %v", err),
			},
		}, nil
	}

	replID := s.browserRepl.id

	// Reset with no code: just start a fresh REPL.
	if code == "" {
		return oapi.ExecuteBrowserCode200JSONResponse{
			Success: true,
			ReplId:  replID,
		}, nil
	}

	execStart := time.Now()
	resp, err := s.executeOnBrowserReplLocked(ctx, code, timeout)
	if err != nil {
		// Any transport or protocol failure is fatal to the child: kill the
		// process group, wait for exit, remove the stale socket, and clear the
		// handle. The next request lazily starts a fresh REPL with a new ID.
		log.Error("browser REPL execution failed; terminating child", "repl_id", replID, "error", err)
		var timeoutErr *browserReplTimeoutError
		if errors.As(err, &timeoutErr) {
			// The daemon never answered, so its event loop is blocked and a
			// graceful SIGTERM could never be handled; kill immediately.
			s.killBrowserReplLocked(ctx, "execution timeout")
		} else if waitErr := s.terminateBrowserReplLocked(ctx, "execution failure"); waitErr != nil {
			// Surface the child's exit reason (e.g. SIGKILL from the OOM
			// killer near the heap cap) instead of a bare transport error.
			err = fmt.Errorf("browser REPL process terminated during execution (%v): %w", waitErr, err)
		}
		return browserReplTerminatedResponse(replID, err, int(time.Since(execStart).Milliseconds())), nil
	}

	mapped, err := browserReplMapResponse(resp)
	if err != nil {
		// A response that does not decode into the public schema is protocol
		// corruption; do not risk state from this child.
		log.Error("browser REPL returned an undecodable response; terminating child", "repl_id", replID, "error", err)
		s.terminateBrowserReplLocked(ctx, "protocol corruption")
		return browserReplTerminatedResponse(replID, err, int(time.Since(execStart).Milliseconds())), nil
	}

	if resp.TimedOut || resp.Exiting {
		if resp.Exiting {
			// The daemon hit an uncaught exception, answered this execution
			// with the exception details, and is exiting non-zero (resuming
			// after an uncaught exception is unsafe per Node semantics).
			// Reap the child and report repl_terminated so the state loss is
			// explicit; the next request lazily starts a fresh REPL.
			log.Warn("browser REPL reported an uncaught exception and is exiting; terminating child", "repl_id", replID)
			s.terminateBrowserReplLocked(ctx, "uncaught exception in REPL process")
		} else {
			// A timeout is destructive: the daemon only abandoned the
			// execution, so its code is still running inside the child. Kill
			// the process group, wait for exit, and clear the handle; the next
			// request lazily starts a fresh REPL with a new CUID2. The
			// response carries the terminated ID, repl_terminated: true, and
			// the partial content the execution produced before the deadline.
			log.Warn("browser REPL execution timed out; terminating child", "repl_id", replID)
			s.terminateBrowserReplLocked(ctx, "execution timeout")
		}
		terminated := true
		mapped.ReplTerminated = &terminated
	}
	return mapped, nil
}

// browserReplMapResponse converts a daemon response into the public API
// shape, decoding typed content items through the generated union.
func browserReplMapResponse(resp *browserReplDaemonResponse) (oapi.ExecuteBrowserCode200JSONResponse, error) {
	out := oapi.ExecuteBrowserCode200JSONResponse{
		Success:          resp.Success,
		ReplId:           resp.ReplID,
		ResultRepr:       resp.ResultRepr,
		Stack:            resp.Stack,
		ResultTruncated:  &resp.ResultTruncated,
		ContentTruncated: &resp.ContentTruncated,
		DurationMs:       &resp.DurationMs,
	}

	if len(resp.Result) > 0 && string(resp.Result) != "null" {
		var result interface{}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return out, fmt.Errorf("failed to decode result: %w", err)
		}
		out.Result = result
	}

	if resp.Error != "" {
		out.Error = &resp.Error
	}

	if resp.Content != nil {
		content := make([]oapi.BrowserExecutionContent, 0, len(resp.Content))
		for i, raw := range resp.Content {
			var item oapi.BrowserExecutionContent
			if err := json.Unmarshal(raw, &item); err != nil {
				return out, fmt.Errorf("failed to decode content item %d: %w", i, err)
			}
			content = append(content, item)
		}
		out.Content = &content
	}

	return out, nil
}
