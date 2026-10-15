package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/stretchr/testify/require"
)

func newProcessTestService(t *testing.T, config ProcessOutputConfig) *ApiService {
	t.Helper()
	budget, err := newProcessOutputBudget(config)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	svc := &ApiService{processOutput: budget, lifecycleCtx: ctx, lifecycleCancel: cancel}
	t.Cleanup(func() {
		cancel()
		svc.procMu.RLock()
		handles := make([]*processHandle, 0, len(svc.procs))
		for _, h := range svc.procs {
			handles = append(handles, h)
		}
		svc.procMu.RUnlock()
		for _, h := range handles {
			select {
			case <-h.doneCh:
				svc.releaseProcess(h)
			case <-time.After(5 * time.Second):
				t.Error("process did not stop during cleanup")
			}
		}
	})
	return svc
}

func spawnProcess(t *testing.T, svc *ApiService, command string, args ...string) oapi.ProcessSpawn200JSONResponse {
	t.Helper()
	resp, err := svc.ProcessSpawn(context.Background(), oapi.ProcessSpawnRequestObject{Body: &oapi.ProcessSpawnRequest{Command: command, Args: &args}})
	require.NoError(t, err)
	spawn, ok := resp.(oapi.ProcessSpawn200JSONResponse)
	require.True(t, ok, "unexpected spawn response: %T", resp)

	return spawn
}

func processStream(t *testing.T, svc *ApiService, spawn oapi.ProcessSpawn200JSONResponse, params ...oapi.ProcessStdoutStreamParams) io.ReadCloser {
	t.Helper()
	server := httptest.NewServer(oapi.Handler(oapi.NewStrictHandler(svc, nil)))
	t.Cleanup(server.Close)
	client, err := oapi.NewClient(server.URL, oapi.WithHTTPClient(&http.Client{Timeout: 20 * time.Second}))
	require.NoError(t, err)
	var p oapi.ProcessStdoutStreamParams
	if len(params) > 0 {
		p = params[0]
	}
	resp, err := client.ProcessStdoutStream(context.Background(), *spawn.ProcessId, &p)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp.Body
}

func collectProcessStream(t *testing.T, body io.Reader) (string, []oapi.ProcessStreamEvent) {
	t.Helper()
	var output strings.Builder
	events := make([]oapi.ProcessStreamEvent, 0)
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event oapi.ProcessStreamEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		events = append(events, event)
		if event.DataB64 != nil {
			data, err := base64.StdEncoding.DecodeString(*event.DataB64)
			require.NoError(t, err)
			output.Write(data)
		}
	}
	require.NoError(t, scanner.Err())
	return output.String(), events
}

func waitProcessExit(t *testing.T, svc *ApiService, spawn oapi.ProcessSpawn200JSONResponse) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := svc.ProcessStatus(context.Background(), oapi.ProcessStatusRequestObject{ProcessId: *spawn.ProcessId})
		if err != nil {
			return false
		}
		status, ok := resp.(oapi.ProcessStatus200JSONResponse)
		return ok && status.State != nil && *status.State == "exited"
	}, 5*time.Second, 10*time.Millisecond, "process did not exit without a stream reader")
}

func requireProcessExitEvent(t *testing.T, events []oapi.ProcessStreamEvent, code int) {
	t.Helper()
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.NotNil(t, last.Event)
	require.Equal(t, "exit", string(*last.Event))
	require.Equal(t, code, *last.ExitCode)
	for _, event := range events[:len(events)-1] {
		require.Nil(t, event.Event)
	}
}

func TestProcessOutputWithoutReaderAndReplay(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	spawn := spawnProcess(t, svc, "head", "-c", "16777216", "/dev/zero")
	waitProcessExit(t, svc, spawn)
	for i := 0; i < 2; i++ {
		output, events := collectProcessStream(t, processStream(t, svc, spawn, oapi.ProcessStdoutStreamParams{Offset: ptrOf(int64(0))}))
		require.Equal(t, strings.Repeat("\x00", 16<<20), output)
		requireProcessExitEvent(t, events, 0)
	}
}

func TestProcessStreamAfterExit(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	for i := 0; i < 20; i++ {
		spawn := spawnProcess(t, svc, "sh", "-c", "printf one; printf two >&2; exit 7")
		waitProcessExit(t, svc, spawn)
		output, events := collectProcessStream(t, processStream(t, svc, spawn))
		require.Contains(t, output, "one")
		require.Contains(t, output, "two")
		requireProcessExitEvent(t, events, 7)
	}
}

func TestProcessExecTimeoutIsNotServerError(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	args, timeout := []string{"-c", "printf before; sleep 4; true"}, 1
	started := time.Now()
	resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "sh", Args: &args, TimeoutSec: &timeout}})
	require.NoError(t, err)
	require.Less(t, time.Since(started), 3*time.Second)
	rec := httptest.NewRecorder()
	require.NoError(t, resp.VisitProcessExecResponse(rec))
	require.Equal(t, 422, rec.Code, rec.Body.String())
	var result struct {
		Code      string `json:"code"`
		StdoutB64 string `json:"stdout_b64"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Equal(t, "process_timeout", result.Code)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("before")), result.StdoutB64)
}

func TestProcessSlowReaderAndResume(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	spawn := spawnProcess(t, svc, "head", "-c", "2097152", "/dev/zero")
	body := processStream(t, svc, spawn)
	scanner := bufio.NewScanner(body)
	var first oapi.ProcessStreamEvent
	var lastID string
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "id: ") {
			lastID = strings.TrimPrefix(scanner.Text(), "id: ")
		}
		if strings.HasPrefix(scanner.Text(), "data: ") {
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &first))
			break
		}
	}
	require.NotNil(t, first.Offset)
	require.Equal(t, strconv.FormatInt(*first.Offset, 10), lastID)
	// Leave the first stream stalled while the producer finishes.
	waitProcessExit(t, svc, spawn)
	require.NoError(t, body.Close())
	firstData, err := base64.StdEncoding.DecodeString(*first.DataB64)
	require.NoError(t, err)
	for _, params := range []oapi.ProcessStdoutStreamParams{
		{LastEventID: &lastID},
		{Offset: first.Offset},
		{Offset: ptrOf(int64(0)), LastEventID: &lastID},
	} {
		stream := processStream(t, svc, spawn, params)
		output, events := collectProcessStream(t, stream)
		require.NoError(t, stream.Close())
		require.Equal(t, 2<<20, len(firstData)+len(output), "resumed stream duplicated or lost output")
		require.Equal(t, strings.Repeat("\x00", 2<<20), string(firstData)+output)
		requireProcessExitEvent(t, events, 0)
	}
	for _, offset := range []int64{-1, 1, 1 << 40} {
		resp, err := svc.ProcessStdoutStream(context.Background(), oapi.ProcessStdoutStreamRequestObject{ProcessId: *spawn.ProcessId, Params: oapi.ProcessStdoutStreamParams{Offset: &offset}})
		require.NoError(t, err)
		require.IsType(t, oapi.ProcessStdoutStream400JSONResponse{}, resp)
	}
	resp, err := svc.ProcessStdoutStream(context.Background(), oapi.ProcessStdoutStreamRequestObject{
		ProcessId: *spawn.ProcessId, Params: oapi.ProcessStdoutStreamParams{Offset: ptrOf(int64(0)), LastEventID: ptrOf("invalid")},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.ProcessStdoutStream400JSONResponse{}, resp)
}

func TestProcessExecCancellationStopsChildren(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	args := []string{"-c", `sleep 30 & child=$!; printf before; printf '%s' "$child" > "$1"; wait`, "sh", pidFile}
	type result struct {
		response oapi.ProcessExecResponseObject
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		response, err := svc.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "sh", Args: &args}})
		completed <- result{response: response, err: err}
	}()
	childPID := waitProcessPIDFile(t, pidFile)
	cancel()
	select {
	case result := <-completed:
		require.NoError(t, result.err)
		require.IsType(t, oapi.ProcessExec422JSONResponse{}, result.response)
		failure := result.response.(oapi.ProcessExec422JSONResponse)
		require.Equal(t, oapi.ProcessCanceled, failure.Code)
		require.Equal(t, base64.StdEncoding.EncodeToString([]byte("before")), *failure.StdoutB64)
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not finish after request cancellation")
	}
	requireProcessStopped(t, childPID)
}

func TestProcessSpawnShutdownStopsChildren(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			svc, err := newSvc(t, recorder.NewFFmpegManager())
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, svc.Shutdown(ctx))
			})
			pidFile := filepath.Join(t.TempDir(), "child-pid")
			args := []string{"-c", `sleep 30 & printf '%s' "$!" > "$1"; wait`, "sh", pidFile}
			response, err := svc.ProcessSpawn(context.Background(), oapi.ProcessSpawnRequestObject{Body: &oapi.ProcessSpawnRequest{
				Command: "sh", Args: &args, AllocateTty: &tty,
			}})
			require.NoError(t, err)
			require.IsType(t, oapi.ProcessSpawn200JSONResponse{}, response)
			spawn := response.(oapi.ProcessSpawn200JSONResponse)
			childPID := waitProcessPIDFile(t, pidFile)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, svc.Shutdown(ctx))
			requireProcessStopped(t, *spawn.Pid)
			requireProcessStopped(t, childPID)
			require.Eventually(t, func() bool {
				status, err := svc.ProcessStatus(context.Background(), oapi.ProcessStatusRequestObject{ProcessId: *spawn.ProcessId})
				require.NoError(t, err)
				_, released := status.(oapi.ProcessStatus404JSONResponse)
				return released
			}, time.Second, 10*time.Millisecond, "shutdown did not release the process")
		})
	}
}

func waitProcessPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(data))
		return err == nil && pid > 0
	}, 5*time.Second, 10*time.Millisecond, "command did not start its child")
	return pid
}

func requireProcessStopped(t *testing.T, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return os.IsNotExist(err) || strings.Contains(string(stat), ") Z ")
	}, time.Second, 10*time.Millisecond, "process %d is still running", pid)
}

func TestProcessSpawnTimeoutSurvivesRequestCancellation(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			t.Parallel()
			svc := newProcessTestService(t, ProcessOutputConfig{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			args := []string{"-c", "sleep 30"}
			resp, err := svc.ProcessSpawn(ctx, oapi.ProcessSpawnRequestObject{Body: &oapi.ProcessSpawnRequest{Command: "sh", Args: &args, TimeoutSec: ptrOf(1), AllocateTty: &tty}})
			require.NoError(t, err)
			spawn := resp.(oapi.ProcessSpawn200JSONResponse)
			cancel()
			_, events := collectProcessStream(t, processStream(t, svc, spawn))
			requireProcessExitEvent(t, events, -1)
			require.Equal(t, oapi.ProcessTimeout, *events[len(events)-1].TerminationReason)
			status, err := svc.ProcessStatus(context.Background(), oapi.ProcessStatusRequestObject{ProcessId: *spawn.ProcessId})
			require.NoError(t, err)
			require.Equal(t, oapi.ProcessTimeout, *status.(oapi.ProcessStatus200JSONResponse).TerminationReason)
		})
	}
}

func TestProcessTimeoutStopsChildren(t *testing.T) {
	t.Parallel()
	svc := newProcessTestService(t, ProcessOutputConfig{})
	args := []string{"-c", "sleep 30 & echo $!; wait"}
	resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "sh", Args: &args, TimeoutSec: ptrOf(1)}})
	require.NoError(t, err)
	failure := resp.(oapi.ProcessExec422JSONResponse)
	require.Equal(t, oapi.ProcessTimeout, failure.Code)
	data, err := base64.StdEncoding.DecodeString(*failure.StdoutB64)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return os.IsNotExist(err) || strings.Contains(string(stat), ") Z ")
	}, time.Second, 10*time.Millisecond, "child %d is still running", pid)
}

func TestProcessDetachedChildDoesNotHoldExecResponse(t *testing.T) {
	t.Parallel()
	svc := newProcessTestService(t, ProcessOutputConfig{})
	pidFile := filepath.Join(t.TempDir(), "pid")
	args := []string{"-c", `setsid sh -c 'echo $$ > "$1"; exec sleep 30' sh "$1" & wait`, "sh", pidFile}
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	started := time.Now()
	resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "sh", Args: &args, TimeoutSec: ptrOf(1)}})
	require.NoError(t, err)
	require.Less(t, time.Since(started), 4*time.Second)
	failure := resp.(oapi.ProcessExec422JSONResponse)
	require.Equal(t, oapi.ProcessTimeout, failure.Code)
	require.True(t, *failure.OutputTruncated)
}

func TestProcessOutputQuota(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{MaxBytes: 4096, TotalMaxBytes: 8192})
	for i := 0; i < 2; i++ {
		spawn := spawnProcess(t, svc, "sh", "-c", "head -c 65536 /dev/zero; sleep 30")
		output, events := collectProcessStream(t, processStream(t, svc, spawn))
		require.NotEmpty(t, output)
		require.Less(t, len(output), 4096)
		require.Equal(t, strings.Repeat("\x00", len(output)), output)
		requireProcessExitEvent(t, events, -1)
		require.Equal(t, oapi.OutputLimit, *events[len(events)-1].TerminationReason)
		require.True(t, *events[len(events)-1].OutputTruncated)
	}

	// Retained output consumes the shared budget until its retention expires.
	args := []string{"-c", "printf more; sleep 30"}
	resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "sh", Args: &args}})
	require.NoError(t, err)
	failure := resp.(oapi.ProcessExec422JSONResponse)
	require.Equal(t, oapi.OutputLimit, failure.Code)
	require.Empty(t, *failure.StdoutB64)
}

func TestProcessRetentionReclaimsOutputQuota(t *testing.T) {
	t.Parallel()
	svc := newProcessTestService(t, ProcessOutputConfig{MaxBytes: 4096, TotalMaxBytes: 4096})
	spawn := spawnProcess(t, svc, "head", "-c", "65536", "/dev/zero")
	waitProcessExit(t, svc, spawn)
	require.Eventually(t, func() bool {
		resp, err := svc.ProcessStatus(context.Background(), oapi.ProcessStatusRequestObject{ProcessId: *spawn.ProcessId})
		_, expired := resp.(oapi.ProcessStatus404JSONResponse)
		return err == nil && expired
	}, 12*time.Second, 20*time.Millisecond)
	args := []string{"-c", "printf reclaimed"}
	resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "sh", Args: &args}})
	require.NoError(t, err)
	result := resp.(oapi.ProcessExec200JSONResponse)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("reclaimed")), *result.StdoutB64)
}

func TestProcessStorageFailureDoesNotRunCommand(t *testing.T) {
	dir := t.TempDir()
	svc := newProcessTestService(t, ProcessOutputConfig{Dir: filepath.Join(dir, "missing")})
	marker := filepath.Join(dir, "ran")
	resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "touch", Args: &[]string{marker}}})
	require.NoError(t, err)
	require.IsType(t, oapi.ProcessExec500JSONResponse{}, resp)
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err))
}

func TestProcessTimeoutHTTPContract(t *testing.T) {
	t.Parallel()
	svc := newProcessTestService(t, ProcessOutputConfig{})
	server := httptest.NewServer(oapi.Handler(oapi.NewStrictHandler(svc, nil)))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(server.URL+"/process/exec", "application/json", strings.NewReader(`{"command":"sh","args":["-c","printf partial; sleep 30"],"timeout_sec":1}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var failure oapi.ProcessExecFailure
	require.NoError(t, json.Unmarshal(payload, &failure))
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(payload, &document))
	schema := spec.Paths.Find("/process/exec").Post.Responses.Status(422).Value.Content["application/json"].Schema.Value
	require.NoError(t, schema.VisitJSON(document))
	require.Equal(t, oapi.ProcessTimeout, failure.Code)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("partial")), *failure.StdoutB64)
}

func TestProcessTimeoutAfterParentExitStopsChild(t *testing.T) {
	t.Parallel()
	svc := newProcessTestService(t, ProcessOutputConfig{})
	args := []string{"-c", "sleep 30 & echo $!"}
	resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &oapi.ProcessExecRequest{Command: "sh", Args: &args, TimeoutSec: ptrOf(1)}})
	require.NoError(t, err)
	var output string
	switch r := resp.(type) {
	case oapi.ProcessExec422JSONResponse:
		output = *r.StdoutB64
	case oapi.ProcessExec200JSONResponse:
		output = *r.StdoutB64
	}
	data, err := base64.StdEncoding.DecodeString(output)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	require.IsType(t, oapi.ProcessExec422JSONResponse{}, resp)
	require.Equal(t, oapi.ProcessTimeout, resp.(oapi.ProcessExec422JSONResponse).Code)
	require.Eventually(t, func() bool {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return os.IsNotExist(err) || strings.Contains(string(stat), ") Z ")
	}, time.Second, 10*time.Millisecond, "child %d is still running", pid)
}

func TestProcessRetentionStopsBlockedHTTPStream(t *testing.T) {
	t.Parallel()
	svc := newProcessTestService(t, ProcessOutputConfig{})
	spawn := spawnProcess(t, svc, "head", "-c", "16777216", "/dev/zero")
	finished := make(chan struct{})
	handler := chiMiddleware.Logger(oapi.Handler(oapi.NewStrictHandler(svc, nil)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(1024))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "GET /process/%s/stdout/stream HTTP/1.1\r\nHost: localhost\r\n\r\n", spawn.ProcessId.String())
	require.NoError(t, err)
	status, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, status, "200")
	waitProcessExit(t, svc, spawn)
	select {
	case <-finished:
	case <-time.After(12 * time.Second):
		t.Fatal("HTTP stream stayed blocked after output retention expired")
	}
	resp, err := svc.ProcessStatus(context.Background(), oapi.ProcessStatusRequestObject{ProcessId: *spawn.ProcessId})
	require.NoError(t, err)
	require.IsType(t, oapi.ProcessStatus404JSONResponse{}, resp)
}

func TestProcessCursorlessReconnectDoesNotReplay(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	spawn := spawnProcess(t, svc, "printf", "once")
	output, events := collectProcessStream(t, processStream(t, svc, spawn))
	require.Equal(t, "once", output)
	requireProcessExitEvent(t, events, 0)
	output, events = collectProcessStream(t, processStream(t, svc, spawn))
	require.Empty(t, output)
	require.Empty(t, events)
}

func TestProcessTTYDoesNotRequireOutputStorage(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{Dir: filepath.Join(t.TempDir(), "missing")})
	resp, err := svc.ProcessSpawn(context.Background(), oapi.ProcessSpawnRequestObject{Body: &oapi.ProcessSpawnRequest{Command: "true", AllocateTty: ptrOf(true)}})
	require.NoError(t, err)
	require.IsType(t, oapi.ProcessSpawn200JSONResponse{}, resp)
	spawn := resp.(oapi.ProcessSpawn200JSONResponse)
	waitProcessExit(t, svc, spawn)
	_, events := collectProcessStream(t, processStream(t, svc, spawn))
	requireProcessExitEvent(t, events, 0)
}

func TestProcessExitedStatusIncludesCaptureFailure(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{MaxBytes: 1024, TotalMaxBytes: 1024 * 100})
	for i := 0; i < 50; i++ {
		spawn := spawnProcess(t, svc, "head", "-c", "8192", "/dev/zero")
		require.Eventually(t, func() bool {
			resp, err := svc.ProcessStatus(context.Background(), oapi.ProcessStatusRequestObject{ProcessId: *spawn.ProcessId})
			require.NoError(t, err)
			status := resp.(oapi.ProcessStatus200JSONResponse)
			if *status.State != "exited" {
				return false
			}
			require.NotNil(t, status.TerminationReason)
			require.Equal(t, oapi.OutputLimit, *status.TerminationReason)
			require.True(t, *status.OutputTruncated)
			return true
		}, time.Second, time.Millisecond)
	}
}

func TestProcessCursorlessReconnectContinuesUnreadOutput(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	spawn := spawnProcess(t, svc, "sh", "-c", "printf first; read line; printf second")
	finished := make(chan struct{})
	handler := oapi.Handler(oapi.NewStrictHandler(svc, nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(server.URL + "/process/" + spawn.ProcessId.String() + "/stdout/stream")
	require.NoError(t, err)
	body := resp.Body
	defer body.Close()
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			var event oapi.ProcessStreamEvent
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event))
			require.Equal(t, base64.StdEncoding.EncodeToString([]byte("first")), *event.DataB64)
			break
		}
	}
	require.NoError(t, scanner.Err())
	require.NoError(t, body.Close())
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("disconnected stream did not stop")
	}
	_, err = svc.ProcessStdin(context.Background(), oapi.ProcessStdinRequestObject{ProcessId: *spawn.ProcessId, Body: &oapi.ProcessStdinRequest{DataB64: base64.StdEncoding.EncodeToString([]byte("go\n"))}})
	require.NoError(t, err)
	output, events := collectProcessStream(t, processStream(t, svc, spawn))
	require.Equal(t, "second", output)
	requireProcessExitEvent(t, events, 0)
	replay, replayEvents := collectProcessStream(t, processStream(t, svc, spawn, oapi.ProcessStdoutStreamParams{Offset: ptrOf(int64(0))}))
	require.Equal(t, "firstsecond", replay)
	requireProcessExitEvent(t, replayEvents, 0)
}

func TestProcessManualSignalTargetsLeader(t *testing.T) {
	svc := newProcessTestService(t, ProcessOutputConfig{})
	spawn := spawnProcess(t, svc, "sh", "-c", `trap 'printf parent-signal; exit 17' HUP
sh -c 'trap "printf child-signal" HUP; printf ready; while :; do sleep 1; done' &
wait`)
	body := processStream(t, svc, spawn)
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			var event oapi.ProcessStreamEvent
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event))
			require.Equal(t, base64.StdEncoding.EncodeToString([]byte("ready")), *event.DataB64)
			break
		}
	}
	require.NoError(t, scanner.Err())
	response, err := svc.ProcessKill(context.Background(), oapi.ProcessKillRequestObject{ProcessId: *spawn.ProcessId, Body: &oapi.ProcessKillRequest{Signal: "HUP"}})
	require.NoError(t, err)
	require.IsType(t, oapi.ProcessKill200JSONResponse{}, response)
	var output strings.Builder
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			var event oapi.ProcessStreamEvent
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event))
			if event.DataB64 != nil {
				data, err := base64.StdEncoding.DecodeString(*event.DataB64)
				require.NoError(t, err)
				output.Write(data)
			} else {
				require.Equal(t, 17, *event.ExitCode)
			}
		}
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, "parent-signal", output.String())
}
