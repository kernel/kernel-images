package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePlaywrightDaemon speaks the daemon's socket protocol. Code is a command:
// "sleep:<ms>" resolves after ms (reporting timed_out when it exceeds
// timeout_ms, like the real daemon), "hang" blocks the event loop and "exit"
// kills the process. FAKE_START_DELAY_MS delays listening on the socket. A tab
// exists while its file exists in FAKE_TABS_DIR (see
// fakeExecutorTabs); a missing tab is answered with tab_missing. Every result
// carries the process ID so tests can tell when a process is replaced.
const fakePlaywrightDaemon = `
const fs = require('fs');
const net = require('net');
const path = require('path');
const socketPath = process.env.PLAYWRIGHT_DAEMON_SOCKET;
const server = net.createServer(socket => {
  let buffer = '';
  socket.on('data', data => {
    buffer += data;
    let i;
    while ((i = buffer.indexOf('\n')) !== -1) {
      const req = JSON.parse(buffer.slice(0, i));
      buffer = buffer.slice(i + 1);
      if (!req.executor) {
        req.target_id = 'active-tab';
      } else if (!fs.existsSync(path.join(process.env.FAKE_TABS_DIR, req.target_id))) {
        socket.write(JSON.stringify({ id: req.id, success: false, error: 'tab closed', tab_missing: true }) + '\n');
        continue;
      }
      const respond = extra => socket.write(JSON.stringify({
        id: req.id, target_id: req.target_id, tab_created: !!req.tab_created, ...extra,
      }) + '\n');
      if (req.code === 'hang') { for (;;) {} }
      if (req.code === 'exit') { process.exit(1); }
      const ms = Number(req.code.split(':')[1] || 0);
      if (ms > req.timeout_ms) {
        setTimeout(() => respond({ success: false, error: 'timed out', timed_out: true }), req.timeout_ms);
      } else {
        setTimeout(() => respond({ success: true, result: { pid: process.pid } }), ms);
      }
    }
  });
});
setTimeout(() => server.listen(socketPath), Number(process.env.FAKE_START_DELAY_MS || 0));
`

// fakeExecutorTabs records each open tab as a file the fake daemon can see.
// When gate is set, Open signals opening and blocks until gate is closed.
type fakeExecutorTabs struct {
	dir     string
	mu      sync.Mutex
	n       int
	gate    chan struct{}
	opening chan struct{}
}

func (f *fakeExecutorTabs) Open(context.Context) (string, error) {
	if f.gate != nil {
		f.opening <- struct{}{}
		<-f.gate
	}
	f.mu.Lock()
	f.n++
	targetID := fmt.Sprintf("tab-%d", f.n)
	f.mu.Unlock()
	return targetID, os.WriteFile(filepath.Join(f.dir, targetID), nil, 0o644)
}

func (f *fakeExecutorTabs) Close(_ context.Context, targetID string) error {
	if err := os.Remove(filepath.Join(f.dir, targetID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (f *fakeExecutorTabs) isOpen(targetID string) bool {
	_, err := os.Stat(filepath.Join(f.dir, targetID))
	return err == nil
}

func newTestPlaywrightExecutorManager(t *testing.T) (*playwrightExecutorManager, *fakeExecutorTabs) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not available: %v", err)
	}
	// Unix socket paths are limited to ~108 bytes, so avoid t.TempDir's long paths.
	dir, err := os.MkdirTemp("", "pwx")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	script := filepath.Join(dir, "daemon.js")
	require.NoError(t, os.WriteFile(script, []byte(fakePlaywrightDaemon), 0o644))
	tabs := &fakeExecutorTabs{dir: t.TempDir()}
	t.Setenv("FAKE_TABS_DIR", tabs.dir)

	m := newPlaywrightExecutorManager(tabs)
	m.script = script
	m.socketDir = dir
	m.responseGrace = 500 * time.Millisecond
	t.Cleanup(func() { m.Shutdown(context.Background()) })
	return m, tabs
}

// namedExecutors drops the default executor, which always exists.
func namedExecutors(executors []*playwrightExecutor) []*playwrightExecutor {
	return slices.DeleteFunc(executors, func(e *playwrightExecutor) bool { return e.pinned })
}

func executorPID(t *testing.T, resp *playwrightDaemonResponse) int {
	t.Helper()
	require.True(t, resp.Success, "execution failed: %s", resp.Error)
	raw, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var result struct {
		PID int `json:"pid"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	return result.PID
}

func TestPlaywrightExecutorsRunConcurrently(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	// Warm both processes so the measurement covers only execution.
	for _, name := range []string{"a", "b"} {
		_, err := m.Execute(ctx, name, "sleep:0", 10*time.Second)
		require.NoError(t, err)
	}

	start := time.Now()
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := m.Execute(ctx, name, "sleep:2000", 10*time.Second)
			assert.NoError(t, err)
			assert.True(t, resp.Success)
		}()
	}
	wg.Wait()
	assert.Less(t, time.Since(start), 3500*time.Millisecond, "different executors should overlap")
}

func TestPlaywrightExecutorSerializesCallsOnOneName(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()
	_, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)

	start := time.Now()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Execute(ctx, "a", "sleep:500", 10*time.Second)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.GreaterOrEqual(t, time.Since(start), time.Second, "calls on one executor should not overlap")
}

func TestPlaywrightExecutorKeepsItsTab(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, first.TargetID)
	assert.True(t, first.TabCreated)

	second, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, first.TargetID, second.TargetID)
	assert.False(t, second.TabCreated)
}

func TestPlaywrightExecutorTimeoutReplacesOnlyThatProcess(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	firstPID := executorPID(t, first)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := m.Execute(ctx, "b", "sleep:1500", 10*time.Second)
		assert.NoError(t, err)
		assert.True(t, resp.Success, "executor b should be unaffected by a's timeout")
	}()

	timedOut, err := m.Execute(ctx, "a", "sleep:5000", time.Second)
	require.NoError(t, err)
	assert.True(t, timedOut.TimedOut)
	wg.Wait()

	next, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.NotEqual(t, firstPID, executorPID(t, next), "a timed-out executor should get a fresh process")
	assert.Equal(t, first.TargetID, next.TargetID, "the fresh process should be handed the executor's tab")
	assert.False(t, next.TabCreated)
}

func TestPlaywrightExecutorUnresponsiveProcessIsKilled(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	firstPID := executorPID(t, first)

	hung, err := m.Execute(ctx, "a", "hang", time.Second)
	require.NoError(t, err)
	assert.False(t, hung.Success)
	assert.Equal(t, first.TargetID, hung.TargetID, "a call whose process stops answering still reports its tab")

	next, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.NotEqual(t, firstPID, executorPID(t, next))
	assert.Equal(t, first.TargetID, next.TargetID)
}

func TestPlaywrightExecutorProcessExitReportsTab(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)

	exited, err := m.Execute(ctx, "a", "exit", 10*time.Second)
	require.NoError(t, err)
	assert.False(t, exited.Success)
	assert.Equal(t, first.TargetID, exited.TargetID)
	assert.False(t, exited.TabCreated)

	next, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.True(t, next.Success)
	assert.Equal(t, first.TargetID, next.TargetID)
}

func TestPlaywrightExecutorReopensClosedTab(t *testing.T) {
	m, tabs := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	require.NoError(t, tabs.Close(ctx, first.TargetID))

	next, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.True(t, next.Success)
	assert.True(t, next.TabCreated)
	assert.NotEqual(t, first.TargetID, next.TargetID)
	assert.True(t, tabs.isOpen(next.TargetID))
}

func TestPlaywrightExecutorShutdownClosesTabs(t *testing.T) {
	m, tabs := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	a, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	b, err := m.Execute(ctx, "b", "sleep:0", 10*time.Second)
	require.NoError(t, err)

	m.Shutdown(ctx)
	assert.False(t, tabs.isOpen(a.TargetID))
	assert.False(t, tabs.isOpen(b.TargetID))
	assert.Empty(t, m.List())
}

func TestPlaywrightExecutorShutdownClosesTabOpenedDuringShutdown(t *testing.T) {
	m, tabs := newTestPlaywrightExecutorManager(t)
	tabs.gate = make(chan struct{})
	tabs.opening = make(chan struct{}, 1)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
		done <- err
	}()
	<-tabs.opening

	shutdown := make(chan struct{})
	go func() {
		m.Shutdown(ctx)
		close(shutdown)
	}()
	select {
	case <-shutdown:
		t.Fatal("Shutdown returned before the in-flight call finished opening its tab")
	case <-time.After(200 * time.Millisecond):
	}

	close(tabs.gate)
	<-shutdown
	assert.ErrorIs(t, <-done, errPlaywrightExecutorsShutDown)
	entries, err := os.ReadDir(tabs.dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the tab opened during shutdown should be closed")
}

func TestPlaywrightExecutorLimit(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	for i := range maxPlaywrightExecutors {
		_, err := m.Execute(ctx, fmt.Sprintf("e%d", i), "sleep:0", 10*time.Second)
		require.NoError(t, err)
	}

	_, err := m.Execute(ctx, "one-too-many", "sleep:0", 10*time.Second)
	var limitErr *playwrightExecutorLimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Len(t, namedExecutors(limitErr.executors), maxPlaywrightExecutors)
	assert.Contains(t, limitErr.Error(), "DELETE /playwright/executors/{name}")

	// Existing executors keep working at the limit, and deleting one frees a slot.
	_, err = m.Execute(ctx, "e0", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	_, err = m.Delete(ctx, "e0")
	require.NoError(t, err)
	_, err = m.Execute(ctx, "one-too-many", "sleep:0", 10*time.Second)
	require.NoError(t, err)
}

func TestPlaywrightExecutorDelete(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Execute(ctx, "a", "sleep:5000", 10*time.Second)
		errCh <- err
	}()
	require.Eventually(t, func() bool {
		executors := namedExecutors(m.List())
		return len(executors) == 1 && executors[0].busy
	}, 2*time.Second, 10*time.Millisecond)

	targetID, err := m.Delete(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, first.TargetID, targetID)

	select {
	case err := <-errCh:
		assert.ErrorIs(t, err, errPlaywrightExecutorDeleted)
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call did not fail after delete")
	}
	assert.Empty(t, namedExecutors(m.List()))

	_, err = m.Delete(ctx, "a")
	assert.ErrorIs(t, err, errPlaywrightExecutorNotFound)

	// The name is reusable and gets a fresh tab.
	reused, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.True(t, reused.TabCreated)
	assert.NotEqual(t, first.TargetID, reused.TargetID)
}

func TestPlaywrightExecutorDeleteReportsTabOpenedByInFlightCall(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	done := make(chan *playwrightDaemonResponse, 1)
	go func() {
		resp, _ := m.Execute(ctx, "a", "sleep:5000", 10*time.Second)
		done <- resp
	}()
	require.Eventually(t, func() bool {
		executors := namedExecutors(m.List())
		return len(executors) == 1 && executors[0].targetID != ""
	}, 3*time.Second, 10*time.Millisecond, "the tab should be known before the call finishes")
	openedTab := namedExecutors(m.List())[0].targetID

	targetID, err := m.Delete(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, openedTab, targetID)
	assert.Nil(t, <-done)
}

func TestPlaywrightExecutorStartFailure(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	m.script = filepath.Join(m.socketDir, "missing.js")
	svc, err := newSvc(t, recorder.NewFFmpegManager())
	require.NoError(t, err)
	svc.playwrightExecutors = m

	name := "a"
	resp, err := svc.ExecutePlaywrightCode(context.Background(), oapi.ExecutePlaywrightCodeRequestObject{
		Body: &oapi.ExecutePlaywrightCodeJSONRequestBody{Code: "sleep:0", Executor: &name},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.ExecutePlaywrightCode500JSONResponse{}, resp)
	assert.Empty(t, namedExecutors(m.List()), "an executor that never started should not take a slot")

	resp, err = svc.ExecutePlaywrightCode(context.Background(), oapi.ExecutePlaywrightCodeRequestObject{
		Body: &oapi.ExecutePlaywrightCodeJSONRequestBody{Code: "sleep:0"},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.ExecutePlaywrightCode500JSONResponse{}, resp)
	executors := m.List()
	require.Len(t, executors, 1, "a failed start must not remove the default executor")
	assert.Equal(t, defaultPlaywrightExecutor, executors[0].name)
}

func TestPlaywrightExecutorHandlers(t *testing.T) {
	m, tabs := newTestPlaywrightExecutorManager(t)
	svc, err := newSvc(t, recorder.NewFFmpegManager())
	require.NoError(t, err)
	svc.playwrightExecutors = m
	ctx := context.Background()

	execute := func(name string) oapi.ExecutePlaywrightCodeResponseObject {
		t.Helper()
		resp, err := svc.ExecutePlaywrightCode(ctx, oapi.ExecutePlaywrightCodeRequestObject{
			Body: &oapi.ExecutePlaywrightCodeJSONRequestBody{Code: "sleep:0", Executor: &name},
		})
		require.NoError(t, err)
		return resp
	}

	ok, isOK := execute("a").(oapi.ExecutePlaywrightCode200JSONResponse)
	require.True(t, isOK)
	assert.True(t, ok.Success)
	require.NotNil(t, ok.Tab)
	assert.NotEmpty(t, ok.Tab.TargetId)
	assert.True(t, ok.Tab.Created)

	_, isBadRequest := execute("not a valid name").(oapi.ExecutePlaywrightCode400JSONResponse)
	assert.True(t, isBadRequest)

	for i := 1; i < maxPlaywrightExecutors; i++ {
		execute(fmt.Sprintf("e%d", i))
	}
	limit, isLimit := execute("one-too-many").(oapi.ExecutePlaywrightCode409JSONResponse)
	require.True(t, isLimit)
	assert.Len(t, limit.Executors, maxPlaywrightExecutors+1, "the default executor is listed but not counted")
	assert.Equal(t, defaultPlaywrightExecutor, limit.Executors[0].Name)
	assert.Equal(t, "a", limit.Executors[1].Name)

	list, err := svc.ListPlaywrightExecutors(ctx, oapi.ListPlaywrightExecutorsRequestObject{})
	require.NoError(t, err)
	assert.Len(t, list.(oapi.ListPlaywrightExecutors200JSONResponse).Executors, maxPlaywrightExecutors+1)

	keepTab := false
	deleted, err := svc.DeletePlaywrightExecutor(ctx, oapi.DeletePlaywrightExecutorRequestObject{
		Name:   "a",
		Params: oapi.DeletePlaywrightExecutorParams{CloseTab: &keepTab},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeletePlaywrightExecutor204Response{}, deleted)
	assert.True(t, tabs.isOpen(ok.Tab.TargetId), "close_tab=false should keep the tab")

	missing, err := svc.DeletePlaywrightExecutor(ctx, oapi.DeletePlaywrightExecutorRequestObject{Name: "a"})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeletePlaywrightExecutor404JSONResponse{}, missing)

	e1, ok1 := execute("e1").(oapi.ExecutePlaywrightCode200JSONResponse)
	require.True(t, ok1)
	deleted, err = svc.DeletePlaywrightExecutor(ctx, oapi.DeletePlaywrightExecutorRequestObject{Name: "e1"})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeletePlaywrightExecutor204Response{}, deleted)
	assert.False(t, tabs.isOpen(e1.Tab.TargetId), "delete should close the tab by default")
}

func TestPlaywrightExecutorListIncludesDefault(t *testing.T) {
	svc, err := newSvc(t, recorder.NewFFmpegManager())
	require.NoError(t, err)
	list, err := svc.ListPlaywrightExecutors(context.Background(), oapi.ListPlaywrightExecutorsRequestObject{})
	require.NoError(t, err)
	executors := list.(oapi.ListPlaywrightExecutors200JSONResponse).Executors
	require.Len(t, executors, 1)
	assert.Equal(t, defaultPlaywrightExecutor, executors[0].Name)
	assert.Nil(t, executors[0].TargetId, "the default executor owns no tab")
}

func TestPlaywrightExecutorDefaultLane(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	svc, err := newSvc(t, recorder.NewFFmpegManager())
	require.NoError(t, err)
	svc.playwrightExecutors = m
	ctx := context.Background()

	execute := func(executor *string) oapi.ExecutePlaywrightCodeResponseObject {
		t.Helper()
		resp, err := svc.ExecutePlaywrightCode(ctx, oapi.ExecutePlaywrightCodeRequestObject{
			Body: &oapi.ExecutePlaywrightCodeJSONRequestBody{Code: "sleep:0", Executor: executor},
		})
		require.NoError(t, err)
		return resp
	}

	unnamed, ok := execute(nil).(oapi.ExecutePlaywrightCode200JSONResponse)
	require.True(t, ok)
	require.NotNil(t, unnamed.Tab)
	assert.Equal(t, "active-tab", unnamed.Tab.TargetId, "the default executor binds the active tab")
	assert.False(t, unnamed.Tab.Created)

	name := defaultPlaywrightExecutor
	explicit, ok := execute(&name).(oapi.ExecutePlaywrightCode200JSONResponse)
	require.True(t, ok)
	assert.Equal(t, executorPID(t, &playwrightDaemonResponse{Success: true, Result: unnamed.Result}),
		executorPID(t, &playwrightDaemonResponse{Success: true, Result: explicit.Result}),
		`executor "default" should run on the same process as calls without an executor`)

	for i := range maxPlaywrightExecutors {
		n := fmt.Sprintf("e%d", i)
		_, ok := execute(&n).(oapi.ExecutePlaywrightCode200JSONResponse)
		require.True(t, ok, "the default executor should not take a named slot")
	}
	_, ok = execute(nil).(oapi.ExecutePlaywrightCode200JSONResponse)
	assert.True(t, ok, "the default executor works at the named executor limit")
}

func TestPlaywrightExecutorDefaultRecoversFromBlockedProcess(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)

	hung, err := m.Execute(ctx, defaultPlaywrightExecutor, "hang", time.Second)
	require.NoError(t, err)
	assert.False(t, hung.Success)

	for range 3 {
		start := time.Now()
		next, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
		require.NoError(t, err)
		assert.NotEqual(t, executorPID(t, first), executorPID(t, next))
		assert.Less(t, time.Since(start), 3*time.Second, "calls after a blocked process should not wait on it")
	}
}

func TestPlaywrightExecutorDeleteRestartsDefault(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:5000", 10*time.Second)
		errCh <- err
	}()
	require.Eventually(t, func() bool { return m.List()[0].busy }, 2*time.Second, 10*time.Millisecond)

	targetID, err := m.Delete(ctx, defaultPlaywrightExecutor)
	require.NoError(t, err)
	assert.Empty(t, targetID, "the default executor owns no tab to close")
	select {
	case err := <-errCh:
		assert.ErrorIs(t, err, errPlaywrightExecutorRestarted)
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call did not fail after restart")
	}

	executors := m.List()
	require.Len(t, executors, 1, "restarting the default executor must not remove it")
	assert.Equal(t, defaultPlaywrightExecutor, executors[0].name)

	next, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.NotEqual(t, executorPID(t, first), executorPID(t, next))
}

func TestPlaywrightExecutorRestartDefaultDuringStartup(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	t.Setenv("FAKE_START_DELAY_MS", "1000")
	ctx := context.Background()

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
		errCh <- err
	}()
	require.Eventually(t, func() bool { return m.List()[0].busy }, 2*time.Second, 10*time.Millisecond)

	start := time.Now()
	_, err := m.Delete(ctx, defaultPlaywrightExecutor)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "restart should not wait for the starting call")
	assert.ErrorIs(t, <-errCh, errPlaywrightExecutorRestarted, "a call starting its process during a restart should fail")

	next, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.True(t, next.Success)
}

func TestPlaywrightExecutorRestartDefaultDoesNotWaitForQueuedCalls(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()
	_, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)

	inFlight := make(chan error, 1)
	go func() {
		_, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:3000", 10*time.Second)
		inFlight <- err
	}()
	require.Eventually(t, func() bool { return m.List()[0].busy }, 2*time.Second, 10*time.Millisecond)
	queued := make(chan *playwrightDaemonResponse, 1)
	go func() {
		resp, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:1000", 10*time.Second)
		assert.NoError(t, err)
		queued <- resp
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	_, err = m.Delete(ctx, defaultPlaywrightExecutor)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "restart should not wait for queued calls")
	assert.ErrorIs(t, <-inFlight, errPlaywrightExecutorRestarted)

	resp := <-queued
	require.NotNil(t, resp)
	queuedPID := executorPID(t, resp)
	after, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, queuedPID, executorPID(t, after), "the process started for the queued call should be kept")
}

func TestPlaywrightExecutorDefaultKeepsProcessAfterTimeout(t *testing.T) {
	m, _ := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)
	timedOut, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:5000", time.Second)
	require.NoError(t, err)
	require.True(t, timedOut.TimedOut)

	next, err := m.Execute(ctx, defaultPlaywrightExecutor, "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, executorPID(t, first), executorPID(t, next), "the default executor keeps its process after a reported timeout")
}

func TestPlaywrightExecutorRejectsCallsAfterShutdown(t *testing.T) {
	m, tabs := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()
	m.Shutdown(ctx)

	for _, name := range []string{defaultPlaywrightExecutor, "a"} {
		_, err := m.Execute(ctx, name, "sleep:0", 10*time.Second)
		assert.ErrorIs(t, err, errPlaywrightExecutorsShutDown)
	}
	entries, err := os.ReadDir(tabs.dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no tab should be opened after shutdown")
	assert.Empty(t, m.List())
}
