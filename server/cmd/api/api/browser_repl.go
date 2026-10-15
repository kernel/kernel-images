package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/nrednav/cuid2"
)

var (
	errBrowserReplShuttingDown = errors.New("browser REPL is shutting down")
	errBrowserReplDeleted      = errors.New("executor was deleted")
	errBrowserReplRestarted    = errors.New("executor was restarted")
)

const (
	defaultBrowserReplSocket = "/tmp/browser-repl.sock"
	defaultBrowserReplScript = "/usr/local/lib/browser-repl/browser-repl.js"
	defaultBrowserReplHeapMB = 512

	// The daemon caps each newline-delimited request line at this size.
	// API requests are marshaled into this wire format before they are sent.
	maxBrowserReplRequestLineBytes = 8 * 1024 * 1024
	// Keep the HTTP envelope bounded before it is copied for strict decoding.
	maxBrowserReplBodyBytes = maxBrowserReplRequestLineBytes

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

// browserReplManager is one Browser REPL executor: it owns execution
// admission and the executor's persistent Node child. Lifecycle
// synchronization stays behind its Execute, restart, retire and Shutdown
// methods.
type browserReplManager struct {
	name string
	// socket is a named executor's socket path; the default executor reads
	// browserReplSocketPath() so tests can point it elsewhere.
	socket string
	// tabs opens a named executor's own tab; nil for the default executor,
	// which attaches to an existing page instead.
	tabs              executorTabs
	admission         chan struct{}
	lifecycle         context.Context
	stop              context.CancelCauseFunc
	child             *browserReplChild // guarded by admission
	customToolsMu     sync.RWMutex
	customToolsReplID string

	// restartPending makes the next admitted operation replace the child, for
	// a restart that found the executor busy.
	restartPending atomic.Bool

	// infoMu guards the fields below, which list and restart read without
	// admission.
	infoMu     sync.Mutex
	createdAt  time.Time
	lastUsedAt time.Time
	busy       bool
	// homeTargetID is the tab the API opened for a named executor. It is
	// recorded before any child uses it.
	homeTargetID string
	// cancelCurrent interrupts the admitted Execute call, if any.
	cancelCurrent context.CancelCauseFunc
}

// newBrowserReplManager returns the default executor.
func newBrowserReplManager() *browserReplManager {
	return newNamedBrowserReplManager(defaultBrowserReplExecutor, "", nil)
}

func newNamedBrowserReplManager(name, socket string, tabs executorTabs) *browserReplManager {
	lifecycle, stop := context.WithCancelCause(context.Background())
	admission := make(chan struct{}, 1)
	admission <- struct{}{}
	now := time.Now()
	return &browserReplManager{
		name:       name,
		socket:     socket,
		tabs:       tabs,
		admission:  admission,
		lifecycle:  lifecycle,
		stop:       stop,
		createdAt:  now,
		lastUsedAt: now,
	}
}

func (m *browserReplManager) isDefault() bool {
	return m.name == defaultBrowserReplExecutor
}

func (m *browserReplManager) socketPath() string {
	if m.socket != "" {
		return m.socket
	}
	return browserReplSocketPath()
}

func (m *browserReplManager) setCustomToolsReplID(replID string) {
	m.customToolsMu.Lock()
	defer m.customToolsMu.Unlock()
	m.customToolsReplID = replID
}

// The daemon's atomically published file is the sole discovery snapshot. A
// missing or unreadable snapshot is an error, not permission to serve stale metadata.
func (m *browserReplManager) customToolsSnapshot() (map[string]oapi.CustomWebMCPDefinition, error) {
	m.customToolsMu.RLock()
	defer m.customToolsMu.RUnlock()
	if m.customToolsReplID == "" {
		return make(map[string]oapi.CustomWebMCPDefinition), nil
	}
	data, err := os.ReadFile(browserReplCustomToolsPath())
	if err != nil {
		return nil, fmt.Errorf("read custom WebMCP discovery snapshot: %w", err)
	}
	var state struct {
		ReplID string                        `json:"repl_id"`
		Tools  []oapi.CustomWebMCPDefinition `json:"tools"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode custom WebMCP discovery snapshot: %w", err)
	}
	if state.ReplID != m.customToolsReplID {
		return nil, fmt.Errorf("custom WebMCP discovery snapshot belongs to another REPL")
	}
	if state.Tools == nil {
		return nil, fmt.Errorf("custom WebMCP discovery snapshot has no tools list")
	}
	tools := make(map[string]oapi.CustomWebMCPDefinition, len(state.Tools))
	for _, tool := range state.Tools {
		tools[tool.Id] = tool
	}
	return tools, nil
}

func browserReplCustomToolsPath() string {
	return browserReplSocketPath() + ".custom-tools.json"
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

func (m *browserReplManager) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.lifecycle.Done():
		return context.Cause(m.lifecycle)
	case <-m.admission:
	}
	if err := ctx.Err(); err != nil {
		m.release()
		return err
	}
	if m.lifecycle.Err() != nil {
		m.release()
		return context.Cause(m.lifecycle)
	}
	if m.restartPending.Swap(false) {
		m.terminateLocked(ctx, "restart")
	}
	return nil
}

func (m *browserReplManager) acquireForShutdown(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.admission:
		return nil
	}
}

func (m *browserReplManager) release() {
	m.admission <- struct{}{}
}

func (m *browserReplManager) operationContext(ctx context.Context) (context.Context, context.CancelCauseFunc, func() bool) {
	operationCtx, cancel := context.WithCancelCause(ctx)
	stopPropagation := context.AfterFunc(m.lifecycle, func() {
		cancel(context.Cause(m.lifecycle))
	})
	if cause := context.Cause(m.lifecycle); cause != nil {
		cancel(cause)
	}
	return operationCtx, cancel, stopPropagation
}

func (m *browserReplManager) Shutdown(ctx context.Context) error {
	m.stop(errBrowserReplShuttingDown)
	if err := m.acquireForShutdown(ctx); err != nil {
		return err
	}
	defer m.release()
	return m.terminateLocked(ctx, "api shutdown")
}

// ensureLocked starts the REPL child if none is running and reports whether
// it started one. If the previous child died unexpectedly it is cleared and
// replaced with a fresh REPL and fresh CUID2. The caller must hold admission.
func (m *browserReplManager) ensureLocked(ctx context.Context) (bool, error) {
	log := logger.FromContext(ctx)

	if child := m.child; child != nil {
		select {
		case err := <-child.done:
			// The wait goroutine already reaped the child; do not consume the
			// result here. Replace the channel so the value remains observable.
			log.Warn("browser REPL child exited unexpectedly; starting a fresh REPL",
				"repl_id", child.id, "exit_err", err)
			child.done = closedWaitChannel(err)
			// The group leader exited, but descendants may still be alive.
			_ = signalChildProcessGroup(child.cmd, killSignal)
			m.clearLocked(ctx, child)
		default:
			return false, nil
		}
	}

	return true, m.startLocked(ctx)
}

// closedWaitChannel returns a channel that has already received (and closed
// over) the given wait result.
func closedWaitChannel(err error) chan error {
	ch := make(chan error, 1)
	ch <- err
	return ch
}

// clearLocked detaches the child handle and removes its stale socket. The
// caller must hold admission.
func (m *browserReplManager) clearLocked(ctx context.Context, child *browserReplChild) {
	if m.child == child {
		m.child = nil
		if m.isDefault() {
			m.setCustomToolsReplID("")
			_ = os.Remove(browserReplCustomToolsPath())
		}
	}
	removeBrowserReplSocket(logger.FromContext(ctx), m.socketPath())
}

func removeBrowserReplSocket(log *slog.Logger, socketPath string) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove stale browser REPL socket", "path", socketPath, "err", err)
	}
}

// startLocked spawns a new REPL child with a fresh CUID2 and waits for its
// socket to accept connections. The caller must hold admission.
func (m *browserReplManager) startLocked(ctx context.Context) error {
	log := logger.FromContext(ctx).With("executor", m.name)
	socketPath := m.socketPath()

	// Never adopt state from a previous process. Unlink any stale socket;
	// Linux parent-death signaling handles daemons spawned by this API.
	removeBrowserReplSocket(log, socketPath)

	replID := cuid2.Generate()

	cmd := exec.Command("node", "--experimental-vm-modules", "--max-old-space-size="+browserReplHeapMB(), browserReplScriptPath())
	// The protocol lives on the socket; child output is diagnostics only.
	output := newHeapLimitWatcher(os.Stderr, log, m.name, browserReplHeapMB())
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Env = append(os.Environ(),
		"BROWSER_REPL_SOCKET="+socketPath,
		"BROWSER_REPL_ID="+replID,
		"BROWSER_REPL_EXECUTOR="+m.name,
	)
	configureChildProcessCmd(cmd)

	log.Info("starting browser REPL", "repl_id", replID, "socket", socketPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start browser REPL: %w", err)
	}

	child := &browserReplChild{id: replID, cmd: cmd, done: make(chan error, 1)}
	go func() {
		child.done <- cmd.Wait()
	}()
	m.child = child

	deadline := time.Now().Add(browserReplStartupTimeout)
	for {
		conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			if m.isDefault() {
				m.setCustomToolsReplID(replID)
			}
			log.Info("browser REPL ready", "repl_id", replID)
			return nil
		}
		select {
		case waitErr := <-child.done:
			child.done = closedWaitChannel(waitErr)
			_ = signalChildProcessGroup(child.cmd, killSignal)
			m.clearLocked(ctx, child)
			return fmt.Errorf("browser REPL exited during startup: %w", waitErr)
		case <-ctx.Done():
			m.killLocked(context.WithoutCancel(ctx), "startup cancelled")
			return context.Cause(ctx)
		default:
		}
		if time.Now().After(deadline) {
			m.terminateLocked(ctx, "startup timeout")
			return fmt.Errorf("browser REPL failed to start within %v", browserReplStartupTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// terminateLocked stops the REPL child's process group (SIGTERM,
// escalating to SIGKILL), waits for exit, removes the socket, and clears the
// in-memory handle. The next request lazily starts a fresh REPL with a new
// CUID2. Returns the child's exit error when observed (nil for a clean exit
// or when the exit could not be observed within the grace period). The caller
// must hold admission.
func (m *browserReplManager) terminateLocked(ctx context.Context, reason string) error {
	child := m.child
	if child == nil {
		return nil
	}
	log := logger.FromContext(ctx)
	log.Info("terminating browser REPL", "repl_id", child.id, "reason", reason)

	// SIGTERM the whole process group so any grandchildren go down too.
	_ = signalChildProcessGroup(child.cmd, termSignal)

	select {
	case err := <-child.done:
		child.done = closedWaitChannel(err)
		// The group leader exiting does not imply descendants honored SIGTERM.
		// Kill the process group before relinquishing ownership.
		_ = signalChildProcessGroup(child.cmd, killSignal)
		m.clearLocked(ctx, child)
		return err
	case <-time.After(browserReplShutdownGrace):
	}

	log.Warn("browser REPL did not exit on SIGTERM; escalating to SIGKILL", "repl_id", child.id)
	_ = signalChildProcessGroup(child.cmd, killSignal)

	var waitErr error
	select {
	case err := <-child.done:
		child.done = closedWaitChannel(err)
		waitErr = err
	case <-time.After(browserReplShutdownGrace):
		log.Error("browser REPL did not exit after SIGKILL", "repl_id", child.id)
	}
	// Re-signal after the leader is reaped: descendants remain members of the
	// original process group even if the leader exited first.
	_ = signalChildProcessGroup(child.cmd, killSignal)
	m.clearLocked(ctx, child)
	return waitErr
}

// killLocked SIGKILLs the REPL process group without a SIGTERM
// grace period. Use when the daemon's event loop is known to be blocked
// (e.g. an uninterruptible execution that never answered before the socket
// read deadline): a graceful signal could never be handled and would only
// add browserReplShutdownGrace of dead time to every such timeout. The caller
// must hold admission.
func (m *browserReplManager) killLocked(ctx context.Context, reason string) {
	child := m.child
	if child == nil {
		return
	}
	log := logger.FromContext(ctx)
	log.Info("killing browser REPL", "repl_id", child.id, "reason", reason)
	_ = signalChildProcessGroup(child.cmd, killSignal)

	select {
	case err := <-child.done:
		child.done = closedWaitChannel(err)
	case <-time.After(browserReplShutdownGrace):
		log.Error("browser REPL did not exit after SIGKILL", "repl_id", child.id)
	}
	_ = signalChildProcessGroup(child.cmd, killSignal)
	m.clearLocked(ctx, child)
}

// browserReplDaemonRequest is the wire format sent to the REPL daemon.
type browserReplDaemonRequest struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	TimeoutMs    int    `json:"timeout_ms,omitempty"`
	HomeTargetID string `json:"home_target_id,omitempty"`
}

// browserReplDaemonResponse is the wire format returned by the REPL daemon.
type browserReplDaemonResponse struct {
	ID               string            `json:"id"`
	ReplID           string            `json:"repl_id"`
	Success          bool              `json:"success"`
	Error            string            `json:"error,omitempty"`
	ErrorCode        string            `json:"error_code,omitempty"`
	Stack            *string           `json:"stack,omitempty"`
	Content          []json.RawMessage `json:"content,omitempty"`
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
	Exiting bool `json:"exiting,omitempty"`
	// TargetID is the tab the runtime is attached to after the execution.
	TargetID string `json:"target_id,omitempty"`
	// TabMissing reports that a named executor's tab is closed and nothing
	// is attached. The code was not run, so it is safe to send it again with
	// a new tab.
	TabMissing bool `json:"tab_missing,omitempty"`
	DurationMs int  `json:"duration_ms"`
}

// browserReplRequest is the already-encoded request sent over the daemon
// socket. Preparing it before touching the child makes the wire-size check a
// non-destructive API validation rather than a protocol failure after dialing.
type browserReplRequest struct {
	id    string
	bytes []byte
}

// prepareBrowserReplRequest encodes the daemon request with HTML escaping
// disabled. The daemon's limit applies to the line without its trailing
// newline, so the encoded request must fit before it is sent.
func prepareBrowserReplRequest(code string, timeout time.Duration, homeTargetID string) (*browserReplRequest, error) {
	id := uuid.New().String()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(browserReplDaemonRequest{
		ID:           id,
		Code:         code,
		TimeoutMs:    int(timeout.Milliseconds()),
		HomeTargetID: homeTargetID,
	}); err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if requestLineBytes := buf.Len() - 1; requestLineBytes > maxBrowserReplRequestLineBytes {
		return nil, fmt.Errorf("code too large: encoded request is %d bytes, maximum is %d", requestLineBytes, maxBrowserReplRequestLineBytes)
	}
	return &browserReplRequest{id: id, bytes: buf.Bytes()}, nil
}

type browserReplNotDispatchedError struct {
	cause error
}

func (e *browserReplNotDispatchedError) Error() string { return e.cause.Error() }
func (e *browserReplNotDispatchedError) Unwrap() error { return e.cause }

// executeLocked sends one prepared execution to the current child and reads
// its response. The returned error is a transport/protocol failure; execution
// failures are reported inside the response. The caller must hold admission.
func (m *browserReplManager) executeLocked(ctx context.Context, request *browserReplRequest, timeout time.Duration) (*browserReplDaemonResponse, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, &browserReplNotDispatchedError{cause: err}
	}
	child := m.child
	if child == nil {
		return nil, errors.New("no browser REPL child")
	}

	conn, err := net.DialTimeout("unix", m.socketPath(), 2*time.Second)
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

	if err := context.Cause(ctx); err != nil {
		return nil, &browserReplNotDispatchedError{cause: err}
	}
	if _, err := conn.Write(request.bytes); err != nil {
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
		return nil, fmt.Errorf("request context cancelled: %w", context.Cause(ctx))
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

	if resp.ID != request.id {
		return nil, fmt.Errorf("response ID mismatch: expected %s, got %s", request.id, resp.ID)
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
// content truncation flag) so clients can read them unconditionally; partial
// content is never available here because the child died without answering.
func browserReplTerminatedResponse(replID string, newState bool, err error, durationMs int) oapi.ExecuteBrowserRepl200JSONResponse {
	errMsg := err.Error()
	terminated := true
	notTruncated := false
	return oapi.ExecuteBrowserRepl200JSONResponse{
		Success:          false,
		ReplId:           replID,
		NewState:         newState,
		Error:            &errMsg,
		Terminated:       &terminated,
		ReplTerminated:   &terminated,
		DurationMs:       &durationMs,
		ContentTruncated: &notTruncated,
	}
}

// StrictBrowserReplBodyMiddleware enforces additionalProperties: false on
// Browser REPL-owned request bodies. The generated strict-server decoder
// silently drops unknown fields, so malformed extensions need an explicit
// check before dispatch.
func StrictBrowserReplBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		var probe any
		maxBytes := int64(0)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repl":
			probe = &oapi.BrowserReplRequest{}
			maxBytes = maxBrowserReplBodyBytes
		case r.Method == http.MethodPost && r.URL.Path == "/webmcp/custom-tools":
			probe = &oapi.AddCustomWebMCPToolsRequest{}
			maxBytes = maxCustomWebMCPRequestBytes
		default:
			next.ServeHTTP(w, r)
			return
		}

		limitedBody := http.MaxBytesReader(w, r.Body, maxBytes)
		body, err := io.ReadAll(limitedBody)
		_ = r.Body.Close()
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_ = json.NewEncoder(w).Encode(oapi.BadRequestError{
					Message: fmt.Sprintf("request body exceeds %d bytes", maxBytes),
				})
				return
			}
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(probe); err != nil && strings.HasPrefix(err.Error(), "json: unknown field") {
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

func (m *browserReplManager) Execute(ctx context.Context, request oapi.ExecuteBrowserReplRequestObject) (oapi.ExecuteBrowserReplResponseObject, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer m.release()

	operationCtx, cancelOperation, stopPropagation := m.operationContext(ctx)
	defer stopPropagation()
	defer m.beginOperation(ctx, cancelOperation)()
	if err := context.Cause(operationCtx); err != nil {
		return nil, err
	}
	ctx = operationCtx
	log := logger.FromContext(ctx).With("executor", m.name)

	if request.Body == nil {
		return oapi.ExecuteBrowserRepl400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "request body is required",
			},
		}, nil
	}

	reset := request.Body.Reset != nil && *request.Body.Reset
	code := request.Body.Code
	if code == "" && !reset {
		return oapi.ExecuteBrowserRepl400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "code is required (it may be empty only when reset is true)",
			},
		}, nil
	}

	timeout := 60 * time.Second
	if request.Body.TimeoutSec != nil {
		if *request.Body.TimeoutSec < browserReplMinTimeoutSec || *request.Body.TimeoutSec > browserReplMaxTimeoutSec {
			return oapi.ExecuteBrowserRepl400JSONResponse{
				BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
					Message: fmt.Sprintf("timeout_sec must be between %d and %d", browserReplMinTimeoutSec, browserReplMaxTimeoutSec),
				},
			}, nil
		}
		timeout = time.Duration(*request.Body.TimeoutSec) * time.Second
	}

	var preparedRequest *browserReplRequest
	if code != "" {
		var err error
		preparedRequest, err = prepareBrowserReplRequest(code, timeout, "")
		if err != nil {
			return oapi.ExecuteBrowserRepl400JSONResponse{
				BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
					Message: err.Error(),
				},
			}, nil
		}
	}

	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if reset {
		m.terminateLocked(ctx, "explicit reset")
	}

	newState, err := m.ensureLocked(ctx)
	if err != nil {
		if cause := browserReplInterruption(ctx); cause != nil {
			return nil, cause
		}
		log.Error("failed to start browser REPL", "error", err)
		return oapi.ExecuteBrowserRepl500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
				Message: fmt.Sprintf("failed to start browser REPL: %v", err),
			},
		}, nil
	}
	replID := m.child.id

	homeTargetID, tabCreated, err := m.ensureTab(ctx)
	if cause := browserReplInterruption(ctx); cause != nil {
		return nil, cause
	}
	if err != nil {
		log.Error("failed to open executor tab", "error", err)
		return oapi.ExecuteBrowserRepl500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
				Message: err.Error(),
			},
		}, nil
	}

	// Reset with no code: just start a fresh REPL.
	if code == "" {
		out := oapi.ExecuteBrowserRepl200JSONResponse{
			Success:  true,
			ReplId:   replID,
			NewState: newState,
		}
		if homeTargetID != "" {
			out.Tab = &oapi.BrowserReplTab{TargetId: homeTargetID, Created: tabCreated}
		}
		return out, nil
	}

	execStart := time.Now()
	resp, err := m.executeTabLocked(ctx, code, timeout, preparedRequest, homeTargetID)
	if err == nil && resp.TabMissing {
		// The executor's tab was closed and nothing is attached. The child
		// answered without running the code, so send it again with a new tab.
		if homeTargetID, err = m.openTab(ctx); err != nil {
			if cause := browserReplInterruption(ctx); cause != nil {
				return nil, cause
			}
			log.Error("failed to reopen executor tab", "error", err)
			return oapi.ExecuteBrowserRepl500JSONResponse{
				InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
					Message: err.Error(),
				},
			}, nil
		}
		tabCreated = true
		resp, err = m.executeTabLocked(ctx, code, timeout, preparedRequest, homeTargetID)
	}
	if err != nil {
		var notDispatched *browserReplNotDispatchedError
		if errors.As(err, &notDispatched) {
			return nil, notDispatched.cause
		}
		var tooLarge *browserReplRequestTooLargeError
		if errors.As(err, &tooLarge) {
			return oapi.ExecuteBrowserRepl400JSONResponse{
				BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
					Message: err.Error(),
				},
			}, nil
		}
		// Any transport or protocol failure is fatal to the child: kill the
		// process group, wait for exit, remove the stale socket, and clear the
		// handle. The next request lazily starts a fresh REPL with a new ID.
		log.Error("browser REPL execution failed; terminating child", "repl_id", replID, "error", err)
		var timeoutErr *browserReplTimeoutError
		if errors.As(err, &timeoutErr) {
			// The daemon never answered, so its event loop is blocked and a
			// graceful SIGTERM could never be handled; kill immediately.
			m.killLocked(ctx, "execution timeout")
		} else if waitErr := m.terminateLocked(ctx, "execution failure"); waitErr != nil {
			// Surface the child's exit reason (e.g. SIGKILL from the OOM
			// killer near the heap cap) instead of a bare transport error.
			err = fmt.Errorf("browser REPL process terminated during execution (%v): %w", waitErr, err)
		}
		return browserReplTerminatedResponse(replID, newState, err, int(time.Since(execStart).Milliseconds())), nil
	}

	mapped, err := browserReplMapResponse(resp)
	if err != nil {
		// A response that does not decode into the public schema is protocol
		// corruption; do not risk state from this child.
		log.Error("browser REPL returned an undecodable response; terminating child", "repl_id", replID, "error", err)
		m.terminateLocked(ctx, "protocol corruption")
		return browserReplTerminatedResponse(replID, newState, err, int(time.Since(execStart).Milliseconds())), nil
	}
	mapped.NewState = newState
	attached := resp.TargetID
	if attached == "" && tabCreated {
		attached = homeTargetID
	}
	if attached != "" {
		mapped.Tab = &oapi.BrowserReplTab{TargetId: attached, Created: tabCreated}
	}

	if resp.TimedOut || resp.Exiting {
		if resp.Exiting {
			// The daemon hit an uncaught exception, answered this execution
			// with the exception details, and is exiting non-zero (resuming
			// after an uncaught exception is unsafe per Node semantics).
			// Reap the child and report termination so the state loss is
			// explicit; the next request lazily starts a fresh REPL.
			log.Warn("browser REPL reported an uncaught exception and is exiting; terminating child", "repl_id", replID)
			m.terminateLocked(ctx, "uncaught exception in REPL process")
		} else {
			// A timeout is destructive: the daemon only abandoned the
			// execution, so its code is still running inside the child. Kill
			// the process group, wait for exit, and clear the handle; the next
			// request lazily starts a fresh REPL with a new CUID2. The
			// response carries the terminated ID, terminated: true, and the
			// partial content the execution produced before the deadline.
			log.Warn("browser REPL execution timed out; terminating child", "repl_id", replID)
			m.terminateLocked(ctx, "execution timeout")
		}
		terminated := true
		mapped.Terminated = &terminated
		mapped.ReplTerminated = &terminated
	}
	return mapped, nil
}

// beginOperation marks an admitted operation as the executor's current one,
// so restart can interrupt it, and returns the function that ends it. A
// restart that arrived before the operation registered, or while it ran,
// replaces the child. The caller must hold admission until the returned
// function has run.
func (m *browserReplManager) beginOperation(ctx context.Context, cancel context.CancelCauseFunc) func() {
	m.infoMu.Lock()
	m.busy = true
	m.lastUsedAt = time.Now()
	m.cancelCurrent = cancel
	// restart sets restartPending and reads cancelCurrent under infoMu, so it
	// either cancels this operation or leaves the restart for it to apply.
	restartPending := m.restartPending.Swap(false)
	m.infoMu.Unlock()
	if restartPending {
		m.terminateLocked(ctx, "restart")
	}
	return func() {
		m.infoMu.Lock()
		m.busy = false
		m.cancelCurrent = nil
		m.infoMu.Unlock()
		cancel(nil)
		// A removed executor's retire may have stopped waiting for this
		// operation, so the operation ends the child itself.
		if m.restartPending.Swap(false) || m.lifecycle.Err() != nil {
			m.terminateLocked(context.WithoutCancel(ctx), "restart or removal")
		}
	}
}

// browserReplInterruption returns the delete or restart that ended a call, if
// one did. Such calls fail like calls interrupted while queued, not as REPL
// start failures.
func browserReplInterruption(ctx context.Context) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, errBrowserReplDeleted) || errors.Is(cause, errBrowserReplRestarted) {
		return cause
	}
	return nil
}

type browserReplRequestTooLargeError struct {
	err error
}

func (e *browserReplRequestTooLargeError) Error() string { return e.err.Error() }

// executeTabLocked runs code bound to a named executor's tab. The default
// executor sends the request prepared during validation. The caller must hold
// admission.
func (m *browserReplManager) executeTabLocked(ctx context.Context, code string, timeout time.Duration, prepared *browserReplRequest, homeTargetID string) (*browserReplDaemonResponse, error) {
	if homeTargetID != "" {
		var err error
		if prepared, err = prepareBrowserReplRequest(code, timeout, homeTargetID); err != nil {
			return nil, &browserReplRequestTooLargeError{err: err}
		}
	}
	return m.executeLocked(ctx, prepared, timeout)
}

// ensureTab returns a named executor's tab, opening one the first time, and
// reports whether it opened one. The default executor has no tab. The caller
// must hold admission.
func (m *browserReplManager) ensureTab(ctx context.Context) (string, bool, error) {
	if m.tabs == nil {
		return "", false, nil
	}
	m.infoMu.Lock()
	targetID := m.homeTargetID
	m.infoMu.Unlock()
	if targetID != "" {
		return targetID, false, nil
	}
	targetID, err := m.openTab(ctx)
	return targetID, true, err
}

// openTab opens a new tab for a named executor and records it before any
// child uses it, so DELETE and shutdown can always close it. A delete or
// restart does not cancel the open, so a tab Chromium creates is always
// recorded. The caller must hold admission.
func (m *browserReplManager) openTab(ctx context.Context) (string, error) {
	targetID, err := m.tabs.Open(context.WithoutCancel(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to open executor tab: %w", err)
	}
	if cause := context.Cause(m.lifecycle); cause != nil {
		// The executor was removed while the tab opened, and its retire may
		// already have stopped waiting for this call; close the tab here.
		if err := m.tabs.Close(context.WithoutCancel(ctx), targetID); err != nil {
			logger.FromContext(ctx).Warn("failed to close executor tab", "executor", m.name, "target_id", targetID, "error", err)
		}
		return "", cause
	}
	m.infoMu.Lock()
	m.homeTargetID = targetID
	m.infoMu.Unlock()
	return targetID, nil
}

// restart replaces the default executor's child without waiting for queued
// calls: the call running on it fails, and the next admitted call starts a
// fresh child.
func (m *browserReplManager) restart(ctx context.Context) {
	m.infoMu.Lock()
	m.restartPending.Store(true)
	cancel := m.cancelCurrent
	m.infoMu.Unlock()
	if cancel != nil {
		cancel(errBrowserReplRestarted)
		return
	}
	// Idle: replace the child now. If a call is admitted but not yet
	// registered, restartPending makes it start from a fresh child.
	select {
	case <-m.admission:
		if m.restartPending.Swap(false) {
			m.terminateLocked(ctx, "restart")
		}
		m.release()
	default:
	}
}

// retire stops a removed named executor for good: the call running on it and
// queued calls fail with cause, and the child is terminated. It returns the
// executor's tab, which was recorded before any child used it.
func (m *browserReplManager) retire(ctx context.Context, cause error) string {
	m.stop(cause)
	if err := m.acquireForShutdown(ctx); err == nil {
		m.terminateLocked(ctx, "executor removed")
		m.release()
	}
	m.infoMu.Lock()
	defer m.infoMu.Unlock()
	return m.homeTargetID
}

// browserReplMapResponse converts a daemon response into the public API
// shape, decoding typed content items through the generated union.
func browserReplMapResponse(resp *browserReplDaemonResponse) (oapi.ExecuteBrowserRepl200JSONResponse, error) {
	out := oapi.ExecuteBrowserRepl200JSONResponse{
		Success:          resp.Success,
		ReplId:           resp.ReplID,
		Stack:            resp.Stack,
		ContentTruncated: &resp.ContentTruncated,
		DurationMs:       &resp.DurationMs,
	}

	if resp.Error != "" {
		out.Error = &resp.Error
	}

	if resp.Content != nil {
		content := make([]oapi.BrowserReplContent, 0, len(resp.Content))
		for i, raw := range resp.Content {
			var item oapi.BrowserReplContent
			if err := json.Unmarshal(raw, &item); err != nil {
				return out, fmt.Errorf("failed to decode content item %d: %w", i, err)
			}
			content = append(content, item)
		}
		out.Content = &content
	}

	return out, nil
}
