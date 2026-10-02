package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// timeout_ms, like the real daemon) and "hang" blocks the event loop. A call
// without a target ID opens a tab and reports it before running. Every result
// carries the process ID so tests can tell when a process is replaced.
const fakePlaywrightDaemon = `
const net = require('net');
const socketPath = process.env.PLAYWRIGHT_DAEMON_SOCKET;
let tabs = 0;
net.createServer(socket => {
  let buffer = '';
  socket.on('data', data => {
    buffer += data;
    let i;
    while ((i = buffer.indexOf('\n')) !== -1) {
      const req = JSON.parse(buffer.slice(0, i));
      buffer = buffer.slice(i + 1);
      const targetId = req.target_id || ('tab-' + process.pid + '-' + (++tabs));
      if (!req.target_id) {
        socket.write(JSON.stringify({ id: req.id, tab: { target_id: targetId, tab_created: true } }) + '\n');
      }
      const respond = extra => socket.write(JSON.stringify({
        id: req.id, target_id: targetId, tab_created: !req.target_id, ...extra,
      }) + '\n');
      if (req.code === 'hang') { for (;;) {} }
      const ms = Number(req.code.split(':')[1] || 0);
      if (ms > req.timeout_ms) {
        setTimeout(() => respond({ success: false, error: 'timed out', timed_out: true }), req.timeout_ms);
      } else {
        setTimeout(() => respond({ success: true, result: { pid: process.pid } }), ms);
      }
    }
  });
}).listen(socketPath);
`

func newTestPlaywrightExecutorManager(t *testing.T) *playwrightExecutorManager {
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

	m := newPlaywrightExecutorManager()
	m.script = script
	m.socketDir = dir
	m.responseGrace = 500 * time.Millisecond
	t.Cleanup(m.Shutdown)
	return m
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
	m := newTestPlaywrightExecutorManager(t)
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
	m := newTestPlaywrightExecutorManager(t)
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
	m := newTestPlaywrightExecutorManager(t)
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
	m := newTestPlaywrightExecutorManager(t)
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
	m := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	firstPID := executorPID(t, first)

	_, err = m.Execute(ctx, "a", "hang", time.Second)
	require.Error(t, err)

	next, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.NotEqual(t, firstPID, executorPID(t, next))
	assert.Equal(t, first.TargetID, next.TargetID)
}

func TestPlaywrightExecutorLimit(t *testing.T) {
	m := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	for i := range maxPlaywrightExecutors {
		_, err := m.Execute(ctx, fmt.Sprintf("e%d", i), "sleep:0", 10*time.Second)
		require.NoError(t, err)
	}

	_, err := m.Execute(ctx, "one-too-many", "sleep:0", 10*time.Second)
	var limitErr *playwrightExecutorLimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Len(t, limitErr.executors, maxPlaywrightExecutors)
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
	m := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	first, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Execute(ctx, "a", "sleep:5000", 10*time.Second)
		errCh <- err
	}()
	require.Eventually(t, func() bool {
		executors := m.List()
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
	assert.Empty(t, m.List())

	_, err = m.Delete(ctx, "a")
	assert.ErrorIs(t, err, errPlaywrightExecutorNotFound)

	// The name is reusable and gets a fresh tab.
	reused, err := m.Execute(ctx, "a", "sleep:0", 10*time.Second)
	require.NoError(t, err)
	assert.True(t, reused.TabCreated)
	assert.NotEqual(t, first.TargetID, reused.TargetID)
}

func TestPlaywrightExecutorDeleteReportsTabOpenedByInFlightCall(t *testing.T) {
	m := newTestPlaywrightExecutorManager(t)
	ctx := context.Background()

	done := make(chan *playwrightDaemonResponse, 1)
	go func() {
		resp, _ := m.Execute(ctx, "a", "sleep:5000", 10*time.Second)
		done <- resp
	}()
	require.Eventually(t, func() bool {
		executors := m.List()
		return len(executors) == 1 && executors[0].targetID != ""
	}, 3*time.Second, 10*time.Millisecond, "the tab should be known before the call finishes")
	openedTab := m.List()[0].targetID

	targetID, err := m.Delete(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, openedTab, targetID)
	assert.Nil(t, <-done)
}

func TestPlaywrightExecutorStartFailure(t *testing.T) {
	m := newTestPlaywrightExecutorManager(t)
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
}

func TestPlaywrightExecutorHandlers(t *testing.T) {
	m := newTestPlaywrightExecutorManager(t)
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
	assert.Len(t, limit.Executors, maxPlaywrightExecutors)
	assert.Equal(t, "a", limit.Executors[0].Name)

	list, err := svc.ListPlaywrightExecutors(ctx, oapi.ListPlaywrightExecutorsRequestObject{})
	require.NoError(t, err)
	assert.Len(t, list.(oapi.ListPlaywrightExecutors200JSONResponse).Executors, maxPlaywrightExecutors)

	keepTab := false
	deleted, err := svc.DeletePlaywrightExecutor(ctx, oapi.DeletePlaywrightExecutorRequestObject{
		Name:   "a",
		Params: oapi.DeletePlaywrightExecutorParams{CloseTab: &keepTab},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeletePlaywrightExecutor204Response{}, deleted)

	missing, err := svc.DeletePlaywrightExecutor(ctx, oapi.DeletePlaywrightExecutorRequestObject{Name: "a"})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeletePlaywrightExecutor404JSONResponse{}, missing)
}

func TestPlaywrightExecutorListIsNeverNil(t *testing.T) {
	svc, err := newSvc(t, recorder.NewFFmpegManager())
	require.NoError(t, err)
	list, err := svc.ListPlaywrightExecutors(context.Background(), oapi.ListPlaywrightExecutorsRequestObject{})
	require.NoError(t, err)
	raw, err := json.Marshal(list)
	require.NoError(t, err)
	assert.JSONEq(t, `{"executors":[]}`, string(raw))
}
