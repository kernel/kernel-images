package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReplExecutorTabs records the tabs the API opens and closes for named
// executors. When gate is set, Open signals opening and waits for a value from
// gate: true opens the tab, false fails.
type fakeReplExecutorTabs struct {
	mu      sync.Mutex
	n       int
	open    map[string]bool
	closed  []string
	gate    chan bool
	opening chan struct{}
}

func (f *fakeReplExecutorTabs) Open(context.Context) (string, error) {
	if f.gate != nil {
		f.opening <- struct{}{}
		if !<-f.gate {
			return "", fmt.Errorf("open failed")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	targetID := fmt.Sprintf("tab-%d", f.n)
	f.open[targetID] = true
	return targetID, nil
}

func (f *fakeReplExecutorTabs) Close(_ context.Context, targetID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, targetID)
	f.closed = append(f.closed, targetID)
	return nil
}

func (f *fakeReplExecutorTabs) isOpen(targetID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[targetID]
}

func newBrowserReplExecutorsSvc(t *testing.T) (*ApiService, *fakeReplExecutorTabs) {
	t.Helper()
	svc := newBrowserReplSvc(t)
	// Unix socket paths are limited to ~108 bytes, so keep every executor's
	// socket in a short directory.
	dir, err := os.MkdirTemp("", "brx")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("BROWSER_REPL_SOCKET", filepath.Join(dir, "browser-repl.sock"))

	tabs := &fakeReplExecutorTabs{open: make(map[string]bool)}
	svc.browserReplExecutors = newBrowserReplExecutors(svc.browserRepl, tabs)
	t.Cleanup(func() { svc.browserReplExecutors.Shutdown(context.Background()) })
	return svc, tabs
}

func execOn(t *testing.T, svc *ApiService, executor, code string, timeoutSec int) oapi.ExecuteBrowserRepl200JSONResponse {
	t.Helper()
	body := &oapi.ExecuteBrowserReplJSONRequestBody{Code: code}
	if executor != "" {
		body.Executor = &executor
	}
	if timeoutSec > 0 {
		body.TimeoutSec = &timeoutSec
	}
	return executeBrowserRepl(t, svc, body)
}

// written returns the last repl.write value decoded as JSON.
func written(t *testing.T, resp oapi.ExecuteBrowserRepl200JSONResponse) any {
	t.Helper()
	require.True(t, resp.Success, "execution failed: %v", resp.Error)
	require.NotNil(t, resp.Content)
	for i := len(*resp.Content) - 1; i >= 0; i-- {
		text, err := (*resp.Content)[i].AsBrowserReplTextContent()
		if err != nil || text.Channel != oapi.BrowserReplTextContentChannelWrite {
			continue
		}
		var got any
		require.NoError(t, json.Unmarshal([]byte(text.Text), &got))
		return got
	}
	t.Fatal("no repl.write content")
	return nil
}

func TestBrowserReplExecutorsRunConcurrently(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	for _, name := range []string{"", "a", "b"} {
		execOn(t, svc, name, "1", 0)
	}

	start := time.Now()
	var wg sync.WaitGroup
	for _, name := range []string{"", "a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := &oapi.ExecuteBrowserReplJSONRequestBody{Code: "await new Promise((resolve) => setTimeout(resolve, 2000));"}
			if name != "" {
				body.Executor = &name
			}
			resp, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{Body: body})
			assert.NoError(t, err)
			typed, ok := resp.(oapi.ExecuteBrowserRepl200JSONResponse)
			assert.True(t, ok && typed.Success, "executor %q: %#v", name, resp)
		}()
	}
	wg.Wait()
	assert.Less(t, time.Since(start), 3500*time.Millisecond, "calls on different executors should overlap")
}

func TestBrowserReplExecutorStateIsIsolated(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)

	first := execOn(t, svc, "a", "var shared = 'a'; repl.write(JSON.stringify(repl.executor))", 0)
	assert.Equal(t, "a", written(t, first))
	assert.True(t, first.NewState)
	execOn(t, svc, "", "var shared = 'default';", 0)

	again := execOn(t, svc, "a", "repl.write(JSON.stringify(shared))", 0)
	assert.Equal(t, "a", written(t, again))
	assert.False(t, again.NewState)
	assert.Equal(t, "default", written(t, execOn(t, svc, "", "repl.write(JSON.stringify(shared))", 0)))
	assert.Equal(t, "undefined", written(t, execOn(t, svc, "b", "repl.write(JSON.stringify(typeof shared))", 0)))
}

func TestBrowserReplExecutorTimeoutOnlyEndsThatExecutor(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	execOn(t, svc, "a", "var kept = 'a'", 0)
	execOn(t, svc, "b", "var kept = 'b'", 0)

	timedOut := execOn(t, svc, "a", "await new Promise(() => {})", 1)
	assert.False(t, timedOut.Success)
	require.NotNil(t, timedOut.Terminated)
	assert.True(t, *timedOut.Terminated)

	other := execOn(t, svc, "b", "repl.write(JSON.stringify(kept))", 0)
	assert.Equal(t, "b", written(t, other))
	assert.False(t, other.NewState, "a timeout in one executor must not end another")

	next := execOn(t, svc, "a", "repl.write(JSON.stringify(typeof kept))", 0)
	assert.Equal(t, "undefined", written(t, next))
	assert.True(t, next.NewState)
}

func TestBrowserReplExecutorOwnsATab(t *testing.T) {
	svc, tabs := newBrowserReplExecutorsSvc(t)

	execOn(t, svc, "a", "1", 0)
	execOn(t, svc, "", "1", 0)
	list, err := svc.ListBrowserReplExecutors(context.Background(), oapi.ListBrowserReplExecutorsRequestObject{})
	require.NoError(t, err)
	executors := list.(oapi.ListBrowserReplExecutors200JSONResponse).Executors
	require.Len(t, executors, 2)
	assert.Equal(t, defaultBrowserReplExecutor, executors[0].Name)
	assert.Nil(t, executors[0].TargetId, "the default executor owns no tab")
	assert.Equal(t, "a", executors[1].Name)
	require.NotNil(t, executors[1].TargetId)
	assert.True(t, tabs.isOpen(*executors[1].TargetId))

	// A new process keeps the executor's tab.
	reset := executeBrowserRepl(t, svc, &oapi.ExecuteBrowserReplJSONRequestBody{Executor: ptrTo("a"), Reset: ptrTo(true)})
	assert.True(t, reset.NewState)
	require.NotNil(t, reset.Tab)
	assert.Equal(t, *executors[1].TargetId, reset.Tab.TargetId)
	assert.False(t, reset.Tab.Created)
}

func TestBrowserReplExecutorLimit(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	for i := range maxBrowserReplExecutors {
		execOn(t, svc, fmt.Sprintf("e%d", i), "1", 0)
	}

	name := "one-too-many"
	resp, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
		Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: "1", Executor: &name},
	})
	require.NoError(t, err)
	limit, ok := resp.(oapi.ExecuteBrowserRepl409JSONResponse)
	require.True(t, ok, "expected 409, got %T", resp)
	assert.Len(t, limit.Executors, maxBrowserReplExecutors+1, "the default executor is listed but not counted")
	assert.Equal(t, defaultBrowserReplExecutor, limit.Executors[0].Name)
	assert.Contains(t, limit.Message, "DELETE /repl/executors/{name}")

	assert.True(t, execOn(t, svc, "", "1", 0).Success, "the default executor works at the limit")
}

func TestBrowserReplExecutorInvalidName(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	name := "not a name"
	resp, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
		Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: "1", Executor: &name},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.ExecuteBrowserRepl400JSONResponse{}, resp)
}

func TestBrowserReplExecutorDelete(t *testing.T) {
	svc, tabs := newBrowserReplExecutorsSvc(t)
	ctx := context.Background()
	execOn(t, svc, "a", "var kept = 1", 0)
	targetID := svc.browserReplExecutors.list()[1].targetID

	inFlight := make(chan oapi.ExecuteBrowserRepl200JSONResponse, 1)
	queued := make(chan oapi.ExecuteBrowserRepl200JSONResponse, 1)
	go func() { inFlight <- execOn(t, svc, "a", "await new Promise(() => {})", 30) }()
	require.Eventually(t, func() bool { return svc.browserReplExecutors.list()[1].busy }, 5*time.Second, 10*time.Millisecond)
	go func() { queued <- execOn(t, svc, "a", "1", 30) }()
	time.Sleep(100 * time.Millisecond)

	deleted, err := svc.DeleteBrowserReplExecutor(ctx, oapi.DeleteBrowserReplExecutorRequestObject{Name: "a"})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeleteBrowserReplExecutor204Response{}, deleted)

	running := <-inFlight
	assert.False(t, running.Success)
	require.NotNil(t, running.Error)
	assert.Contains(t, *running.Error, "executor was deleted")
	waiting := <-queued
	assert.False(t, waiting.Success)
	require.NotNil(t, waiting.Error)
	assert.Contains(t, *waiting.Error, "executor was deleted")
	assert.False(t, tabs.isOpen(targetID), "delete should close the executor's tab")
	assert.Len(t, svc.browserReplExecutors.list(), 1)

	missing, err := svc.DeleteBrowserReplExecutor(ctx, oapi.DeleteBrowserReplExecutorRequestObject{Name: "a"})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeleteBrowserReplExecutor404JSONResponse{}, missing)

	reused := execOn(t, svc, "a", "repl.write(JSON.stringify(typeof kept))", 0)
	assert.Equal(t, "undefined", written(t, reused))
	assert.True(t, reused.NewState)
	assert.NotEqual(t, targetID, svc.browserReplExecutors.list()[1].targetID)

	keepTab := false
	newTarget := svc.browserReplExecutors.list()[1].targetID
	_, err = svc.DeleteBrowserReplExecutor(ctx, oapi.DeleteBrowserReplExecutorRequestObject{
		Name:   "a",
		Params: oapi.DeleteBrowserReplExecutorParams{CloseTab: &keepTab},
	})
	require.NoError(t, err)
	assert.True(t, tabs.isOpen(newTarget), "close_tab=false should keep the tab")
}

func TestBrowserReplExecutorDeleteRestartsDefault(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	execOn(t, svc, "", "var kept = 1", 0)

	inFlight := make(chan oapi.ExecuteBrowserRepl200JSONResponse, 1)
	queued := make(chan oapi.ExecuteBrowserRepl200JSONResponse, 1)
	go func() { inFlight <- execOn(t, svc, "", "await new Promise(() => {})", 30) }()
	require.Eventually(t, func() bool { return svc.browserReplExecutors.list()[0].busy }, 5*time.Second, 10*time.Millisecond)
	go func() { queued <- execOn(t, svc, "", "repl.write(JSON.stringify(typeof kept))", 30) }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	restarted, err := svc.DeleteBrowserReplExecutor(context.Background(), oapi.DeleteBrowserReplExecutorRequestObject{Name: defaultBrowserReplExecutor})
	require.NoError(t, err)
	assert.IsType(t, oapi.DeleteBrowserReplExecutor204Response{}, restarted)
	assert.Less(t, time.Since(start), time.Second, "restarting the default executor should not wait for queued calls")

	running := <-inFlight
	assert.False(t, running.Success)
	require.NotNil(t, running.Error)
	assert.Contains(t, *running.Error, "executor was restarted")

	next := <-queued
	assert.Equal(t, "undefined", written(t, next), "queued calls run on a new process")
	assert.True(t, next.NewState)
	assert.Len(t, svc.browserReplExecutors.list(), 1, "restarting the default executor must not remove it")
}

func TestBrowserReplExecutorRestartIdleDefault(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	execOn(t, svc, "", "var kept = 1", 0)
	pid := browserReplTestChild(t, svc).cmd.Process.Pid

	_, err := svc.DeleteBrowserReplExecutor(context.Background(), oapi.DeleteBrowserReplExecutorRequestObject{Name: defaultBrowserReplExecutor})
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return !processAlive(pid) }, 5*time.Second, 20*time.Millisecond, "an idle restart should stop the process right away")

	next := execOn(t, svc, "", "repl.write(JSON.stringify(typeof kept))", 0)
	assert.Equal(t, "undefined", written(t, next))
	assert.True(t, next.NewState)
}

func TestBrowserReplNamedExecutorRejectsCustomTools(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	resp := execOn(t, svc, "a", `webmcp.listCustomTools();`, 0)
	assert.False(t, resp.Success)
	require.NotNil(t, resp.Error)
	assert.Contains(t, *resp.Error, "only available in the default executor")
}

func TestBrowserReplExecutorStartFailureDropsExecutor(t *testing.T) {
	svc, tabs := newBrowserReplExecutorsSvc(t)
	t.Setenv("BROWSER_REPL_SCRIPT", filepath.Join(t.TempDir(), "missing.js"))

	name := "a"
	resp, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
		Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: "1", Executor: &name},
	})
	require.NoError(t, err)
	assert.IsType(t, oapi.ExecuteBrowserRepl500JSONResponse{}, resp)
	assert.Len(t, svc.browserReplExecutors.list(), 1, "an executor that never started should not take a slot")
	assert.Zero(t, tabs.n, "no tab should be opened for an executor that never started")
}

func TestBrowserReplExecutorsShutdown(t *testing.T) {
	svc, tabs := newBrowserReplExecutorsSvc(t)
	execOn(t, svc, "a", "1", 0)
	execOn(t, svc, "b", "1", 0)

	svc.browserReplExecutors.Shutdown(context.Background())
	assert.Empty(t, tabs.open, "shutdown should close every executor tab")

	name := "a"
	_, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
		Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: "1", Executor: &name},
	})
	assert.ErrorIs(t, err, errBrowserReplExecutorsShutDown)
}

func TestBrowserReplExecutorInvalidFirstCallsDoNotHoldSlots(t *testing.T) {
	svc, _ := newBrowserReplExecutorsSvc(t)
	for i := range maxBrowserReplExecutors + 1 {
		name := fmt.Sprintf("e%d", i)
		resp, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
			Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: "", Executor: &name},
		})
		require.NoError(t, err)
		assert.IsType(t, oapi.ExecuteBrowserRepl400JSONResponse{}, resp)
	}
	assert.Len(t, svc.browserReplExecutors.list(), 1)
	assert.True(t, execOn(t, svc, "valid", "1", 0).Success)
}

func TestBrowserReplExecutorFailedFirstCallKeepsQueuedCall(t *testing.T) {
	svc, tabs := newBrowserReplExecutorsSvc(t)
	tabs.gate = make(chan bool)
	tabs.opening = make(chan struct{}, 2)

	first := make(chan oapi.ExecuteBrowserReplResponseObject, 1)
	second := make(chan oapi.ExecuteBrowserRepl200JSONResponse, 1)
	name := "a"
	go func() {
		resp, err := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
			Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: "1", Executor: &name},
		})
		assert.NoError(t, err)
		first <- resp
	}()
	<-tabs.opening
	go func() { second <- execOn(t, svc, "a", "repl.write(JSON.stringify(1))", 0) }()
	time.Sleep(100 * time.Millisecond)

	tabs.gate <- false
	assert.IsType(t, oapi.ExecuteBrowserRepl500JSONResponse{}, <-first)
	<-tabs.opening
	tabs.gate <- true
	assert.Equal(t, float64(1), written(t, <-second), "a queued call must survive another call's failed start")
	assert.Len(t, svc.browserReplExecutors.list(), 2)
}

func TestBrowserReplExecutorDeleteDuringTabOpen(t *testing.T) {
	for _, body := range []oapi.ExecuteBrowserReplJSONRequestBody{
		{Code: "1"},
		{Code: "", Reset: ptrTo(true)},
	} {
		t.Run(fmt.Sprintf("reset=%v", body.Reset != nil), func(t *testing.T) {
			testBrowserReplExecutorDeleteDuringTabOpen(t, body)
		})
	}
}

func testBrowserReplExecutorDeleteDuringTabOpen(t *testing.T, body oapi.ExecuteBrowserReplJSONRequestBody) {
	svc, tabs := newBrowserReplExecutorsSvc(t)
	tabs.gate = make(chan bool)
	tabs.opening = make(chan struct{}, 1)

	body.Executor = ptrTo("a")
	result := make(chan oapi.ExecuteBrowserRepl200JSONResponse, 1)
	go func() { result <- executeBrowserRepl(t, svc, &body) }()
	<-tabs.opening

	deleted := make(chan struct{})
	go func() {
		_, err := svc.DeleteBrowserReplExecutor(context.Background(), oapi.DeleteBrowserReplExecutorRequestObject{Name: "a"})
		assert.NoError(t, err)
		close(deleted)
	}()
	time.Sleep(100 * time.Millisecond)
	tabs.gate <- true

	resp := <-result
	assert.False(t, resp.Success)
	require.NotNil(t, resp.Error)
	assert.Contains(t, *resp.Error, "executor was deleted")
	<-deleted
	assert.Empty(t, tabs.open, "the tab opened while the executor was being deleted is closed")
}

func TestBrowserReplExecutorsShutdownClosesTabOpenedAfterRetireBudget(t *testing.T) {
	svc, tabs := newBrowserReplExecutorsSvc(t)
	tabs.gate = make(chan bool)
	tabs.opening = make(chan struct{}, 1)

	result := make(chan oapi.ExecuteBrowserReplResponseObject, 1)
	go func() {
		name := "a"
		resp, _ := svc.ExecuteBrowserRepl(context.Background(), oapi.ExecuteBrowserReplRequestObject{
			Body: &oapi.ExecuteBrowserReplJSONRequestBody{Code: "1", Executor: &name},
		})
		result <- resp
	}()
	<-tabs.opening

	// Shutdown gives up waiting for the call after its retire budget, while
	// the tab is still opening.
	svc.browserReplExecutors.Shutdown(context.Background())
	tabs.gate <- true
	<-result
	assert.Empty(t, tabs.open, "a tab that finishes opening after shutdown is closed")
	assert.Eventually(t, func() bool {
		entries, _ := os.ReadDir(filepath.Dir(browserReplSocketPath()))
		for _, entry := range entries {
			if entry.Name() != "browser-repl.sock" && filepath.Ext(entry.Name()) == ".sock" {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "the executor's process is stopped and its socket removed")
}

func TestHeapLimitWatcher(t *testing.T) {
	var out, logs bytes.Buffer
	w := newHeapLimitWatcher(&out, slog.New(slog.NewTextHandler(&logs, nil)), "a", "512")

	for _, chunk := range []string{"[browser-repl] listening\n", "FATAL ERROR: Reac", "hed heap limit Allocation failed\n", "FATAL ERROR: Reached heap limit\n"} {
		_, err := w.Write([]byte(chunk))
		require.NoError(t, err)
	}
	assert.Equal(t, "[browser-repl] listening\nFATAL ERROR: Reached heap limit Allocation failed\nFATAL ERROR: Reached heap limit\n", out.String(), "output is forwarded unchanged")
	assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("hit heap limit")), "the limit is logged once")
	assert.Contains(t, logs.String(), "executor=a")
	assert.Contains(t, logs.String(), "limit_mb=512")
}

func ptrTo[T any](v T) *T { return &v }
