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
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	// defaultPlaywrightExecutor runs calls that name no executor. It always
	// exists, binds `page` to the active tab instead of owning one, and does
	// not count toward maxPlaywrightExecutors.
	defaultPlaywrightExecutor = "default"

	maxPlaywrightExecutors = 8

	// playwrightExecutorResponseGrace is added to the execution timeout when
	// setting the socket deadline, giving the daemon time to report its own
	// timeout before the API treats the process as unresponsive.
	playwrightExecutorResponseGrace = 5 * time.Second

	// playwrightExecutorKillGrace bounds how long a kill waits for the
	// process to be reaped.
	playwrightExecutorKillGrace = 3 * time.Second
)

var (
	playwrightExecutorNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	errPlaywrightExecutorDeleted   = errors.New("executor was deleted")
	errPlaywrightExecutorRestarted = errors.New("executor was restarted")
	errPlaywrightExecutorsShutDown = errors.New("playwright executors are shutting down")
	errPlaywrightExecutorNotFound  = errors.New("executor not found")
	errPlaywrightExecutorSetup     = errors.New("failed to set up playwright executor")
)

type playwrightExecutorLimitError struct {
	executors []playwrightExecutorInfo
}

func (e *playwrightExecutorLimitError) Error() string {
	return fmt.Sprintf("named executor limit (%d) reached; delete one with DELETE /playwright/executors/{name}", maxPlaywrightExecutors)
}

// playwrightExecutorTabs opens and closes executor tabs. The manager owns
// every executor tab, so a tab's ID is recorded before any daemon process can
// use it and DELETE or shutdown can always find it.
type playwrightExecutorTabs interface {
	Open(ctx context.Context) (string, error)
	Close(ctx context.Context, targetID string) error
}

// cdpPlaywrightExecutorTabs manages executor tabs over the browser's DevTools
// connection.
type cdpPlaywrightExecutorTabs struct {
	withCDP func(context.Context, func(context.Context, *cdpclient.Client) error) error
}

// Open creates a background tab in the default browser context, so it does
// not take focus from the live view or change the active tab that the default
// executor binds to.
func (t cdpPlaywrightExecutorTabs) Open(ctx context.Context) (string, error) {
	var targetID string
	err := t.withCDP(ctx, func(ctx context.Context, c *cdpclient.Client) error {
		var err error
		targetID, err = c.CreateTarget(ctx, "about:blank", true)
		return err
	})
	return targetID, err
}

func (t cdpPlaywrightExecutorTabs) Close(ctx context.Context, targetID string) error {
	return t.withCDP(ctx, func(ctx context.Context, c *cdpclient.Client) error {
		return c.CloseTarget(ctx, targetID)
	})
}

// playwrightExecutorChild is one executor's daemon process.
type playwrightExecutorChild struct {
	cmd      *exec.Cmd
	socket   string
	exited   chan struct{} // closed once cmd.Wait returns
	killOnce sync.Once
	// unwatch stops the executor's life from killing this process; call it
	// when the process is replaced while the executor lives on.
	unwatch func() bool
}

func (c *playwrightExecutorChild) alive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

// kill SIGKILLs the child's process group, waits for it to be reaped, and
// removes its socket. The group is signaled once, even if the leader already
// exited, so descendants are not left behind; later calls do nothing, so a
// reused process group ID is never signaled.
func (c *playwrightExecutorChild) kill() {
	c.killOnce.Do(func() {
		_ = signalChildProcessGroup(c.cmd, killSignal)
		select {
		case <-c.exited:
		case <-time.After(playwrightExecutorKillGrace):
		}
		_ = os.Remove(c.socket)
	})
}

// playwrightExecutor is a named execution lane. admission serializes calls on
// it; every other field is guarded by the manager's mu.
type playwrightExecutor struct {
	// life ends when the executor is deleted or shut down, or, for the
	// default executor, restarted. Ending it kills the process started under
	// it, and its cause is the error that calls admitted under it fail with.
	// A restart gives the default executor a new life.
	life context.Context
	end  context.CancelCauseFunc

	name string
	// pinned marks the default executor: it is never removed, owns no tab,
	// and binds `page` to the active tab.
	pinned     bool
	createdAt  time.Time
	admission  chan struct{}
	lastUsedAt time.Time
	busy       bool
	targetID   string
	child      *playwrightExecutorChild
}

// playwrightExecutorInfo is a point-in-time copy of an executor's listed
// fields, safe to read without the manager's mu.
type playwrightExecutorInfo struct {
	name       string
	pinned     bool
	createdAt  time.Time
	lastUsedAt time.Time
	busy       bool
	targetID   string
}

// playwrightExecutorManager owns the named executors behind
// POST /playwright/execute. Each executor runs in its own daemon process, so
// calls on different executors run concurrently and a crash only takes down
// that executor's process. The manager opens each executor's tab
// and keeps its target ID, so a replacement process binds `page` to the same
// tab.
type playwrightExecutorManager struct {
	script        string
	socketDir     string
	responseGrace time.Duration
	tabs          playwrightExecutorTabs

	mu        sync.Mutex
	executors map[string]*playwrightExecutor
	closed    bool
}

func newPlaywrightExecutorManager(tabs playwrightExecutorTabs) *playwrightExecutorManager {
	m := &playwrightExecutorManager{
		tabs:          tabs,
		script:        playwrightDaemonScript,
		socketDir:     os.TempDir(),
		responseGrace: playwrightExecutorResponseGrace,
		executors:     make(map[string]*playwrightExecutor),
	}
	e := newPlaywrightExecutor(defaultPlaywrightExecutor)
	e.pinned = true
	m.executors[e.name] = e
	return m
}

func newPlaywrightExecutor(name string) *playwrightExecutor {
	now := time.Now()
	e := &playwrightExecutor{
		name:       name,
		createdAt:  now,
		lastUsedAt: now,
		admission:  make(chan struct{}, 1),
	}
	e.life, e.end = context.WithCancelCause(context.Background())
	e.admission <- struct{}{}
	return e
}

// executorFor returns the named executor, creating it if there is room.
func (m *playwrightExecutorManager) executorFor(name string) (*playwrightExecutor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errPlaywrightExecutorsShutDown
	}
	if e, ok := m.executors[name]; ok {
		return e, nil
	}
	if m.namedCountLocked() >= maxPlaywrightExecutors {
		return nil, &playwrightExecutorLimitError{executors: m.snapshotLocked()}
	}
	e := newPlaywrightExecutor(name)
	m.executors[name] = e
	return e, nil
}

// namedCountLocked counts the executors that take a slot. The caller must hold
// mu.
func (m *playwrightExecutorManager) namedCountLocked() int {
	n := 0
	for _, e := range m.executors {
		if !e.pinned {
			n++
		}
	}
	return n
}

// Execute runs code on the named executor, creating the executor, starting its
// process and opening its tab as needed. A failure after the call was handed to
// the process comes back as an unsuccessful response that still reports the
// executor's tab.
func (m *playwrightExecutorManager) Execute(ctx context.Context, name, code string, timeout time.Duration) (*playwrightDaemonResponse, error) {
	e, err := m.executorFor(name)
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.admission:
	}
	defer func() { e.admission <- struct{}{} }()

	m.mu.Lock()
	life := e.life
	if cause := context.Cause(life); cause != nil {
		m.mu.Unlock()
		return nil, cause
	}
	e.busy = true
	e.lastUsedAt = time.Now()
	targetID := e.targetID
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		e.busy = false
		m.mu.Unlock()
	}()

	child, err := m.ensureChild(ctx, e, life)
	if err != nil {
		return nil, err
	}

	created := false
	if targetID == "" && !e.pinned {
		if targetID, err = m.openTab(ctx, e); err != nil {
			return nil, err
		}
		created = true
	}
	resp, err := m.send(child, e, code, targetID, created, timeout)
	if err == nil && resp.TabMissing && !e.pinned {
		// The tab was closed since the last call. The daemon answered without
		// running the code, so it is safe to send it again with a new tab.
		if targetID, err = m.openTab(ctx, e); err != nil {
			return nil, err
		}
		created = true
		resp, err = m.send(child, e, code, targetID, created, timeout)
	}

	// An unresponsive or dead process is replaced on the next call. After a
	// reported timeout the process is kept: the daemon drops its browser
	// connection, so abandoned code cannot keep driving the browser.
	if err != nil {
		m.mu.Lock()
		if e.child == child {
			e.child = nil
		}
		m.mu.Unlock()
		child.unwatch()
		child.kill()
	}

	if cause := context.Cause(life); cause != nil {
		return nil, cause
	}
	if err != nil {
		logger.FromContext(ctx).Error("playwright executor call failed", "executor", name, "error", err)
		return &playwrightDaemonResponse{
			Success:    false,
			Error:      fmt.Sprintf("execution failed: %v", err),
			TargetID:   targetID,
			TabCreated: created,
		}, nil
	}
	return resp, nil
}

// ensureChild returns the executor's live process, starting a new one if
// needed. The process is killed when life ends, including when it ends while
// the process is starting. The caller must hold the executor's admission.
func (m *playwrightExecutorManager) ensureChild(ctx context.Context, e *playwrightExecutor, life context.Context) (*playwrightExecutorChild, error) {
	m.mu.Lock()
	child := e.child
	m.mu.Unlock()
	if child != nil && child.alive() {
		return child, nil
	}
	if child != nil {
		child.unwatch()
		child.kill()
	}

	child, err := m.start(ctx, e.name)
	if err != nil {
		m.mu.Lock()
		// A named executor whose process never started has no tab yet; drop it
		// so it does not take one of the limited slots.
		if !e.pinned && e.targetID == "" && m.executors[e.name] == e {
			delete(m.executors, e.name)
			e.end(err)
		}
		m.mu.Unlock()
		return nil, err
	}
	child.unwatch = context.AfterFunc(life, child.kill)

	m.mu.Lock()
	cause := context.Cause(life)
	if cause == nil {
		e.child = child
	}
	m.mu.Unlock()
	if cause != nil {
		return nil, cause
	}
	return child, nil
}

// openTab opens a new tab for the executor and records it before any process
// uses it. The caller must hold the executor's admission.
func (m *playwrightExecutorManager) openTab(ctx context.Context, e *playwrightExecutor) (string, error) {
	targetID, err := m.tabs.Open(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: open tab: %w", errPlaywrightExecutorSetup, err)
	}
	m.mu.Lock()
	e.targetID = targetID
	m.mu.Unlock()
	return targetID, nil
}

func (m *playwrightExecutorManager) start(ctx context.Context, name string) (*playwrightExecutorChild, error) {
	log := logger.FromContext(ctx)
	socket := filepath.Join(m.socketDir, "playwright-executor-"+uuid.NewString()+".sock")

	cmd := exec.Command("node", m.script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_DAEMON_SOCKET="+socket)
	configureChildProcessCmd(cmd)

	log.Info("starting playwright executor", "executor", name)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %w", errPlaywrightExecutorSetup, err)
	}
	child := &playwrightExecutorChild{cmd: cmd, socket: socket, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(child.exited)
	}()

	deadline := time.Now().Add(playwrightDaemonStartup)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond); err == nil {
			conn.Close()
			return child, nil
		}
		if !child.alive() {
			child.kill()
			return nil, fmt.Errorf("%w: process exited during startup", errPlaywrightExecutorSetup)
		}
		time.Sleep(50 * time.Millisecond)
	}
	child.kill()
	return nil, fmt.Errorf("%w: not ready within %v", errPlaywrightExecutorSetup, playwrightDaemonStartup)
}

// send runs one call on the child, bound to the given tab. created tells the
// daemon the tab was just opened, so it waits longer for it to appear. Calls
// on the default executor carry no executor name, so the daemon binds the
// active tab.
func (m *playwrightExecutorManager) send(child *playwrightExecutorChild, e *playwrightExecutor, code, targetID string, created bool, timeout time.Duration) (*playwrightDaemonResponse, error) {
	conn, err := net.DialTimeout("unix", child.socket, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to executor: %w", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout + m.responseGrace)); err != nil {
		return nil, fmt.Errorf("failed to set deadline: %w", err)
	}

	reqID := uuid.NewString()
	req := playwrightDaemonRequest{
		ID:         reqID,
		Code:       code,
		TimeoutMs:  int(timeout.Milliseconds()),
		TargetID:   targetID,
		TabCreated: created,
	}
	if !e.pinned {
		req.Executor = e.name
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if _, err := conn.Write(append(reqBytes, '\n')); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	respLine, err := bufio.NewReader(conn).ReadBytes('\n')
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

// Delete removes the named executor and kills its process. It returns the
// executor's tab target ID, if it had one, so the caller can close the tab.
// The default executor is restarted instead: its process is killed without
// waiting for queued calls, the call it interrupts fails, and the next call
// starts a new process.
func (m *playwrightExecutorManager) Delete(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	e, ok := m.executors[name]
	if !ok {
		m.mu.Unlock()
		return "", errPlaywrightExecutorNotFound
	}
	if e.pinned {
		e.end(errPlaywrightExecutorRestarted)
		e.life, e.end = context.WithCancelCause(context.Background())
		e.child = nil
		m.mu.Unlock()
		return "", nil
	}
	delete(m.executors, name)
	e.end(errPlaywrightExecutorDeleted)
	m.mu.Unlock()

	return m.retire(ctx, e), nil
}

// retire waits for a removed executor's process to die and its in-flight call
// to unwind, then returns its tab target ID, if any. Waiting for the call
// includes a tab that call opened: a call records a tab before handing it to
// the process. The caller must already have removed the executor and ended
// its life, which kills the process.
func (m *playwrightExecutorManager) retire(ctx context.Context, e *playwrightExecutor) string {
	select {
	case <-e.admission:
		e.admission <- struct{}{}
	case <-ctx.Done():
	}

	m.mu.Lock()
	child, targetID := e.child, e.targetID
	m.mu.Unlock()
	if child != nil {
		child.kill()
	}
	return targetID
}

// List returns the executors, the default executor first and the rest oldest
// first.
func (m *playwrightExecutorManager) List() []playwrightExecutorInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked copies the executors so callers can read them without mu.
// The caller must hold mu.
func (m *playwrightExecutorManager) snapshotLocked() []playwrightExecutorInfo {
	executors := make([]playwrightExecutorInfo, 0, len(m.executors))
	for _, e := range m.executors {
		executors = append(executors, playwrightExecutorInfo{
			name:       e.name,
			pinned:     e.pinned,
			createdAt:  e.createdAt,
			lastUsedAt: e.lastUsedAt,
			busy:       e.busy,
			targetID:   e.targetID,
		})
	}
	slices.SortFunc(executors, func(a, b playwrightExecutorInfo) int {
		if a.pinned != b.pinned {
			if a.pinned {
				return -1
			}
			return 1
		}
		return a.createdAt.Compare(b.createdAt)
	})
	return executors
}

// Shutdown kills every executor process and closes the executors' tabs, which
// a later API process would have no way to reach.
func (m *playwrightExecutorManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	executors := make([]*playwrightExecutor, 0, len(m.executors))
	for _, e := range m.executors {
		e.end(errPlaywrightExecutorsShutDown)
		executors = append(executors, e)
	}
	m.executors = make(map[string]*playwrightExecutor)
	m.mu.Unlock()

	// Retiring and closing get separate budgets, so slow process exits cannot
	// use up the time needed to close the tabs they leave behind.
	retireCtx, cancelRetire := context.WithTimeout(ctx, playwrightExecutorKillGrace)
	defer cancelRetire()
	targetIDs := make([]string, 0, len(executors))
	for _, e := range executors {
		if targetID := m.retire(retireCtx, e); targetID != "" {
			targetIDs = append(targetIDs, targetID)
		}
	}

	closeCtx, cancelClose := context.WithTimeout(ctx, playwrightExecutorKillGrace)
	defer cancelClose()
	for _, targetID := range targetIDs {
		if err := m.tabs.Close(closeCtx, targetID); err != nil {
			logger.FromContext(ctx).Warn("failed to close playwright executor tab", "target_id", targetID, "error", err)
		}
	}
}

// playwrightExecutorsJSON converts executors to API objects, adding the
// current URL of each executor's tab when the browser can report it.
func (s *ApiService) playwrightExecutorsJSON(ctx context.Context, executors []playwrightExecutorInfo) []oapi.PlaywrightExecutor {
	var urls map[string]string
	if slices.ContainsFunc(executors, func(e playwrightExecutorInfo) bool { return e.targetID != "" }) {
		err := s.withCDPClient(ctx, func(ctx context.Context, c *cdpclient.Client) error {
			var err error
			urls, err = c.TargetURLs(ctx)
			return err
		})
		if err != nil {
			logger.FromContext(ctx).Warn("failed to read playwright executor tab URLs", "error", err)
		}
	}

	out := make([]oapi.PlaywrightExecutor, 0, len(executors))
	for _, e := range executors {
		item := oapi.PlaywrightExecutor{
			Name:       e.name,
			Busy:       e.busy,
			CreatedAt:  e.createdAt,
			LastUsedAt: e.lastUsedAt,
		}
		if e.targetID != "" {
			targetID := e.targetID
			item.TargetId = &targetID
			if url, ok := urls[e.targetID]; ok {
				item.Url = &url
			}
		}
		out = append(out, item)
	}
	return out
}

func (s *ApiService) ListPlaywrightExecutors(ctx context.Context, _ oapi.ListPlaywrightExecutorsRequestObject) (oapi.ListPlaywrightExecutorsResponseObject, error) {
	return oapi.ListPlaywrightExecutors200JSONResponse{
		Executors: s.playwrightExecutorsJSON(ctx, s.playwrightExecutors.List()),
	}, nil
}

func (s *ApiService) DeletePlaywrightExecutor(ctx context.Context, request oapi.DeletePlaywrightExecutorRequestObject) (oapi.DeletePlaywrightExecutorResponseObject, error) {
	log := logger.FromContext(ctx)

	if !playwrightExecutorNamePattern.MatchString(request.Name) {
		return oapi.DeletePlaywrightExecutor400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "executor name must match " + playwrightExecutorNamePattern.String(),
			},
		}, nil
	}

	// The default executor owns no tab, so close_tab has nothing to act on.
	targetID, err := s.playwrightExecutors.Delete(ctx, request.Name)
	if errors.Is(err, errPlaywrightExecutorNotFound) {
		return oapi.DeletePlaywrightExecutor404JSONResponse{
			NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{
				Message: fmt.Sprintf("executor %q not found", request.Name),
			},
		}, nil
	}

	closeTab := request.Params.CloseTab == nil || *request.Params.CloseTab
	if closeTab && targetID != "" {
		if err := s.playwrightExecutors.tabs.Close(ctx, targetID); err != nil {
			log.Error("failed to close playwright executor tab", "executor", request.Name, "error", err)
			return oapi.DeletePlaywrightExecutor500JSONResponse{
				InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
					Message: fmt.Sprintf("executor deleted, but closing its tab %s failed: %v", targetID, err),
				},
			}, nil
		}
	}

	return oapi.DeletePlaywrightExecutor204Response{}, nil
}
