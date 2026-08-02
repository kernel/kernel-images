package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// browserReplBundlePath is built once per test run from the TypeScript
// runtime sources, mirroring how the Docker images bundle the daemon.
var (
	browserReplBundleOnce sync.Once
	browserReplBundlePath string
	browserReplBundleErr  error
)

func ensureBrowserReplBundle(t *testing.T) string {
	t.Helper()
	browserReplBundleOnce.Do(func() {
		if _, err := exec.LookPath("node"); err != nil {
			browserReplBundleErr = fmt.Errorf("node not available: %w", err)
			return
		}
		if _, err := exec.LookPath("esbuild"); err != nil {
			browserReplBundleErr = fmt.Errorf("esbuild not available: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "browser-repl-bundle")
		if err != nil {
			browserReplBundleErr = err
			return
		}
		browserReplBundlePath = filepath.Join(dir, "browser-repl.js")
		cmd := exec.Command("esbuild",
			"runtime/browser-repl.ts",
			"--bundle",
			"--platform=node",
			"--target=node22",
			"--format=cjs",
			"--supported:dynamic-import=true",
			"--outfile="+browserReplBundlePath,
			"--external:esbuild",
		)
		cmd.Dir = serverRootDir()
		if out, err := cmd.CombinedOutput(); err != nil {
			browserReplBundleErr = fmt.Errorf("esbuild failed: %w\n%s", err, out)
		}
	})
	if browserReplBundleErr != nil {
		t.Skipf("cannot build browser REPL bundle: %v", browserReplBundleErr)
	}
	return browserReplBundlePath
}

func serverRootDir() string {
	// cmd/api/api -> server root is three directories up.
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..", "..")
}

// newBrowserReplSvc builds an ApiService wired to a freshly built REPL
// bundle on a unique socket, and registers cleanup that kills the child.
func newBrowserReplSvc(t *testing.T) *ApiService {
	t.Helper()
	script := ensureBrowserReplBundle(t)

	t.Setenv("BROWSER_REPL_SCRIPT", script)
	t.Setenv("BROWSER_REPL_SOCKET", filepath.Join(t.TempDir(), "browser-repl.sock"))
	if os.Getenv("NODE_PATH") == "" {
		if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
			root := string(out)
			// trim trailing newline
			for len(root) > 0 && (root[len(root)-1] == '\n' || root[len(root)-1] == '\r') {
				root = root[:len(root)-1]
			}
			if root != "" {
				t.Setenv("NODE_PATH", root)
			}
		}
	}

	svc, err := newSvc(t, recorder.NewFFmpegManager())
	require.NoError(t, err)
	t.Cleanup(func() {
		svc.browserReplMu.Lock()
		svc.terminateBrowserReplLocked(context.Background(), "test cleanup")
		svc.browserReplMu.Unlock()
	})
	return svc
}

func execBrowserCode(t *testing.T, svc *ApiService, body *oapi.ExecuteBrowserCodeJSONRequestBody) oapi.ExecuteBrowserCode200JSONResponse {
	t.Helper()
	resp, err := svc.ExecuteBrowserCode(context.Background(), oapi.ExecuteBrowserCodeRequestObject{Body: body})
	require.NoError(t, err)
	typed, ok := resp.(oapi.ExecuteBrowserCode200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	return typed
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func TestBrowserReplValidation(t *testing.T) {
	svc := newBrowserReplSvc(t)

	resp, err := svc.ExecuteBrowserCode(context.Background(), oapi.ExecuteBrowserCodeRequestObject{Body: nil})
	require.NoError(t, err)
	require.IsType(t, oapi.ExecuteBrowserCode400JSONResponse{}, resp)

	empty := ""
	resp, err = svc.ExecuteBrowserCode(context.Background(), oapi.ExecuteBrowserCodeRequestObject{
		Body: &oapi.ExecuteBrowserCodeJSONRequestBody{Code: empty},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.ExecuteBrowserCode400JSONResponse{}, resp)

	// No REPL should have been started for invalid requests.
	require.Nil(t, svc.browserRepl)
}

func TestBrowserReplPersistenceAndStableID(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "var counter = 40; counter + 2"})
	require.True(t, r1.Success, "error: %v", r1.Error)
	require.Equal(t, float64(42), r1.Result)
	require.NotEmpty(t, r1.ReplId)

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "counter"})
	require.True(t, r2.Success)
	require.Equal(t, float64(40), r2.Result)
	require.Equal(t, r1.ReplId, r2.ReplId, "repl_id must remain stable across calls")

	r3 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "const added = await Promise.resolve(5); added"})
	require.True(t, r3.Success, "top-level await failed: %v", r3.Error)
	require.Equal(t, float64(5), r3.Result)

	r4 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "added + counter"})
	require.True(t, r4.Success)
	require.Equal(t, float64(45), r4.Result)

	r5 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `const { readFileSync } = await import("fs"); typeof readFileSync`})
	require.True(t, r5.Success, "dynamic import failed: %v", r5.Error)
	require.Equal(t, "function", r5.Result)

	// repl.id exposes the same ID the API reports.
	r6 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r6.Success)
	require.Equal(t, r1.ReplId, r6.Result)
}

func TestBrowserReplReset(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "var ephemeral = 1; ephemeral"})
	require.True(t, r1.Success)
	oldID := r1.ReplId
	oldPid := svc.browserRepl.cmd.Process.Pid

	reset := true
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "", Reset: &reset})
	require.True(t, r2.Success)
	require.NotEmpty(t, r2.ReplId)
	require.NotEqual(t, oldID, r2.ReplId, "reset must generate a new CUID2")
	require.False(t, processAlive(oldPid), "reset must kill the previous REPL process")

	r3 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "typeof ephemeral"})
	require.True(t, r3.Success)
	require.Equal(t, "undefined", r3.Result, "reset must clear all prior bindings")
	require.Equal(t, r2.ReplId, r3.ReplId)
}

func TestBrowserReplErrorKeepsREPL(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "var survives = true; throw new Error('boom')"})
	require.False(t, r1.Success)
	require.NotNil(t, r1.Error)
	require.Contains(t, *r1.Error, "boom")
	require.True(t, r1.ReplTerminated == nil || !*r1.ReplTerminated, "ordinary exceptions must not terminate the REPL")

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "survives"})
	require.True(t, r2.Success)
	require.Equal(t, true, r2.Result, "bindings initialized before the failure remain available")
	require.Equal(t, r1.ReplId, r2.ReplId)
}

func TestBrowserReplTimeoutTerminates(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r1.Success)
	oldID := r1.ReplId
	oldPid := svc.browserRepl.cmd.Process.Pid

	timeoutSec := 1
	start := time.Now()
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code:       "while (true) {}",
		TimeoutSec: &timeoutSec,
	})
	elapsed := time.Since(start)

	require.False(t, r2.Success)
	require.Equal(t, oldID, r2.ReplId, "a timeout response carries the terminated REPL's ID")
	require.NotNil(t, r2.ReplTerminated)
	require.True(t, *r2.ReplTerminated)
	require.Less(t, elapsed, 30*time.Second, "the parent must kill an uninterruptible loop promptly")
	require.False(t, processAlive(oldPid), "timeout must kill the REPL process")

	// No replacement is started until the next request.
	require.Nil(t, svc.browserRepl)

	r3 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
	require.True(t, r3.Success)
	require.NotEqual(t, oldID, r3.ReplId, "the next request lazily starts a fresh REPL")
}

func TestBrowserReplCrashRecovery(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "var before = 1"})
	require.True(t, r1.Success)

	// Simulate an OOM kill of the child.
	require.NoError(t, svc.browserRepl.cmd.Process.Kill())
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(svc.browserRepl.cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "'recovered'"})
	require.True(t, r2.Success, "error: %v", r2.Error)
	require.NotEqual(t, r1.ReplId, r2.ReplId, "crash recovery must use a new CUID2")
}

func TestBrowserReplShutdownKillsChild(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r1.Success)
	pid := svc.browserRepl.cmd.Process.Pid
	require.True(t, processAlive(pid))

	require.NoError(t, svc.Shutdown(context.Background()))

	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	require.False(t, processAlive(pid), "API shutdown must kill the REPL child")
	require.Nil(t, svc.browserRepl)
}

func TestBrowserReplSerializesConcurrentRequests(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "globalThis.seq = []; 1"})
	require.True(t, r1.Success)

	const n = 8
	results := make([]float64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := svc.ExecuteBrowserCode(context.Background(), oapi.ExecuteBrowserCodeRequestObject{
				Body: &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "seq.push(seq.length); seq[seq.length - 1]"},
			})
			assert.NoError(t, err)
			if typed, ok := resp.(oapi.ExecuteBrowserCode200JSONResponse); ok && typed.Success {
				if v, ok := typed.Result.(float64); ok {
					results[int(v)] = float64(v)
				}
			}
		}()
	}
	wg.Wait()

	// Strict serialization: the pushed values form a dense 0..n-1 sequence.
	seen := map[int]bool{}
	for _, v := range results {
		seen[int(v)] = true
	}
	require.Len(t, seen, n, "expected %d distinct sequential values, got %v", n, results)
	for i := 0; i < n; i++ {
		require.True(t, seen[i], "missing sequence value %d in %v", i, results)
	}
}

func TestBrowserReplContentOrdering(t *testing.T) {
	svc := newBrowserReplSvc(t)

	code := `
		repl.write("a");
		console.log("b", 1);
		console.error("c");
		repl.write({ k: 1 });
		const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", "base64");
		await repl.emitImage(png);
		repl.write("after");
		"done"
	`
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "done", r.Result)
	require.NotNil(t, r.Content)
	require.Len(t, *r.Content, 6)

	textAt := func(i int) oapi.BrowserExecutionTextContent {
		t.Helper()
		v, err := (*r.Content)[i].AsBrowserExecutionTextContent()
		require.NoError(t, err, "content item %d should be text", i)
		return v
	}

	require.Equal(t, "write", string(textAt(0).Channel))
	require.Equal(t, "a", textAt(0).Text)
	require.Equal(t, "stdout", string(textAt(1).Channel))
	require.Equal(t, "b 1", textAt(1).Text)
	require.Equal(t, "stderr", string(textAt(2).Channel))
	require.Equal(t, "c", textAt(2).Text)
	require.Equal(t, "write", string(textAt(3).Channel))
	require.Equal(t, "{ k: 1 }", textAt(3).Text)

	img, err := (*r.Content)[4].AsBrowserExecutionImageContent()
	require.NoError(t, err, "content item 4 should be an image")
	require.Equal(t, "image/png", img.MimeType)
	require.NotEmpty(t, img.DataB64)

	require.Equal(t, "write", string(textAt(5).Channel))
	require.Equal(t, "after", textAt(5).Text)
}

func TestBrowserReplImageValidation(t *testing.T) {
	svc := newBrowserReplSvc(t)

	// Non-image data URLs are rejected.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `await repl.emitImage("data:text/html;base64,PGI+eDwvYj4=")`,
	})
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "image/*")

	// Non-image bytes are rejected by MIME sniffing.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `await repl.emitImage(Buffer.from("not an image at all"))`,
	})
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "unrecognized image data")

	// The REPL survives user-code exceptions.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "'alive'"})
	require.True(t, r.Success)
}

func TestBrowserReplTruncation(t *testing.T) {
	svc := newBrowserReplSvc(t)

	// Text output beyond 256 KiB is truncated and flagged.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `repl.write("x".repeat(400 * 1024)); "ok"`,
	})
	require.True(t, r.Success)
	require.NotNil(t, r.ContentTruncated)
	require.True(t, *r.ContentTruncated)
	require.NotNil(t, r.Content)
	v, err := (*r.Content)[0].AsBrowserExecutionTextContent()
	require.NoError(t, err)
	require.LessOrEqual(t, len(v.Text), 256*1024)

	// Oversized results fall back to a bounded repr and are flagged.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `"y".repeat(400 * 1024)`,
	})
	require.True(t, r.Success)
	require.NotNil(t, r.ResultTruncated)
	require.True(t, *r.ResultTruncated)
	require.NotNil(t, r.ResultRepr)
}

func TestBrowserReplResultEdgeValues(t *testing.T) {
	svc := newBrowserReplSvc(t)

	// BigInt is not JSON-compatible: bounded repr, no crash.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "10n"})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Contains(t, *r.ResultRepr, "10n")

	// Circular values fall back to repr.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "const c = {}; c.self = c; c"})
	require.True(t, r.Success)
	require.NotNil(t, r.ResultRepr)
	require.Contains(t, *r.ResultRepr, "Circular")

	// undefined result is omitted.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "undefined"})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.Nil(t, r.ResultRepr)
}
