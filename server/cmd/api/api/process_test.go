package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os/user"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/require"
)

func TestProcessExec(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle)}

	cmd := "sh"
	args := []string{"-c", "printf out; printf err 1>&2; exit 3"}
	body := &oapi.ProcessExecRequest{Command: cmd, Args: &args}
	resp, err := svc.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: body})
	require.NoError(t, err, "ProcessExec error")
	r200, ok := resp.(oapi.ProcessExec200JSONResponse)
	require.True(t, ok, "unexpected resp type: %T", resp)
	require.NotNil(t, r200.ExitCode, "missing exit code")
	require.Equal(t, 3, *r200.ExitCode, "exit code mismatch")
	require.NotNil(t, r200.StdoutB64, "missing stdout in response")
	require.NotNil(t, r200.StderrB64, "missing stderr in response")
	out, _ := base64.StdEncoding.DecodeString(*r200.StdoutB64)
	errB, _ := base64.StdEncoding.DecodeString(*r200.StderrB64)
	require.Equal(t, "out", string(out), "stdout mismatch")
	require.Equal(t, "err", string(errB), "stderr mismatch")
}

func TestProcessSpawnStatusAndStream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle), stz: scaletozero.NewNoopController()}

	// Spawn a short-lived process that emits stdout and stderr then exits
	cmd := "sh"
	args := []string{"-c", "printf ABC; sleep 0.05; printf DEF 1>&2; sleep 0.05; exit 0"}
	body := &oapi.ProcessSpawnRequest{Command: cmd, Args: &args}
	spawnResp, err := svc.ProcessSpawn(ctx, oapi.ProcessSpawnRequestObject{Body: body})
	require.NoError(t, err, "ProcessSpawn error")
	s200, ok := spawnResp.(oapi.ProcessSpawn200JSONResponse)
	require.True(t, ok, "unexpected spawn resp type: %T", spawnResp)
	require.NotNil(t, s200.ProcessId, "missing ProcessId in spawn resp")
	require.NotNil(t, s200.Pid, "missing Pid in spawn resp")

	// Status should be running initially (may race to exited; tolerate both by not asserting)
	statusResp, err := svc.ProcessStatus(ctx, oapi.ProcessStatusRequestObject{ProcessId: *s200.ProcessId})
	require.NoError(t, err, "ProcessStatus error")
	_, ok = statusResp.(oapi.ProcessStatus200JSONResponse)
	require.True(t, ok, "unexpected status resp: %T", statusResp)

	// Start stream reader and collect at least two data events and one exit event
	streamResp, err := svc.ProcessStdoutStream(ctx, oapi.ProcessStdoutStreamRequestObject{ProcessId: *s200.ProcessId})
	require.NoError(t, err, "StdoutStream error")
	st200, ok := streamResp.(oapi.ProcessStdoutStream200TexteventStreamResponse)
	require.True(t, ok, "unexpected stream resp: %T", streamResp)

	reader := bufio.NewReader(st200.Body)
	var gotStdout, gotStderr, gotExit bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !(gotStdout && gotStderr && gotExit) {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			require.NoError(t, err, "read SSE line")
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		var evt oapi.ProcessStreamEvent
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			require.NoError(t, err, "unmarshal event")
		}
		if evt.Stream != nil && *evt.Stream == "stdout" && evt.DataB64 != nil {
			b, _ := base64.StdEncoding.DecodeString(*evt.DataB64)
			if strings.Contains(string(b), "ABC") {
				gotStdout = true
			}
		}
		if evt.Stream != nil && *evt.Stream == "stderr" && evt.DataB64 != nil {
			b, _ := base64.StdEncoding.DecodeString(*evt.DataB64)
			if strings.Contains(string(b), "DEF") {
				gotStderr = true
			}
		}
		if evt.Event != nil && *evt.Event == "exit" {
			gotExit = true
		}
		// consume blank line
		_, _ = reader.ReadString('\n')
	}
	require.True(t, gotStdout && gotStderr && gotExit, "missing events: stdout=%v stderr=%v exit=%v", gotStdout, gotStderr, gotExit)
}

func TestProcessStdinAndExit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle), stz: scaletozero.NewNoopController()}

	// Spawn a process that reads exactly 3 bytes then exits
	cmd := "sh"
	args := []string{"-c", "dd of=/dev/null bs=1 count=3 status=none"}
	body := &oapi.ProcessSpawnRequest{Command: cmd, Args: &args}
	spawnResp, err := svc.ProcessSpawn(ctx, oapi.ProcessSpawnRequestObject{Body: body})
	require.NoError(t, err, "ProcessSpawn error")
	s200, ok := spawnResp.(oapi.ProcessSpawn200JSONResponse)
	require.True(t, ok, "unexpected spawn resp: %T", spawnResp)
	require.NotNil(t, s200.ProcessId, "missing ProcessId in spawn resp")

	// Write 3 bytes
	data := base64.StdEncoding.EncodeToString([]byte("xyz"))
	stdinResp, err := svc.ProcessStdin(ctx, oapi.ProcessStdinRequestObject{ProcessId: *s200.ProcessId, Body: &oapi.ProcessStdinRequest{DataB64: data}})
	require.NoError(t, err, "ProcessStdin error")
	st200, ok := stdinResp.(oapi.ProcessStdin200JSONResponse)
	require.True(t, ok, "unexpected stdin resp type: %T", stdinResp)
	require.NotNil(t, st200.WrittenBytes, "missing WrittenBytes in stdin resp")
	require.Equal(t, 3, *st200.WrittenBytes, "written bytes mismatch")

	// Wait for exit via status polling
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := svc.ProcessStatus(ctx, oapi.ProcessStatusRequestObject{ProcessId: *s200.ProcessId})
		require.NoError(t, err, "ProcessStatus error")
		sr, ok := resp.(oapi.ProcessStatus200JSONResponse)
		require.True(t, ok, "unexpected status resp: %T", resp)
		if sr.State != nil && *sr.State == "exited" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, false, "process did not exit in time")
}

func TestProcessKill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle), stz: scaletozero.NewNoopController()}

	cmd := "sh"
	args := []string{"-c", "sleep 5"}
	body := &oapi.ProcessSpawnRequest{Command: cmd, Args: &args}
	spawnResp, err := svc.ProcessSpawn(ctx, oapi.ProcessSpawnRequestObject{Body: body})
	require.NoError(t, err, "ProcessSpawn error")
	s200, ok := spawnResp.(oapi.ProcessSpawn200JSONResponse)
	require.True(t, ok, "unexpected spawn resp: %T", spawnResp)
	require.NotNil(t, s200.ProcessId, "missing ProcessId in spawn resp")

	// Send KILL
	killBody := &oapi.ProcessKillRequest{Signal: "KILL"}
	killResp, err := svc.ProcessKill(ctx, oapi.ProcessKillRequestObject{ProcessId: *s200.ProcessId, Body: killBody})
	require.NoError(t, err, "ProcessKill error")
	_, ok = killResp.(oapi.ProcessKill200JSONResponse)
	require.True(t, ok, "unexpected kill resp: %T", killResp)

	// Verify exited
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := svc.ProcessStatus(ctx, oapi.ProcessStatusRequestObject{ProcessId: *s200.ProcessId})
		require.NoError(t, err, "ProcessStatus error")
		sr, ok := resp.(oapi.ProcessStatus200JSONResponse)
		require.True(t, ok, "unexpected status resp: %T", resp)
		if sr.State != nil && *sr.State == "exited" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, false, "process not killed in time")
}

func TestProcessNotFoundRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle)}

	// random id that will not exist
	id := openapi_types.UUID(uuid.New())
	if resp, _ := svc.ProcessStatus(ctx, oapi.ProcessStatusRequestObject{ProcessId: id}); true {
		require.NotNil(t, resp, "expected a response")
		_, ok := resp.(oapi.ProcessStatus404JSONResponse)
		require.True(t, ok, "expected 404, got %T", resp)
	}
	if resp, _ := svc.ProcessStdoutStream(ctx, oapi.ProcessStdoutStreamRequestObject{ProcessId: id}); true {
		require.NotNil(t, resp, "expected a response")
		_, ok := resp.(oapi.ProcessStdoutStream404JSONResponse)
		require.True(t, ok, "expected 404, got %T", resp)
	}
}

func TestProcessExec_CommandNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle)}

	body := &oapi.ProcessExecRequest{Command: "nonexistent_binary_that_does_not_exist"}
	resp, err := svc.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: body})
	require.NoError(t, err)
	_, ok := resp.(oapi.ProcessExec400JSONResponse)
	require.True(t, ok, "expected 400 for nonexistent command, got %T", resp)
}

func TestProcessSpawn_CommandNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle), stz: scaletozero.NewNoopController()}

	body := &oapi.ProcessSpawnRequest{Command: "nonexistent_binary_that_does_not_exist"}
	resp, err := svc.ProcessSpawn(ctx, oapi.ProcessSpawnRequestObject{Body: body})
	require.NoError(t, err)
	_, ok := resp.(oapi.ProcessSpawn400JSONResponse)
	require.True(t, ok, "expected 400 for nonexistent command, got %T", resp)
}

func TestBuildCmd_AsRootSetsCredential(t *testing.T) {
	t.Parallel()
	asRoot := true
	body := &oapi.ProcessExecRequest{Command: "true", AsRoot: &asRoot}
	cmd, err := buildCmd(body)
	require.NoError(t, err, "buildCmd returned error")
	require.NotNil(t, cmd.SysProcAttr, "expected SysProcAttr to be set for AsRoot")
	require.NotNil(t, cmd.SysProcAttr.Credential, "expected SysProcAttr.Credential to be set for AsRoot")
	require.Equal(t, uint32(0), cmd.SysProcAttr.Credential.Uid, "expected root uid")
	require.Equal(t, uint32(0), cmd.SysProcAttr.Credential.Gid, "expected root gid")
}

func TestBuildCmd_AsUserUidAndGidOverride(t *testing.T) {
	t.Parallel()
	cur, err := user.Current()
	if err != nil {
		t.Skipf("skipping: failed to determine current user: %v", err)
	}
	// Use numeric uid with an explicit gid override to exercise parsing path
	spec := cur.Uid + ":0" // override gid to 0 for determinism; we're not executing
	body := &oapi.ProcessExecRequest{Command: "true", AsUser: &spec}
	cmd, err := buildCmd(body)
	require.NoError(t, err, "buildCmd returned error")
	require.NotNil(t, cmd.SysProcAttr, "expected SysProcAttr to be set for AsUser")
	require.NotNil(t, cmd.SysProcAttr.Credential, "expected SysProcAttr.Credential to be set for AsUser")
	// Verify uid matches the looked-up uid and gid matches the override
	wantUID64, err := strconv.ParseUint(cur.Uid, 10, 32)
	require.NoError(t, err, "parse current uid")
	if cmd.SysProcAttr.Credential.Uid != uint32(wantUID64) {
		require.Equal(t, uint32(wantUID64), cmd.SysProcAttr.Credential.Uid, "uid mismatch")
	}
	if cmd.SysProcAttr.Credential.Gid != 0 {
		require.Equal(t, uint32(0), cmd.SysProcAttr.Credential.Gid, "gid override mismatch")
	}
}

func TestBuildCmd_AsRootAndAsUserConflict(t *testing.T) {
	t.Parallel()
	asRoot := true
	asUser := "0"
	body := &oapi.ProcessExecRequest{Command: "true", AsRoot: &asRoot, AsUser: &asUser}
	_, err := buildCmd(body)
	require.Error(t, err, "expected error when both as_root and as_user are set")
}
