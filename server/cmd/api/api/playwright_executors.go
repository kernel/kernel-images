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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	maxPlaywrightExecutors = 8

	// playwrightExecutorResponseGrace is added to the execution timeout when
	// setting the socket deadline, matching the default lane.
	playwrightExecutorResponseGrace = 5 * time.Second

	// playwrightExecutorKillGrace bounds how long a kill waits for the
	// process to be reaped.
	playwrightExecutorKillGrace = 3 * time.Second
)

var (
	playwrightExecutorNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	errPlaywrightExecutorDeleted  = errors.New("executor was deleted")
	errPlaywrightExecutorNotFound = errors.New("executor not found")
	errPlaywrightExecutorStart    = errors.New("failed to start playwright executor")
)

type playwrightExecutorLimitError struct {
	executors []*playwrightExecutor
}

func (e *playwrightExecutorLimitError) Error() string {
	return fmt.Sprintf("executor limit (%d) reached; delete one with DELETE /playwright/executors/{name}", maxPlaywrightExecutors)
}

// playwrightExecutorChild is one executor's daemon process.
type playwrightExecutorChild struct {
	cmd      *exec.Cmd
	socket   string
	exited   chan struct{} // closed once cmd.Wait returns
	killOnce sync.Once
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
		_ = signalBrowserReplGroup(c.cmd, killSignal)
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
	name       string
	createdAt  time.Time
	admission  chan struct{}
	lastUsedAt time.Time
	busy       bool
	targetID   string
	child      *playwrightExecutorChild
	deleted    bool
}

// playwrightExecutorManager owns the named executors behind
// POST /playwright/execute. Each executor runs in its own daemon process, so
// calls on different executors run concurrently and a timeout or crash only
// takes down that executor's process. The executor's tab target ID lives
// here, so a replacement process binds `page` to the same tab.
type playwrightExecutorManager struct {
	script        string
	socketDir     string
	responseGrace time.Duration

	mu        sync.Mutex
	executors map[string]*playwrightExecutor
}

func newPlaywrightExecutorManager() *playwrightExecutorManager {
	return &playwrightExecutorManager{
		script:        playwrightDaemonScript,
		socketDir:     os.TempDir(),
		responseGrace: playwrightExecutorResponseGrace,
		executors:     make(map[string]*playwrightExecutor),
	}
}

// executorFor returns the named executor, creating it if there is room.
func (m *playwrightExecutorManager) executorFor(name string) (*playwrightExecutor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.executors[name]; ok {
		return e, nil
	}
	if len(m.executors) >= maxPlaywrightExecutors {
		return nil, &playwrightExecutorLimitError{executors: m.snapshotLocked()}
	}
	now := time.Now()
	e := &playwrightExecutor{
		name:       name,
		createdAt:  now,
		lastUsedAt: now,
		admission:  make(chan struct{}, 1),
	}
	e.admission <- struct{}{}
	m.executors[name] = e
	return e, nil
}

// Execute runs code on the named executor, creating the executor and starting
// its process as needed.
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
	if e.deleted {
		m.mu.Unlock()
		return nil, errPlaywrightExecutorDeleted
	}
	e.busy = true
	e.lastUsedAt = time.Now()
	child := e.child
	targetID := e.targetID
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		e.busy = false
		m.mu.Unlock()
	}()

	if child == nil || !child.alive() {
		if child != nil {
			child.kill()
		}
		child, err = m.start(ctx, name)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		deleted := e.deleted
		if !deleted {
			e.child = child
		}
		m.mu.Unlock()
		if deleted {
			child.kill()
			return nil, errPlaywrightExecutorDeleted
		}
	}

	resp, err := m.send(child, name, code, targetID, timeout, func(targetID string) {
		m.mu.Lock()
		e.targetID = targetID
		m.mu.Unlock()
	})

	m.mu.Lock()
	deleted := e.deleted
	if resp != nil && resp.TargetID != "" {
		e.targetID = resp.TargetID
	}
	// The daemon cannot interrupt abandoned user code, so a timed-out or
	// unresponsive process is replaced on the next call.
	replace := err != nil || (resp != nil && resp.TimedOut)
	if replace && e.child == child {
		e.child = nil
	}
	m.mu.Unlock()

	if replace {
		child.kill()
	}
	if deleted {
		return nil, errPlaywrightExecutorDeleted
	}
	return resp, err
}

func (m *playwrightExecutorManager) start(ctx context.Context, name string) (*playwrightExecutorChild, error) {
	log := logger.FromContext(ctx)
	socket := filepath.Join(m.socketDir, "playwright-executor-"+uuid.NewString()+".sock")

	cmd := exec.Command("node", m.script)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_DAEMON_SOCKET="+socket)
	configureBrowserReplCmd(cmd)

	log.Info("starting playwright executor", "executor", name)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %w", errPlaywrightExecutorStart, err)
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
			return nil, fmt.Errorf("%w: process exited during startup", errPlaywrightExecutorStart)
		}
		time.Sleep(50 * time.Millisecond)
	}
	child.kill()
	return nil, fmt.Errorf("%w: not ready within %v", errPlaywrightExecutorStart, playwrightDaemonStartup)
}

// send runs one call on the child. onTabCreated receives the target ID of a tab
// the call opens as soon as the daemon reports it, before user code runs, so
// the tab is known even if the call never returns a final response.
func (m *playwrightExecutorManager) send(child *playwrightExecutorChild, name, code, targetID string, timeout time.Duration, onTabCreated func(string)) (*playwrightDaemonResponse, error) {
	conn, err := net.DialTimeout("unix", child.socket, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to executor: %w", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout + m.responseGrace)); err != nil {
		return nil, fmt.Errorf("failed to set deadline: %w", err)
	}

	reqID := uuid.NewString()
	reqBytes, err := json.Marshal(playwrightDaemonRequest{
		ID:        reqID,
		Code:      code,
		TimeoutMs: int(timeout.Milliseconds()),
		Executor:  name,
		TargetID:  targetID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	if _, err := conn.Write(append(reqBytes, '\n')); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	reader := bufio.NewReader(conn)
	for {
		respLine, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}
		var resp struct {
			playwrightDaemonResponse
			Tab *struct {
				TargetID string `json:"target_id"`
			} `json:"tab"`
		}
		if err := json.Unmarshal(respLine, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}
		if resp.ID != reqID {
			return nil, fmt.Errorf("response ID mismatch: expected %s, got %s", reqID, resp.ID)
		}
		if resp.Tab != nil {
			onTabCreated(resp.Tab.TargetID)
			continue
		}
		return &resp.playwrightDaemonResponse, nil
	}
}

// Delete removes the named executor and kills its process. It returns the
// executor's tab target ID, if it had one, so the caller can close the tab.
func (m *playwrightExecutorManager) Delete(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	e, ok := m.executors[name]
	if !ok {
		m.mu.Unlock()
		return "", errPlaywrightExecutorNotFound
	}
	delete(m.executors, name)
	e.deleted = true
	child := e.child
	e.child = nil
	m.mu.Unlock()

	if child != nil {
		child.kill()
	}

	// Wait for an in-flight call to unwind, so a tab it opened is included.
	// Killing the process makes it return promptly.
	select {
	case <-e.admission:
		e.admission <- struct{}{}
	case <-ctx.Done():
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return e.targetID, nil
}

// List returns the open executors, oldest first.
func (m *playwrightExecutorManager) List() []*playwrightExecutor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked copies the executors so callers can read them without mu.
// The caller must hold mu.
func (m *playwrightExecutorManager) snapshotLocked() []*playwrightExecutor {
	executors := make([]*playwrightExecutor, 0, len(m.executors))
	for _, e := range m.executors {
		executors = append(executors, &playwrightExecutor{
			name:       e.name,
			createdAt:  e.createdAt,
			lastUsedAt: e.lastUsedAt,
			busy:       e.busy,
			targetID:   e.targetID,
		})
	}
	slices.SortFunc(executors, func(a, b *playwrightExecutor) int {
		return a.createdAt.Compare(b.createdAt)
	})
	return executors
}

// Shutdown kills every executor process.
func (m *playwrightExecutorManager) Shutdown() {
	m.mu.Lock()
	children := make([]*playwrightExecutorChild, 0, len(m.executors))
	for _, e := range m.executors {
		e.deleted = true
		if e.child != nil {
			children = append(children, e.child)
			e.child = nil
		}
	}
	m.executors = make(map[string]*playwrightExecutor)
	m.mu.Unlock()

	for _, child := range children {
		child.kill()
	}
}

// playwrightExecutorsJSON converts executors to API objects, adding the
// current URL of each executor's tab when the browser can report it.
func (s *ApiService) playwrightExecutorsJSON(ctx context.Context, executors []*playwrightExecutor) []oapi.PlaywrightExecutor {
	urls := make(map[string]string)
	if len(executors) > 0 {
		err := s.withCDPClient(ctx, func(ctx context.Context, c *cdpclient.Client) error {
			raw, err := c.Send(ctx, "Target.getTargets", nil, "")
			if err != nil {
				return err
			}
			var targets struct {
				TargetInfos []struct {
					TargetID string `json:"targetId"`
					URL      string `json:"url"`
				} `json:"targetInfos"`
			}
			if err := json.Unmarshal(raw, &targets); err != nil {
				return err
			}
			for _, t := range targets.TargetInfos {
				urls[t.TargetID] = t.URL
			}
			return nil
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
		err := s.withCDPClient(ctx, func(ctx context.Context, c *cdpclient.Client) error {
			_, err := c.Send(ctx, "Target.closeTarget", map[string]any{"targetId": targetID}, "")
			return err
		})
		// A tab that is already gone has nothing left to close.
		if err != nil && !strings.Contains(err.Error(), "No target with given id") {
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
