package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// browserReplBundlePath is built once per test run from the runtime sources,
// mirroring how the Docker images bundle the daemon.
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

		// Stage the runtime package and sources exactly like the Docker builds.
		// Meriyah is installed from the exact package lock, never globally.
		stagingDir, err := os.MkdirTemp("", "browser-repl-runtime")
		if err != nil {
			browserReplBundleErr = err
			return
		}
		defer os.RemoveAll(stagingDir)
		entries, err := os.ReadDir(filepath.Join(serverRootDir(), "runtime"))
		if err != nil {
			browserReplBundleErr = err
			return
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".ts") && name != "package.json" && name != "package-lock.json" {
				continue
			}
			data, readErr := os.ReadFile(filepath.Join(serverRootDir(), "runtime", name))
			if readErr != nil {
				browserReplBundleErr = readErr
				return
			}
			if writeErr := os.WriteFile(filepath.Join(stagingDir, name), data, 0o644); writeErr != nil {
				browserReplBundleErr = writeErr
				return
			}
		}
		npm := exec.Command("npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--omit=dev")
		npm.Dir = stagingDir
		if out, err := npm.CombinedOutput(); err != nil {
			browserReplBundleErr = fmt.Errorf("npm ci failed: %w\n%s", err, out)
			return
		}

		cmd := exec.Command("esbuild",
			"browser-repl.ts",
			"--bundle",
			"--platform=node",
			"--target=node22",
			"--format=cjs",
			"--supported:dynamic-import=true",
			"--outfile="+browserReplBundlePath,
		)
		cmd.Dir = stagingDir
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
	require.NotNil(t, r2.Error)
	// The uninterruptible path uses the same message as the daemon's own
	// interruptible timeout.
	require.Contains(t, *r2.Error, "execution timed out after 1000ms")
	// Read deadline (timeout + grace) plus an immediate SIGKILL: well under
	// the old SIGTERM-grace-then-SIGKILL wall time.
	require.Less(t, elapsed, 10*time.Second, "the parent must kill an uninterruptible loop promptly")
	require.False(t, processAlive(oldPid), "timeout must kill the REPL process")

	// No replacement is started until the next request.
	require.Nil(t, svc.browserRepl)

	r3 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
	require.True(t, r3.Success)
	require.NotEqual(t, oldID, r3.ReplId, "the next request lazily starts a fresh REPL")
}

// TestBrowserReplInterruptibleTimeoutTerminates covers the spec's
// destructive-timeout contract for executions the daemon CAN interrupt at the
// event-loop level (e.g. an unresolved promise). The daemon reports
// timed_out and the API must still kill the process group, return the
// terminated repl_id with repl_terminated: true, and guarantee that output
// from the abandoned execution cannot leak into a later one.
func TestBrowserReplInterruptibleTimeoutTerminates(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r1.Success)
	oldID := r1.ReplId
	oldPid := svc.browserRepl.cmd.Process.Pid

	timeoutSec := 1
	start := time.Now()
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code:       `setTimeout(() => repl.write("LEAKED-LATE-OUTPUT"), 2000); await new Promise(() => {})`,
		TimeoutSec: &timeoutSec,
	})
	elapsed := time.Since(start)

	require.False(t, r2.Success)
	require.NotNil(t, r2.Error)
	require.Contains(t, *r2.Error, "timed out")
	require.Equal(t, oldID, r2.ReplId, "a timeout response carries the terminated REPL's ID")
	require.NotNil(t, r2.ReplTerminated)
	require.True(t, *r2.ReplTerminated, "an interruptible timeout is still destructive")
	require.Less(t, elapsed, 15*time.Second, "a daemon-side timeout must answer promptly")
	require.False(t, processAlive(oldPid), "timeout must kill the REPL process")
	require.Nil(t, svc.browserRepl, "no replacement starts until the next request")

	// Let the (now dead) timer fire; its output must not leak into the next
	// execution's content.
	time.Sleep(2500 * time.Millisecond)
	r3 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
	require.True(t, r3.Success)
	require.NotEqual(t, oldID, r3.ReplId, "the next request lazily starts a fresh REPL")
	if r3.Content != nil {
		for _, item := range *r3.Content {
			if txt, err := item.AsBrowserExecutionTextContent(); err == nil {
				require.NotContains(t, txt.Text, "LEAKED-LATE-OUTPUT",
					"output from a terminated execution must not leak into a later execution")
			}
		}
	}
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

	// The documented ImageInput forms are accepted even though user code
	// constructs them in the vm context's realm, whose Uint8Array and
	// ArrayBuffer intrinsics differ from the daemon's (a cross-realm
	// instanceof check previously rejected a direct Uint8Array).
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: fmt.Sprintf(`
			const png = Buffer.from(%q, "base64");
			const u8 = new Uint8Array(png);                       // context-realm Uint8Array
			await repl.emitImage(u8);                             // direct Uint8Array
			await repl.emitImage(u8.buffer);                      // direct ArrayBuffer (context realm)
			await repl.emitImage(new DataView(u8.buffer));        // DataView
			await repl.emitImage({ bytes: new Uint8Array(png) }); // bytes form
			await repl.emitImage({ bytes: u8.buffer, mimeType: "image/png" });
			"image-inputs-ok"
		`, fakeCDPTinyPNG),
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "image-inputs-ok", r.Result)
	require.NotNil(t, r.Content)
	imageCount := 0
	for _, item := range *r.Content {
		if img, err := item.AsBrowserExecutionImageContent(); err == nil && img.Type == "image" {
			imageCount++
			require.Equal(t, "image/png", img.MimeType)
		}
	}
	require.Equal(t, 5, imageCount, "every documented ImageInput form must emit an image")

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

	// undefined is distinguishable from null: it surfaces via result_repr.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "undefined"})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Equal(t, "undefined", *r.ResultRepr)

	// null stays a plain JSON null.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "null"})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.Nil(t, r.ResultRepr)

	// Map and RegExp are not JSON-compatible: bounded repr, not a silent {}.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `new Map([["k", "v"]])`})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Contains(t, *r.ResultRepr, "Map(1)")

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `/regex/gi`})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Contains(t, *r.ResultRepr, "/regex/gi")

	// Dates still serialize to ISO strings.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `new Date("2024-01-02T03:04:05Z")`})
	require.True(t, r.Success)
	require.Equal(t, "2024-01-02T03:04:05.000Z", r.Result)

	// Plain objects and arrays round-trip through result.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `({a: [1, 2, 3], b: "str", c: {d: null}})`})
	require.True(t, r.Success)
	plain, ok := r.Result.(map[string]any)
	require.True(t, ok, "expected object result, got %T", r.Result)
	require.Equal(t, []any{float64(1), float64(2), float64(3)}, plain["a"])
	require.Equal(t, "str", plain["b"])
	require.Equal(t, map[string]any{"d": nil}, plain["c"])

	// NaN and Infinity are not JSON-compatible (JSON.stringify would turn
	// them into null): they must surface via result_repr instead.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "NaN"})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Equal(t, "NaN", *r.ResultRepr)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1/0"})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Equal(t, "Infinity", *r.ResultRepr)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "-1/0"})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Equal(t, "-Infinity", *r.ResultRepr)

	// An error value as the result surfaces via its stack repr.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `const resultErr = new Error("result-eval-error"); resultErr`})
	require.True(t, r.Success)
	require.Nil(t, r.Result)
	require.NotNil(t, r.ResultRepr)
	require.Contains(t, *r.ResultRepr, "result-eval-error")
}

// TestBrowserReplImageSizeLimits covers the 8 MiB per-image and 16 MiB
// aggregate image limits from the spec.
func TestBrowserReplImageSizeLimits(t *testing.T) {
	svc := newBrowserReplSvc(t)

	const pngHeader = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

	// Per-image limit: an image over 8 MiB decoded is rejected.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: fmt.Sprintf(`const big = Buffer.concat([Buffer.from("%s", "base64"), Buffer.alloc(9 * 1024 * 1024)]); await repl.emitImage(big)`, pngHeader),
	})
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "per-image limit")

	// Aggregate limit: images totaling over 16 MiB drop the overflow, note
	// the drop on stderr, and flag content truncation.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: fmt.Sprintf(`
			const mk = () => Buffer.concat([Buffer.from("%s", "base64"), Buffer.alloc(6 * 1024 * 1024)]);
			await repl.emitImage(mk());
			await repl.emitImage(mk());
			await repl.emitImage(mk());
			"done"
		`, pngHeader),
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.NotNil(t, r.ContentTruncated)
	require.True(t, *r.ContentTruncated)
	require.NotNil(t, r.Content)
	images := 0
	sawDropNote := false
	for _, item := range *r.Content {
		// The generated union accessors do not enforce the discriminator, so
		// check Type explicitly.
		if img, err := item.AsBrowserExecutionImageContent(); err == nil && img.Type == "image" {
			images++
		}
		if txt, err := item.AsBrowserExecutionTextContent(); err == nil && strings.Contains(txt.Text, "aggregate response image limit") {
			sawDropNote = true
		}
	}
	require.Equal(t, 2, images, "the third 6 MiB image exceeds the 16 MiB aggregate limit")
	require.True(t, sawDropNote, "a stderr note records the dropped image")
}

// TestBrowserReplTimeoutSecValidation enforces the schema's timeout_sec
// bounds (minimum 1, maximum 300) server-side.
func TestBrowserReplTimeoutSecValidation(t *testing.T) {
	svc := newBrowserReplSvc(t)

	for _, v := range []int{-5, 0, 301, 100000} {
		resp, err := svc.ExecuteBrowserCode(context.Background(), oapi.ExecuteBrowserCodeRequestObject{
			Body: &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1", TimeoutSec: &v},
		})
		require.NoError(t, err)
		require.IsType(t, oapi.ExecuteBrowserCode400JSONResponse{}, resp, "timeout_sec=%d must be rejected", v)
	}
	require.Nil(t, svc.browserRepl, "invalid requests must not start a REPL")

	// Bounds are inclusive.
	for _, v := range []int{1, 300} {
		r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1", TimeoutSec: &v})
		require.True(t, r.Success, "timeout_sec=%d must be accepted: %v", v, r.Error)
	}
}

// ---------------------------------------------------------------------------
// Fake CDP endpoint for browser-helper coverage
// ---------------------------------------------------------------------------

// fakeCDPTarget is one entry in the fake browser's target list.
type fakeCDPTarget struct {
	ID    string
	Type  string
	Title string
	URL   string
}

// fakeCDPServer is a minimal fake of the browser-level CDP WebSocket
// endpoint. It implements just enough of the protocol (target management,
// domain enables, navigation, input, screenshots, DOM queries, and canned
// Runtime.evaluate responses) to exercise every seeded browser helper
// without a real Chromium.
type fakeCDPServer struct {
	t *testing.T

	mu      sync.Mutex
	targets []fakeCDPTarget
	nextSeq int
	conns   map[*websocket.Conn]struct{}
	// queuedEvents are flushed to the daemon after the next command
	// response, avoiding concurrent writes on the websocket.
	queuedEvents []map[string]any

	// lastKeyEvent records the params of the most recent
	// Input.dispatchKeyEvent command so tests can verify modifier bits.
	lastKeyEvent map[string]any

	// frozen simulates a renderer stuck behind a modal JavaScript dialog:
	// Runtime.evaluate commands fail instead of hanging (a real renderer
	// would never answer; failing fast keeps tests quick).
	frozen atomic.Bool
	// hangSession simulates a renderer frozen behind a modal dialog more
	// faithfully: session-routed commands are never answered (browser-level
	// commands still are), matching real Chromium behavior for a tab with a
	// stale pre-attach dialog.
	hangSession atomic.Bool
	// failNextAttach makes the next N Target.attachToTarget commands fail
	// with the stale-target error, simulating a target destroyed between
	// listing and attaching.
	failNextAttach atomic.Int32
	// closeNextConnsAfterFirstCommand makes the next N accepted WebSocket
	// connections close without answering as soon as the first command
	// arrives, simulating the DevTools proxy accepting a connection and
	// then dropping it while Chromium is still coming up after a restart.
	closeNextConnsAfterFirstCommand atomic.Int32
	// totalConns counts every accepted WebSocket connection, so tests can
	// verify a reconnect-and-retry actually happened.
	totalConns atomic.Int32

	// rendererHrefs is the href each target's renderer reports for
	// location.href. Target metadata shows a created target's URL
	// immediately, but the renderer commits the navigation later (the
	// new_tab QA finding), so the two are tracked separately.
	rendererHrefs map[string]string
	// pendingHrefPolls counts how many location.href evaluations still
	// report about:blank before the target's navigation commits in the
	// renderer.
	pendingHrefPolls map[string]int
	// delayCommit makes Target.createTarget leave the renderer on
	// about:blank for the first few location.href polls.
	delayCommit atomic.Bool
	// hangMouseWheel simulates the wedged mouseWheel path observed on rare
	// new-headless Chromium instances: mouseWheel commands are never
	// answered while every other command answers fine.
	hangMouseWheel atomic.Bool
	// sawScrollBy records that the in-page window.scrollBy fallback ran.
	sawScrollBy atomic.Bool
	// swallowNextWheel simulates the upstream first-wheel-after-navigation
	// quirk: the mouseWheel command is answered normally but the page never
	// scrolls (window.scrollY stays put).
	swallowNextWheel bool
	// delayedWheelMs, when > 0, simulates Chromium's asynchronous wheel
	// application: the mouseWheel command is answered normally but the
	// scroll offset only moves after the given delay.
	delayedWheelMs atomic.Int64
	// activatedTargets records every Target.activateTarget target id, in
	// order, so tests can verify attach foregrounds the attached tab.
	activatedTargets []string
	// scrollY is the fake page's current scroll offset; maxScrollY is how
	// far the fake document can scroll (0 = unscrollable).
	scrollY    int64
	maxScrollY int64
	// wheelDispatchCount counts answered mouseWheel dispatches.
	wheelDispatchCount int

	http *httptest.Server
}

// fakeCDPTinyPNG is a valid 1x1 PNG; capture_screenshot writes it to disk
// and repl.emitImage sniffs its magic bytes.
const fakeCDPTinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func newFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()
	f := &fakeCDPServer{
		t:                t,
		conns:            map[*websocket.Conn]struct{}{},
		rendererHrefs:    map[string]string{"target-page-1": "https://example.com/"},
		pendingHrefPolls: map[string]int{},
		targets: []fakeCDPTarget{
			{ID: "target-page-1", Type: "page", Title: "Example Domain", URL: "https://example.com/"},
			{ID: "target-internal-1", Type: "page", Title: "New Tab", URL: "chrome://newtab/"},
			{ID: "target-frame-1", Type: "iframe", Title: "Frame", URL: "https://frame.example.com/widget"},
		},
	}
	f.http = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.http.Close)
	return f
}

// wsURL returns a browser-level WebSocket URL with an explicit path so the
// daemon skips its /json/version discovery.
func (f *fakeCDPServer) wsURL() string {
	return "ws" + strings.TrimPrefix(f.http.URL, "http") + "/devtools/browser/fake"
}

// Restart closes every active daemon connection, simulating a Chromium
// restart. The daemon reconnects on its next browser helper call.
func (f *fakeCDPServer) Restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for conn := range f.conns {
		_ = conn.Close(websocket.StatusGoingAway, "chromium restart")
	}
}

func (f *fakeCDPServer) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

// lastKeyEventParams returns the params of the most recent
// Input.dispatchKeyEvent command, or nil if none was dispatched.
func (f *fakeCDPServer) lastKeyEventParams() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastKeyEvent
}

func (f *fakeCDPServer) handler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	f.totalConns.Add(1)
	f.mu.Lock()
	f.conns[conn] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.conns, conn)
		f.mu.Unlock()
		conn.CloseNow()
	}()

	ctx := r.Context()
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var req struct {
			ID        int             `json:"id"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if err := json.Unmarshal(msg, &req); err != nil || req.ID == 0 {
			continue
		}
		if f.closeNextConnsAfterFirstCommand.Load() > 0 {
			// The proxy accepted the connection but the browser behind it is
			// not up yet: drop the connection mid-first-command without
			// answering. The daemon must reconnect and retry.
			f.closeNextConnsAfterFirstCommand.Add(-1)
			return
		}
		if req.SessionID != "" && f.hangSession.Load() {
			// A frozen renderer never answers session-routed commands.
			continue
		}
		if req.Method == "Input.dispatchMouseEvent" && f.hangMouseWheel.Load() &&
			strings.Contains(string(req.Params), "mouseWheel") {
			// The wedged mouseWheel path never answers.
			continue
		}
		result, events, dispatchErr := f.dispatch(req.Method, req.Params, req.SessionID)
		var resp map[string]any
		if dispatchErr != nil {
			resp = map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": dispatchErr.Error()}}
		} else {
			resp = map[string]any{"id": req.ID, "result": result}
		}
		data, _ := json.Marshal(resp)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			return
		}
		for _, ev := range events {
			data, _ := json.Marshal(ev)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
		for _, ev := range f.takeQueuedEvents() {
			data, _ := json.Marshal(ev)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}
}

// queueEvent buffers a raw CDP event, flushed to the daemon after the next
// command response.
func (f *fakeCDPServer) queueEvent(ev map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queuedEvents = append(f.queuedEvents, ev)
}

func (f *fakeCDPServer) takeQueuedEvents() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	evs := f.queuedEvents
	f.queuedEvents = nil
	return evs
}

// dispatch handles one CDP command, returning the command result plus any
// events to emit after the response.
func (f *fakeCDPServer) dispatch(method string, params json.RawMessage, sessionID string) (any, []map[string]any, error) {
	switch method {
	case "Target.getTargets":
		f.mu.Lock()
		defer f.mu.Unlock()
		infos := make([]map[string]any, 0, len(f.targets))
		for _, tgt := range f.targets {
			infos = append(infos, map[string]any{
				"targetId": tgt.ID,
				"type":     tgt.Type,
				"title":    tgt.Title,
				"url":      tgt.URL,
				"attached": true,
			})
		}
		return map[string]any{"targetInfos": infos}, nil, nil
	case "Target.attachToTarget":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(params, &p)
		if f.failNextAttach.Load() > 0 {
			f.failNextAttach.Add(-1)
			return nil, nil, fmt.Errorf("No target with given id found")
		}
		sid := "session-" + p.TargetID
		// Emit a session-scoped event so drain_events has something to return.
		ev := map[string]any{
			"method":    "Page.loadEventFired",
			"params":    map[string]any{"timestamp": 1},
			"sessionId": sid,
		}
		return map[string]any{"sessionId": sid}, []map[string]any{ev}, nil
	case "Target.detachFromTarget":
		return map[string]any{}, nil, nil
	case "Target.activateTarget":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		f.activatedTargets = append(f.activatedTargets, p.TargetID)
		f.mu.Unlock()
		return map[string]any{}, nil, nil
	case "Target.createTarget":
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(params, &p)
		if p.URL == "" {
			p.URL = "about:blank"
		}
		f.mu.Lock()
		f.nextSeq++
		id := fmt.Sprintf("target-created-%d", f.nextSeq)
		f.targets = append(f.targets, fakeCDPTarget{ID: id, Type: "page", Title: p.URL, URL: p.URL})
		if f.delayCommit.Load() {
			// Target metadata shows the URL immediately; the renderer
			// commits the navigation a few polls later.
			f.rendererHrefs[id] = "about:blank"
			f.pendingHrefPolls[id] = 3
		} else {
			f.rendererHrefs[id] = p.URL
		}
		f.mu.Unlock()
		return map[string]any{"targetId": id}, nil, nil
	case "Target.closeTarget":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		for i, tgt := range f.targets {
			if tgt.ID == p.TargetID {
				f.targets = append(f.targets[:i], f.targets[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		return map[string]any{}, nil, nil
	case "Page.enable", "DOM.enable", "Runtime.enable", "Network.enable":
		return map[string]any{}, nil, nil
	case "Page.navigate":
		return map[string]any{"frameId": "frame-1", "loaderId": "loader-1"}, nil, nil
	case "Page.getLayoutMetrics":
		return map[string]any{
			"cssLayoutViewport": map[string]any{"clientWidth": 800, "clientHeight": 600},
			"cssContentSize":    map[string]any{"width": 800, "height": 2000},
		}, nil, nil
	case "Page.captureScreenshot":
		return map[string]any{"data": fakeCDPTinyPNG}, nil, nil
	case "Input.dispatchMouseEvent", "Input.insertText":
		if method == "Input.dispatchMouseEvent" && strings.Contains(string(params), "mouseWheel") {
			var p struct {
				DeltaY float64 `json:"deltaY"`
			}
			_ = json.Unmarshal(params, &p)
			f.mu.Lock()
			f.wheelDispatchCount++
			swallow := f.swallowNextWheel
			if swallow {
				// Answered normally, but the page never scrolls.
				f.swallowNextWheel = false
			}
			delayMs := f.delayedWheelMs.Load()
			if !swallow && delayMs <= 0 {
				f.scrollY += int64(p.DeltaY)
			}
			f.mu.Unlock()
			if !swallow && delayMs > 0 {
				// Chromium applies the wheel asynchronously after the CDP
				// command answers; scroll() must wait out a settle window
				// instead of false-positive retrying (a double scroll).
				delta := int64(p.DeltaY)
				time.AfterFunc(time.Duration(delayMs)*time.Millisecond, func() {
					f.mu.Lock()
					f.scrollY += delta
					f.mu.Unlock()
				})
			}
		}
		return map[string]any{}, nil, nil
	case "Input.dispatchKeyEvent":
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		f.lastKeyEvent = p
		f.mu.Unlock()
		return map[string]any{}, nil, nil
	case "DOM.getDocument":
		return map[string]any{"root": map[string]any{"nodeId": 1}}, nil, nil
	case "DOM.querySelector":
		return map[string]any{"nodeId": 42}, nil, nil
	case "DOM.setFileInputFiles":
		return map[string]any{}, nil, nil
	case "Runtime.evaluate":
		if f.frozen.Load() {
			return nil, nil, fmt.Errorf("renderer is frozen behind a modal dialog")
		}
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(params, &p)
		return map[string]any{
			"result": map[string]any{"type": "object", "value": f.evalExpression(p.Expression, sessionID)},
		}, nil, nil
	}
	return nil, nil, fmt.Errorf("fake CDP: unhandled method %s", method)
}

// evalExpression returns canned values for the expressions the helpers
// evaluate in the page, and echoes anything else so js() round-trips can be
// verified.
func (f *fakeCDPServer) evalExpression(expr string, sessionID string) any {
	switch {
	case expr == "location.href":
		// The renderer-level href for the session's target, honoring a
		// delayed navigation commit (see delayCommit).
		targetID := strings.TrimPrefix(sessionID, "session-")
		f.mu.Lock()
		defer f.mu.Unlock()
		if remaining, ok := f.pendingHrefPolls[targetID]; ok {
			if remaining > 1 {
				f.pendingHrefPolls[targetID] = remaining - 1
				return "about:blank"
			}
			delete(f.pendingHrefPolls, targetID)
			url := ""
			for _, tgt := range f.targets {
				if tgt.ID == targetID {
					url = tgt.URL
					break
				}
			}
			f.rendererHrefs[targetID] = url
			return url
		}
		if href, ok := f.rendererHrefs[targetID]; ok {
			return href
		}
		return "about:blank"
	case strings.Contains(expr, "scrollingElement"):
		// scroll()'s offset probe for no-op dispatch detection.
		f.mu.Lock()
		defer f.mu.Unlock()
		return map[string]any{
			"x":    0,
			"y":    f.scrollY,
			"maxX": 0,
			"maxY": f.maxScrollY,
		}
	case strings.Contains(expr, "window.scrollBy"):
		// scroll()'s in-page fallback for a wedged mouseWheel path.
		f.sawScrollBy.Store(true)
		return true
	case strings.Contains(expr, "location.href"):
		return map[string]any{
			"url":         "https://example.com/",
			"title":       "Example Domain",
			"viewport":    map[string]any{"width": 800, "height": 600},
			"scroll":      map[string]any{"x": 0, "y": 0},
			"page":        map[string]any{"width": 800, "height": 2000},
			"ready_state": "complete",
		}
	case expr == "document.readyState":
		return "complete"
	case strings.HasPrefix(expr, "(function (selector, requireVisible)"):
		// wait_for_element IIFE: the "#never" selector never matches.
		return !strings.Contains(expr, `"#never"`)
	case strings.HasPrefix(expr, "(function (selector, value)"),
		strings.HasPrefix(expr, "(function (selector, key, opts)"):
		// fill_input, dispatch_key IIFEs.
		return true
	default:
		return expr
	}
}

// TestBrowserReplHelpersWithFakeCDP exercises every seeded browser helper at
// least once against a fake CDP endpoint, per the spec's test plan.
func TestBrowserReplHelpersWithFakeCDP(t *testing.T) {
	fake := newFakeCDPServer(t)

	// Recording and HTTP endpoints used by start_recording, stop_recording,
	// and http_get.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/recording/start", "/recording/stop":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/health":
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	apiPort := api.URL[strings.LastIndex(api.URL, ":")+1:]

	t.Setenv("CDP_ENDPOINT", fake.wsURL())
	t.Setenv("KERNEL_API_PORT", apiPort)

	svc := newBrowserReplSvc(t)

	// Navigation, page state, and the cdp escape hatch.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		const tab = await ensure_real_tab();
		const nav = await goto_url("https://example.com/");
		const state = await wait_for_load();
		const info = await page_info();
		const viaSession = await cdp("Runtime.evaluate", { expression: "document.readyState", returnByValue: true });
		const viaBrowser = await cdp("Target.getTargets", undefined, null);
		({
			tab: tab.id,
			frame: nav.frame_id,
			state,
			title: info.title,
			dialog: info.dialog,
			ready: viaSession.result.value,
			targetCount: viaBrowser.targetInfos.length,
		})
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	nav, ok := r.Result.(map[string]any)
	require.True(t, ok, "expected object result, got %T", r.Result)
	require.Equal(t, "target-page-1", nav["tab"])
	require.Equal(t, "frame-1", nav["frame"])
	require.Equal(t, "complete", nav["state"])
	require.Equal(t, "Example Domain", nav["title"])
	require.Nil(t, nav["dialog"])
	require.Equal(t, "complete", nav["ready"])
	require.Equal(t, float64(3), nav["targetCount"])

	// Input helpers.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		await click_at_xy(10, 20);
		await type_text("hello");
		await fill_input("#q", "world");
		await press_key("Enter");
		await press_key("a", ["Shift"]);
		await scroll(100, 100, 0, 240);
		await dispatch_key("#q", "Enter");
		"input-ok"
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "input-ok", r.Result)

	// Tab management.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		const before = (await list_tabs()).length;
		const created = await new_tab("https://example.com/2");
		const current = await current_tab();
		const mid = (await list_tabs()).length;
		const switched = await switch_tab("target-page-1");
		await close_tab(created.id);
		const after = (await list_tabs()).length;
		({ before, createdId: created.id, currentIsCreated: current.id === created.id, mid, switchedTo: switched.id, after })
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	tabs, ok := r.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(1), tabs["before"], "internal pages are excluded by default")
	require.Equal(t, true, tabs["currentIsCreated"])
	require.Equal(t, float64(2), tabs["mid"])
	require.Equal(t, "target-page-1", tabs["switchedTo"])
	require.Equal(t, float64(1), tabs["after"])

	// Waiting, in-page JS, iframes, and event draining.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		await wait(0.05);
		const found = await wait_for_element("#thing", { visible: true, timeout_sec: 2 });
		await wait_for_network_idle(0.1, 5);
		const echoed = await js("echo-me-please");
		const frame = await iframe_target("frame.example");
		const noFrame = await iframe_target("no-such-host");
		let evs = [];
		for (let i = 0; i < 50 && evs.length === 0; i++) {
			evs = await drain_events();
			if (evs.length === 0) await wait(0.1);
		}
		({ found, echoed, frameUrl: frame && frame.url, noFrame, eventCount: evs.length })
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	waits, ok := r.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, waits["found"])
	require.Equal(t, "echo-me-please", waits["echoed"])
	require.Equal(t, "https://frame.example.com/widget", waits["frameUrl"])
	require.Nil(t, waits["noFrame"])
	require.GreaterOrEqual(t, waits["eventCount"], float64(1), "drain_events returns buffered session events")

	// Screenshots, uploads, HTTP, and recording delegation.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: fmt.Sprintf(`
		const shot = await capture_screenshot("/tmp/fake-cdp-shot.png", false, 400);
		await repl.emitImage({ path: shot });
		await upload_file("#file", ["/tmp/fake-cdp-shot.png"]);
		const body = await http_get("%s/health");
		const rec = await start_recording();
		const dir = await recording_dir();
		const stopped = await stop_recording();
		({ shot, body, recId: rec.recorder_id, dir, stoppedId: stopped.recorder_id })
	`, api.URL)})
	require.True(t, r.Success, "error: %v", r.Error)
	misc, ok := r.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "/tmp/fake-cdp-shot.png", misc["shot"])
	require.Equal(t, "ok", misc["body"])
	require.Equal(t, "default", misc["recId"])
	require.Equal(t, "default", misc["stoppedId"])
	require.NotNil(t, r.Content)
	sawImage := false
	for _, item := range *r.Content {
		if img, err := item.AsBrowserExecutionImageContent(); err == nil && img.Type == "image" {
			sawImage = true
			require.Equal(t, "image/png", img.MimeType)
		}
	}
	require.True(t, sawImage, "the captured screenshot is emitted as image content")

	// The REPL survives all of the above on a single repl_id.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

// TestBrowserReplHelperErgonomics covers the QA findings on helper
// argument validation and wait-helper deadlines: bad arguments must produce
// clear errors (not cryptic TypeErrors or silent ignores), and a wait
// helper's default timeout must lose to the execution deadline so a routine
// miss is a clean error instead of a destructive execution timeout.
func TestBrowserReplHelperErgonomics(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	// press_key rejects non-array/non-object modifiers with a clear error
	// instead of a cryptic "(modifiers ?? []) is not iterable" TypeError.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await press_key("a", "Control")`})
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "press_key: modifiers must be an array")

	// press_key accepts the {ctrl: true} object sugar and dispatches the
	// Control modifier bit (2) to CDP.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await press_key("a", {ctrl: true}); "ok"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "ok", r.Result)
	keyEv := fake.lastKeyEventParams()
	require.NotNil(t, keyEv, "press_key must dispatch key events")
	require.Equal(t, float64(2), keyEv["modifiers"], "ctrl sugar maps to the Control modifier bit")

	// Unknown keys in the modifiers object are rejected with a clear error.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await press_key("a", {bogus: true})`})
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "press_key: unknown modifier")

	// js rejects a non-string target (a natural mistake given other helpers
	// take opts objects) with a clear validation error instead of a raw CDP
	// 'Invalid parameters' failure from Target.attachToTarget.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await js("1", {target: "target-page-1"})`})
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "js: target must be a target id string")

	// A valid target id string still routes to the target.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await js("via-target", "target-page-1")`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "via-target", r.Result)

	// wait_for_element rejects a non-object opts argument immediately
	// instead of silently ignoring it and waiting out the default timeout.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await wait_for_element("#never", false, 2)`})
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "wait_for_element: opts must be an object")

	// A wait helper's default timeout (30s) is clamped below the execution
	// deadline, so a routine element-wait miss surfaces the helper's clean
	// error and the REPL survives; previously this tied the execution
	// timeout and destructively killed the REPL.
	timeoutSec := 3
	start := time.Now()
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code:       `await wait_for_element("#never")`,
		TimeoutSec: &timeoutSec,
	})
	require.Less(t, time.Since(start), 3*time.Second, "the helper error must beat the execution timeout")
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "timed out waiting for element")
	require.Nil(t, r.ReplTerminated, "a helper timeout must not destroy the REPL")

	// The REPL survived all of the above on a single repl_id.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

// TestBrowserReplFrozenRendererRecovery covers the QA finding that a modal
// JavaScript dialog left open by a killed REPL bricked /browser/execute:
// every fresh REPL hung in session attach until the destructive execution
// timeout, burning a REPL per attempt, with no in-API recovery short of a
// Chromium restart. Session-routed CDP commands must instead be bounded
// below the execution deadline, so the caller gets a clean error, the REPL
// survives, browser-level commands keep working, and the session recovers
// once the renderer unfreezes.
func TestBrowserReplFrozenRendererRecovery(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	// Baseline: helpers work against a healthy renderer.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await page_info()).title`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "Example Domain", r.Result)

	// Freeze the renderer (session-routed commands are never answered) and
	// force a fresh REPL, reproducing the stale pre-attach dialog scenario.
	fake.hangSession.Store(true)
	reset := true
	timeoutSec := 5
	start := time.Now()
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code:       `await page_info()`,
		TimeoutSec: &timeoutSec,
		Reset:      &reset,
	})
	require.Less(t, time.Since(start), 6*time.Second,
		"the frozen renderer must surface a clean error at the deadline margin, "+
			"not hang past the socket read deadline (timeout + grace)")
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "renderer is unresponsive",
		"the error should point at the recovery path, got: %s", *r.Error)
	require.True(t, r.ReplTerminated == nil || !*r.ReplTerminated,
		"a frozen renderer must not destroy the REPL")
	frozenID := r.ReplId

	// Browser-level commands keep working on the same REPL while the
	// renderer is frozen.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await list_tabs()).length`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, frozenID, r.ReplId, "the REPL must survive the frozen renderer")

	// A second session command also fails cleanly (no per-attempt REPL burn).
	timeoutSec2 := 3
	start = time.Now()
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code:       `await js("1")`,
		TimeoutSec: &timeoutSec2,
	})
	require.Less(t, time.Since(start), 3*time.Second)
	require.False(t, r.Success)
	require.True(t, r.ReplTerminated == nil || !*r.ReplTerminated)
	require.Equal(t, frozenID, r.ReplId)

	// Unfreeze: the session retries domain enables and recovers in place,
	// without a reattach, a new repl_id, or any JavaScript state loss.
	fake.hangSession.Store(false)
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await page_info()).title`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "Example Domain", r.Result)
	require.Equal(t, frozenID, r.ReplId, "recovery must not replace the REPL")
}

// TestBrowserReplCrashDuringExecutionResponse covers the QA finding that
// crash/OOM error responses omitted duration_ms and the truncation flags
// while the timeout path included them. All failure paths must populate the
// same optional fields so clients can read them unconditionally.
func TestBrowserReplCrashDuringExecutionResponse(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r1.Success)

	// The child SIGKILLs itself mid-execution (stand-in for an OOM kill):
	// the socket read fails and the API reports the terminated REPL.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `process.kill(process.pid, "SIGKILL")`,
	})
	require.False(t, r2.Success)
	require.Equal(t, r1.ReplId, r2.ReplId, "the response carries the terminated REPL's ID")
	require.NotNil(t, r2.ReplTerminated)
	require.True(t, *r2.ReplTerminated)
	require.NotNil(t, r2.Error)
	require.Contains(t, *r2.Error, "terminated during execution")
	require.NotNil(t, r2.DurationMs, "crash responses must include duration_ms")
	require.GreaterOrEqual(t, *r2.DurationMs, 0)
	require.NotNil(t, r2.ResultTruncated, "crash responses must include result_truncated")
	require.NotNil(t, r2.ContentTruncated, "crash responses must include content_truncated")

	// The next request lazily starts a fresh REPL.
	r3 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
	require.True(t, r3.Success)
	require.NotEqual(t, r1.ReplId, r3.ReplId)
}

// TestBrowserReplStaticImportRejected verifies that the JavaScript-only cell
// runtime rejects static module syntax while retaining dynamic import().
func TestBrowserReplStaticImportRejected(t *testing.T) {
	svc := newBrowserReplSvc(t)

	for name, code := range map[string]string{
		"unused default import": `import path from "node:path"; 1`,
		"used default import":   `import path from "node:path"; path.basename("/x")`,
		"namespace import":      `import * as fs from "node:fs"`,
		"named import":          `import { basename } from "node:path"`,
		"side-effect import":    `import "node:path"`,
		"export declaration":    `export const x = 1`,
	} {
		r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
		require.False(t, r.Success, "%s must be rejected", name)
		require.NotNil(t, r.Error)
		require.Contains(t, *r.Error, "static import/export is not supported", name)
		require.True(t, r.ReplTerminated == nil || !*r.ReplTerminated,
			"a static import error must not destroy the REPL")
	}

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `(await import("node:path")).basename("/a/b")`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "b", r.Result)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `return 1`})
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "top-level return is not supported")
}

// TestBrowserReplHeapCapConfigurable verifies the BROWSER_REPL_HEAP_MB knob
// reaches the node child's --max-old-space-size argument.
func TestBrowserReplHeapCapConfigurable(t *testing.T) {
	t.Setenv("BROWSER_REPL_HEAP_MB", "256")

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)

	svc.browserReplMu.Lock()
	args := svc.browserRepl.cmd.Args
	svc.browserReplMu.Unlock()
	require.Contains(t, args, "--max-old-space-size=256")
}

// TestBrowserReplEventRingBounded floods the daemon with more CDP events
// than the event ring capacity (500) and verifies old events are dropped
// instead of growing the buffer unboundedly.
func TestBrowserReplEventRingBounded(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "attached"`})
	require.True(t, r.Success, "error: %v", r.Error)

	// Queue 600 session events; they flush to the daemon after the next
	// command response.
	for i := 0; i < 600; i++ {
		fake.queueEvent(map[string]any{
			"method":    "Network.requestWillBeSent",
			"params":    map[string]any{"requestId": fmt.Sprintf("flood-%d", i)},
			"sessionId": "session-target-page-1",
		})
	}
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		await js("flush"); // command round-trip flushes the queued events
		await wait(0.5);   // let the daemon process the flooded socket
		const evs = await drain_events();
		evs.length
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(500), r.Result, "old events are dropped at the ring capacity")
}

// TestBrowserReplReconnectPreservesState simulates a Chromium restart by
// dropping the daemon's browser connection: the next helper call must
// reconnect and reattach without changing repl_id or losing bindings.
func TestBrowserReplReconnectPreservesState(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `var restartToken = "pre-restart"; await ensure_real_tab(); restartToken`,
	})
	require.True(t, r1.Success, "error: %v", r1.Error)
	require.Equal(t, "pre-restart", r1.Result)
	require.Equal(t, 1, fake.connCount())

	// Simulate a Chromium restart: the browser connection drops.
	fake.Restart()
	require.Eventually(t, func() bool { return fake.connCount() == 0 },
		5*time.Second, 10*time.Millisecond, "daemon connection must close")

	// The next helper call reconnects and reattaches; repl_id and JavaScript
	// bindings survive.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `const info = await page_info(); ({ token: restartToken, title: info.title })`,
	})
	require.True(t, r2.Success, "error: %v", r2.Error)
	require.Equal(t, r1.ReplId, r2.ReplId, "a Chromium restart must not change repl_id")
	res, ok := r2.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pre-restart", res["token"], "bindings survive a browser reconnect")
	require.Equal(t, "Example Domain", res["title"])
	require.Equal(t, 1, fake.connCount(), "the daemon reconnected")
}

// TestBrowserReplResultIntegrityUnderPollution verifies that user-installed
// global/prototype hooks (JSON.stringify replacement, toJSON pollution)
// cannot corrupt the result payload or protocol framing.
func TestBrowserReplResultIntegrityUnderPollution(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		JSON.stringify = () => "PWNED";
		Array.prototype.toJSON = () => "PWNED";
		Object.prototype.toJSON = () => "PWNED";
		"polluted"
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "polluted", r.Result)

	// The result payload reflects actual values, not user-installed hooks.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `({a: [1, 2, 3], b: "str"})`})
	require.True(t, r.Success, "error: %v", r.Error)
	res, ok := r.Result.(map[string]any)
	require.True(t, ok, "expected object result, got %T", r.Result)
	require.Equal(t, []any{float64(1), float64(2), float64(3)}, res["a"])
	require.Equal(t, "str", res["b"])

	// Protocol framing still works: repl.write output arrives intact.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `repl.write("frame-ok"); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "done", r.Result)
	require.NotNil(t, r.Content)
	require.Len(t, *r.Content, 1)
	txt, err := (*r.Content)[0].AsBrowserExecutionTextContent()
	require.NoError(t, err)
	require.Equal(t, "frame-ok", txt.Text)
}

// TestBrowserReplPageInfoReportsPendingDialog verifies that page_info
// short-circuits while a modal JavaScript dialog is pending: a real renderer
// freezes behind the dialog, so a Runtime.evaluate would block until the CDP
// command timeout and the dialog field would be unreachable exactly when it
// matters.
func TestBrowserReplPageInfoReportsPendingDialog(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "attached"`})
	require.True(t, r.Success, "error: %v", r.Error)

	// Simulate a modal dialog: the renderer freezes (Runtime.evaluate fails)
	// and the browser emits Page.javascriptDialogOpening.
	fake.frozen.Store(true)
	fake.queueEvent(map[string]any{
		"method":    "Page.javascriptDialogOpening",
		"params":    map[string]any{"type": "alert", "message": "hello-dialog"},
		"sessionId": "session-target-page-1",
	})

	// Flush the queued event with a browser-level command, then give the
	// daemon a moment to process it.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await cdp("Target.getTargets", undefined, null); "flushed"`})
	require.True(t, r.Success, "error: %v", r.Error)
	time.Sleep(200 * time.Millisecond)

	// page_info must return promptly with the dialog instead of issuing a
	// Runtime.evaluate that would block behind the frozen renderer.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		const info = await page_info();
		({ url: info.url, title: info.title, dialog: info.dialog })
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	res, ok := r.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "https://example.com/", res["url"])
	require.Equal(t, "Example Domain", res["title"])
	dialog, ok := res["dialog"].(map[string]any)
	require.True(t, ok, "expected dialog payload, got %v", res["dialog"])
	require.Equal(t, "alert", dialog["type"])
	require.Equal(t, "hello-dialog", dialog["message"])

	// Closing the dialog restores the normal page_info path.
	fake.frozen.Store(false)
	fake.queueEvent(map[string]any{
		"method":    "Page.javascriptDialogClosed",
		"params":    map[string]any{"result": true},
		"sessionId": "session-target-page-1",
	})
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await cdp("Target.getTargets", undefined, null); "flushed"`})
	require.True(t, r.Success, "error: %v", r.Error)
	time.Sleep(200 * time.Millisecond)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await page_info()).dialog`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Nil(t, r.Result)
	require.Nil(t, r.ResultRepr)
}

// TestBrowserReplAttachRetriesStaleTarget verifies that a target destroyed
// between listing and attaching (a target-swap race) is absorbed by the
// runtime instead of surfacing a raw CDP error.
func TestBrowserReplAttachRetriesStaleTarget(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "attached"`})
	require.True(t, r.Success, "error: %v", r.Error)

	// Drop the browser connection so the next helper call must re-attach,
	// and make the first attach attempt fail with the stale-target error a
	// target swap produces.
	fake.Restart()
	require.Eventually(t, func() bool { return fake.connCount() == 0 },
		5*time.Second, 10*time.Millisecond, "daemon connection must close")
	fake.failNextAttach.Store(1)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await page_info()).title`})
	require.True(t, r.Success, "a transient stale-target attach must be retried: %v", r.Error)
	require.Equal(t, "Example Domain", r.Result)
	require.Equal(t, int32(0), fake.failNextAttach.Load(), "the first attach attempt failed as planned")
}

// TestBrowserReplRetriesCommandOnFreshConnectionClose reproduces the flaky
// e2e failure where the first browser-helper call after a Chromium restart
// hit "CDP connection closed": the DevTools proxy accepts the WebSocket and
// then closes it while the browser behind it is still coming up. A command
// whose connection died before answering anything must be retried once on a
// fresh connection.
func TestBrowserReplRetriesCommandOnFreshConnectionClose(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `var retryToken = "pre-restart"; await ensure_real_tab(); retryToken`,
	})
	require.True(t, r1.Success, "error: %v", r1.Error)
	require.Equal(t, int32(1), fake.totalConns.Load())

	// Simulate a Chromium restart, then a proxy that accepts the next
	// connection and drops it mid-first-command because the browser behind
	// it is not up yet.
	fake.Restart()
	require.Eventually(t, func() bool { return fake.connCount() == 0 },
		5*time.Second, 10*time.Millisecond, "daemon connection must close")
	fake.closeNextConnsAfterFirstCommand.Store(1)

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `const info = await page_info(); ({ token: retryToken, title: info.title })`,
	})
	require.True(t, r2.Success, "a command on a fresh connection that died unanswered must be retried: %v", r2.Error)
	require.Equal(t, r1.ReplId, r2.ReplId, "the transient close must not change repl_id")
	res, ok := r2.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pre-restart", res["token"], "bindings survive the reconnect-and-retry")
	require.Equal(t, "Example Domain", res["title"])
	require.Equal(t, int32(0), fake.closeNextConnsAfterFirstCommand.Load(), "the fresh connection was dropped as planned")
	require.Equal(t, int32(3), fake.totalConns.Load(), "initial + dropped + retried connections")
	require.Equal(t, 1, fake.connCount(), "the retried connection is still open")
}

// fakeReplDaemonJS is a minimal Unix-socket daemon used to inject protocol
// failures the real daemon never produces. FAKE_REPL_MODE selects the
// failure mode.
const fakeReplDaemonJS = `
const net = require('net');
const fs = require('fs');
const sock = process.env.BROWSER_REPL_SOCKET;
try { fs.unlinkSync(sock); } catch (e) {}
const mode = process.env.FAKE_REPL_MODE || 'ok';
net.createServer((conn) => {
  let buf = '';
  conn.on('data', (d) => {
    buf += d.toString();
    const idx = buf.indexOf('\n');
    if (idx === -1) return;
    const line = buf.slice(0, idx);
    buf = buf.slice(idx + 1);
    let req = {};
    try { req = JSON.parse(line); } catch (e) {}
    const base = {
      id: req.id,
      repl_id: process.env.BROWSER_REPL_ID,
      success: true,
      result: 1,
      content: [],
      result_truncated: false,
      content_truncated: false,
      duration_ms: 1,
    };
    if (mode === 'bad-request-id') {
      conn.write(JSON.stringify({ ...base, id: 'wrong-id' }) + '\n');
    } else if (mode === 'bad-repl-id') {
      conn.write(JSON.stringify({ ...base, repl_id: 'wrong-repl' }) + '\n');
    } else if (mode === 'garbage') {
      conn.write('this is not json\n');
    } else if (mode === 'die') {
      process.exit(1);
    } else {
      conn.write(JSON.stringify(base) + '\n');
    }
  });
}).listen(sock);
`

// TestBrowserReplProtocolCorruptionTerminates verifies that mismatched
// request/repl IDs, malformed daemon responses, and a child dying
// mid-execution all terminate the child and surface repl_terminated: true.
func TestBrowserReplProtocolCorruptionTerminates(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not available: %v", err)
	}
	script := filepath.Join(t.TempDir(), "fake-repl.js")
	require.NoError(t, os.WriteFile(script, []byte(fakeReplDaemonJS), 0o644))

	for _, tc := range []struct {
		mode        string
		wantErrPart string
	}{
		{"bad-request-id", "response ID mismatch"},
		{"bad-repl-id", "repl_id mismatch"},
		{"garbage", "failed to parse response"},
		// A child dying mid-execution must surface its exit reason (e.g.
		// SIGKILL from the OOM killer near the heap cap), not a bare
		// transport error.
		{"die", "terminated during execution (exit status 1)"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("BROWSER_REPL_SCRIPT", script)
			t.Setenv("BROWSER_REPL_SOCKET", filepath.Join(t.TempDir(), "browser-repl.sock"))
			t.Setenv("FAKE_REPL_MODE", tc.mode)

			svc, err := newSvc(t, recorder.NewFFmpegManager())
			require.NoError(t, err)
			t.Cleanup(func() {
				svc.browserReplMu.Lock()
				svc.terminateBrowserReplLocked(context.Background(), "test cleanup")
				svc.browserReplMu.Unlock()
			})

			resp, err := svc.ExecuteBrowserCode(context.Background(), oapi.ExecuteBrowserCodeRequestObject{
				Body: &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"},
			})
			require.NoError(t, err)
			typed, ok := resp.(oapi.ExecuteBrowserCode200JSONResponse)
			require.True(t, ok, "expected 200 response, got %T", resp)
			require.False(t, typed.Success)
			require.NotNil(t, typed.ReplTerminated)
			require.True(t, *typed.ReplTerminated, "protocol corruption must terminate the REPL")
			require.NotNil(t, typed.Error)
			require.Contains(t, *typed.Error, tc.wantErrPart)
			require.Nil(t, svc.browserRepl, "no replacement starts until the next request")
		})
	}
}

// TestBrowserReplUnhandledRejectionSurvives covers the high-severity QA
// finding that a floating rejected promise in user code crashed the whole
// REPL child (Node >= 15 crashes on unhandled rejections by default). A
// settled rejection leaves no inconsistent in-flight state, so the daemon
// must surface it as a stderr content item and keep the REPL — and all of
// its bindings — alive.
func TestBrowserReplUnhandledRejectionSurvives(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		var kept = 'state-kept';
		setTimeout(() => { Promise.reject(new Error('boom-floating')); }, 20);
		'submitted'
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "submitted", r.Result)

	// The rejection surfaces as a stderr content item — either drained from
	// the stray buffer into this execution or captured while it runs — and
	// the REPL (and its bindings) survive.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await wait(0.3); kept`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "state-kept", r.Result)
	require.NotNil(t, r.Content)
	sawRejection := false
	for _, item := range *r.Content {
		txt, err := item.AsBrowserExecutionTextContent()
		if err == nil && txt.Channel == "stderr" &&
			strings.Contains(txt.Text, "unhandled promise rejection") &&
			strings.Contains(txt.Text, "boom-floating") {
			sawRejection = true
		}
	}
	require.True(t, sawRejection, "the floating rejection must surface as a stderr content item, got %v", r.Content)

	// Same repl_id throughout: no state was lost.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

// TestBrowserReplUncaughtExceptionTerminates covers the high-severity QA
// finding that an uncaught exception in user code (e.g. a throwing
// setTimeout callback) crashed the REPL child with only a bare EOF for the
// in-flight caller. Resuming after an uncaught exception is unsafe per
// Node semantics, so the daemon must terminate deterministically: the
// in-flight execution is answered with the exception and repl_terminated,
// and the next request lazily starts a fresh REPL — explicit state loss,
// never silent.
func TestBrowserReplUncaughtExceptionTerminates(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		var doomed = 'will-be-lost';
		globalThis.boom = () => { throw new Error('boom-uncaught') };
		'scheduled'
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "scheduled", r.Result)
	doomedID := r.ReplId

	// The throw fires from a timer while this execution is deterministically
	// in flight: the daemon answers it with the exception and exiting: true
	// (mapped to repl_terminated), then exits non-zero.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		setTimeout(() => boom(), 10);
		await wait(5);
		'never-reached'
	`})
	require.False(t, r.Success)
	require.NotNil(t, r.Error)
	require.Contains(t, *r.Error, "uncaught exception")
	require.Contains(t, *r.Error, "boom-uncaught")
	require.NotNil(t, r.ReplTerminated)
	require.True(t, *r.ReplTerminated, "an uncaught exception must report repl_terminated explicitly")
	require.Equal(t, doomedID, r.ReplId, "the terminated response carries the dead REPL's ID")

	// The next request lazily starts a fresh REPL: new repl_id, prior
	// bindings gone.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `typeof doomed`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "undefined", r.Result)
	require.NotEqual(t, doomedID, r.ReplId)

	// An uncaught exception with no execution in flight also terminates the
	// child deterministically; the next request recovers with a fresh ID.
	idleID := r.ReplId
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		setTimeout(() => { throw new Error('boom-idle') }, 20);
		'scheduled'
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	time.Sleep(500 * time.Millisecond) // let the timer fire and the child exit
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r.Success, "error: %v", r.Error)
	require.NotEqual(t, idleID, r.ReplId, "an idle-time uncaught exception must cost the REPL its ID")
}

// TestBrowserReplRequestLineCapEnforced covers the QA finding that the
// 8 MiB incoming-request cap was bypassed when the request arrived in a
// single write containing the newline. The cap must apply per accumulated
// line regardless of chunking, and the REPL must survive the rejection.
func TestBrowserReplRequestLineCapEnforced(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r.Success)

	conn, err := net.Dial("unix", browserReplSocketPath())
	require.NoError(t, err)
	defer conn.Close()

	// One write containing the trailing newline: previously parsed and
	// executed instead of rejected.
	payload := []byte(`{"id":"big","code":"` + strings.Repeat("A", 8*1024*1024+1000) + `"}` + "\n")
	_, err = conn.Write(payload)
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(line, &resp))
	require.Equal(t, false, resp["success"])
	require.Contains(t, resp["error"], "byte limit")

	// The REPL survives the rejection on the same repl_id.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

// TestBrowserReplHalfClosedClientReceivesResponse covers the QA finding
// that a client half-closing (SHUT_WR) after sending a valid request lost
// the execution response: the server socket self-destroyed on the client
// FIN before the async response was written. With allowHalfOpen the
// response must still be delivered, after which the daemon ends its side.
func TestBrowserReplHalfClosedClientReceivesResponse(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r.Success)

	raw, err := net.Dial("unix", browserReplSocketPath())
	require.NoError(t, err)
	conn := raw.(*net.UnixConn)
	defer conn.Close()

	_, err = conn.Write([]byte(`{"id":"hc","code":"40 + 2","timeout_ms":5000}` + "\n"))
	require.NoError(t, err)
	require.NoError(t, conn.CloseWrite()) // SHUT_WR

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	data, err := io.ReadAll(conn) // reads until the daemon ends its side
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &resp))
	require.Equal(t, "hc", resp["id"])
	require.Equal(t, true, resp["success"])
	require.Equal(t, float64(42), resp["result"])
}

// TestBrowserReplNewTabWaitsForRendererCommit covers the QA finding that
// new_tab(url) returned before the initial navigation committed in the
// renderer: target metadata shows the URL as soon as the navigation
// starts, so an immediate page_info()/js() still observed about:blank.
// new_tab must wait for the renderer-level commit.
func TestBrowserReplNewTabWaitsForRendererCommit(t *testing.T) {
	fake := newFakeCDPServer(t)
	fake.delayCommit.Store(true)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		const nt = await new_tab("https://example.com/2");
		const href = await js("location.href");
		({ id: nt.id, href })
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	res, ok := r.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "https://example.com/2", res["href"],
		"new_tab must wait for the renderer-level navigation commit, not just target metadata")
}

// TestBrowserReplScrollFallback covers the QA finding that CDP mouseWheel
// commands intermittently hang (never answered) on rare new-headless
// Chromium instances while every other command answers fine, costing the
// full 30s default command timeout per attempt. scroll() must bound the
// dispatch, fall back to an in-page window.scrollBy, surface the fallback,
// and keep the REPL alive.
func TestBrowserReplScrollFallback(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	// Baseline: the normal mouseWheel dispatch works.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 240); "ok"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "ok", r.Result)
	require.False(t, fake.sawScrollBy.Load(), "no fallback when mouseWheel answers")

	// Wedge the mouseWheel path: the command is never answered. scroll()
	// must fail fast (its bounded dispatch timeout, not the 30s default)
	// and fall back to window.scrollBy.
	fake.hangMouseWheel.Store(true)
	timeoutSec := 30
	start := time.Now()
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code:       `await scroll(100, 100, 0, 240); "scrolled"`,
		TimeoutSec: &timeoutSec,
	})
	require.Less(t, time.Since(start), 15*time.Second,
		"the fallback must engage well before the default 30s command timeout")
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "scrolled", r.Result)
	require.True(t, fake.sawScrollBy.Load(), "the in-page scrollBy fallback must run")
	require.NotNil(t, r.Content)
	sawNote := false
	for _, item := range *r.Content {
		txt, err := item.AsBrowserExecutionTextContent()
		if err == nil && txt.Channel == "stderr" && strings.Contains(txt.Text, "falling back to window.scrollBy") {
			sawNote = true
		}
	}
	require.True(t, sawNote, "the fallback must be surfaced as a stderr content item, got %v", r.Content)

	// The REPL survives on the same repl_id.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

// TestBrowserReplAsyncConstLetSemantics covers const/let behavior across
// module cells containing top-level await. Bindings must keep real JavaScript semantics no matter which
// evaluation path declared them.
func TestBrowserReplAsyncConstLetSemantics(t *testing.T) {
	svc := newBrowserReplSvc(t)
	await_ := "await new Promise(r => setTimeout(r, 1)); "

	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}
	expectErr := func(code string, substr string) {
		t.Helper()
		r := exec(code)
		require.False(t, r.Success, "expected failure for %q", code)
		require.NotNil(t, r.Error)
		require.Contains(t, *r.Error, substr, "code: %q", code)
	}
	expectResult := func(code string, want interface{}) {
		t.Helper()
		r := exec(code)
		require.True(t, r.Success, "code %q failed: %v", code, r.Error)
		require.Equal(t, want, r.Result, "code: %q", code)
	}

	// const declared in async mode keeps redeclaration protection and
	// immutability on both paths.
	exec(`const qaConst = 1; ` + await_ + `'declared'`)
	expectErr(`const qaConst = 2; `+await_+`qaConst`, "Identifier 'qaConst' has already been declared")
	expectErr(`qaConst = 99; `+await_+`qaConst`, "Assignment to constant variable.")
	expectErr(`qaConst = 99`, "Assignment to constant variable.")
	expectResult(`qaConst`, float64(1))

	// let stays mutable but cannot be redeclared.
	expectResult(`let qaLet = 10; `+await_+`qaLet`, float64(10))
	expectErr(`let qaLet = 11; `+await_+`qaLet`, "Identifier 'qaLet' has already been declared")
	expectResult(`qaLet = 42; `+await_+`qaLet`, float64(42))
	expectResult(`qaLet`, float64(42))

	// class declarations get let-like protection.
	expectResult(`class QaClass { hi() { return 'hi' } }; `+await_+`new QaClass().hi()`, "hi")
	expectErr(`class QaClass {}; `+await_+`1`, "Identifier 'QaClass' has already been declared")

	// var and function declarations stay freely redeclarable.
	expectResult(`var qaVar = 1; `+await_+`qaVar`, float64(1))
	expectResult(`var qaVar = 2; `+await_+`qaVar`, float64(2))
	expectResult(`function qaFn() { return 1 }; `+await_+`qaFn()`, float64(1))
	expectResult(`function qaFn() { return 2 }; `+await_+`qaFn()`, float64(2))

	// Destructured const bindings persist and are protected.
	expectResult(`const { a: qaA, b: qaB } = { a: 1, b: 2 }; `+await_+`qaA + qaB`, float64(3))
	expectErr(`qaA = 5`, "Assignment to constant variable.")

	// Cross-path conflicts: a fast-path declaration cannot be redeclared in
	// async mode, and an async-mode declaration cannot be redeclared on the
	// fast path.
	exec(`const qaFastConst = 'fc'`)
	expectErr(`const qaFastConst = 'x'; `+await_+`1`, "Identifier 'qaFastConst' has already been declared")
	exec(`const qaAsyncConst = 'ac'; ` + await_ + `1`)
	expectErr(`const qaAsyncConst = 'x'`, "Identifier 'qaAsyncConst' has already been declared")
	expectResult(`qaAsyncConst`, "ac")

	// A failed initializer still reserves the name (script declarations
	// instantiate before the first statement runs).
	expectErr(`let qaFailLet = (() => { throw new Error('initfail') })(); 1`, "initfail")
	expectErr(`let qaFailLet = 2; 2`, "Identifier 'qaFailLet' has already been declared")

	// None of the failures above terminated the REPL.
	r := exec(`repl.id`)
	require.True(t, r.Success)
}

// TestBrowserReplTrailingBlockResult verifies that every SourceTextModule cell
// reports only an implicit final expression, never a block completion value.
func TestBrowserReplTrailingBlockResult(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `try { await new Promise(r => setTimeout(r, 1)); 'after' } catch { 'caught' }`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.NotNil(t, r.ResultRepr)
	require.Equal(t, "undefined", *r.ResultRepr)

	// A block has no implicit result on the module-cell path either.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `try { 42 } catch {}`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "undefined", *r.ResultRepr)

	// A trailing expression after the await still yields an implicit result.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `await new Promise(r => setTimeout(r, 1)); 'tail'`,
	})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "tail", r.Result)
}

// TestBrowserReplScrollRetriesSwallowedWheel covers the QA finding that
// Chromium silently swallows the first mouseWheel dispatch after a
// navigation: the command answers normally but the page never scrolls, so
// the dispatch-timeout fallback cannot detect it. scroll() must detect a
// no-op dispatch on a scrollable page and retry once.
func TestBrowserReplScrollRetriesSwallowedWheel(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	fake.mu.Lock()
	fake.maxScrollY = 2000
	fake.swallowNextWheel = true
	fake.mu.Unlock()

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count := fake.wheelDispatchCount
	y := fake.scrollY
	fake.mu.Unlock()
	require.Equal(t, 2, count, "the swallowed dispatch must be retried exactly once")
	require.Equal(t, int64(700), y, "the retry must actually scroll")
	require.NotNil(t, r.Content)
	sawNote := false
	for _, item := range *r.Content {
		txt, err := item.AsBrowserExecutionTextContent()
		if err == nil && txt.Channel == "stderr" && strings.Contains(txt.Text, "retrying once") {
			sawNote = true
		}
	}
	require.True(t, sawNote, "the retry must be surfaced as a stderr content item, got %v", r.Content)

	// Once the input pipeline is awake the next scroll dispatches exactly
	// once and moves without a retry.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done2"`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count = fake.wheelDispatchCount
	y = fake.scrollY
	fake.mu.Unlock()
	require.Equal(t, 3, count)
	require.Equal(t, int64(1400), y)
	if r.Content != nil {
		for _, item := range *r.Content {
			txt, err := item.AsBrowserExecutionTextContent()
			require.False(t, err == nil && txt.Channel == "stderr" && strings.Contains(txt.Text, "retrying once"),
				"no retry expected once the pipeline is awake")
		}
	}

	// An unscrollable page is left unverified: a single dispatch, no retry.
	fake.mu.Lock()
	fake.maxScrollY = 0
	fake.swallowNextWheel = true
	fake.mu.Unlock()
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done3"`})
	require.True(t, r.Success, "error: %v", r.Error)
	fake.mu.Lock()
	count = fake.wheelDispatchCount
	fake.mu.Unlock()
	require.Equal(t, 4, count, "unscrollable pages must not be retried")
}

// TestBrowserReplScrollWaitsForAsyncWheelApplication covers the QA finding
// that scroll() probed the scroll offset before Chromium asynchronously
// applied the wheel, so the swallowed-wheel retry false-positived on every
// scroll and the page landed 2x past its target. scroll() must poll the
// offset through a settle window before declaring a dispatch had no effect.
func TestBrowserReplScrollWaitsForAsyncWheelApplication(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	fake.mu.Lock()
	fake.maxScrollY = 5000
	fake.mu.Unlock()
	fake.delayedWheelMs.Store(100)

	scrollY := func() int64 {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.scrollY
	}
	wheelCount := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.wheelDispatchCount
	}

	// The wheel applies ~100ms after the command answers. scroll() must not
	// mistake the not-yet-applied offset for a swallowed dispatch: exactly
	// one dispatch, no retry, no double scroll.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Eventually(t, func() bool { return scrollY() == 700 }, 3*time.Second, 20*time.Millisecond,
		"the wheel must apply exactly once")
	require.Equal(t, 1, wheelCount(), "an asynchronously-applied wheel must not be retried")
	if r.Content != nil {
		for _, item := range *r.Content {
			txt, err := item.AsBrowserExecutionTextContent()
			require.False(t, err == nil && txt.Channel == "stderr" && strings.Contains(txt.Text, "retrying once"),
				"no retry expected when the wheel applies within the settle window")
		}
	}

	// A second scroll lands exactly one more delta (cumulative 1400).
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 700); "done2"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Eventually(t, func() bool { return scrollY() == 1400 }, 3*time.Second, 20*time.Millisecond,
		"the second scroll must move the offset by exactly one delta")
	require.Equal(t, 2, wheelCount())
}

// TestBrowserReplAttachActivatesTarget covers the QA finding that JS
// dialogs were non-deterministically auto-cancelled after a Chromium
// restart: which tab is foreground is not deterministic across restarts,
// and headless Chromium auto-cancels hidden tabs' dialogs
// (Page.javascriptDialogClosed result:false immediately after opening).
// Attach must activate the target so dialog semantics are stable.
func TestBrowserReplAttachActivatesTarget(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)

	fake.mu.Lock()
	activated := append([]string(nil), fake.activatedTargets...)
	fake.mu.Unlock()
	require.Contains(t, activated, "target-page-1", "attach must activate the attached target")
}

// TestBrowserReplOrphanedDaemonKilled covers the QA finding that an
// orphaned REPL daemon (started outside the API process) was unlinked but
// never killed, leaking for the container lifetime. Lazy start must kill
// the orphan holding the socket before replacing it.
func TestBrowserReplOrphanedDaemonKilled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("orphan detection requires /proc")
	}
	script := ensureBrowserReplBundle(t)
	svc := newBrowserReplSvc(t)
	socketPath := browserReplSocketPath()

	// Start a rogue daemon on the API's socket path, outside the API's
	// supervision (the pdeathsig path cannot cover it).
	rogue := exec.Command("node", script)
	rogue.Env = append(os.Environ(),
		"BROWSER_REPL_SOCKET="+socketPath,
		"BROWSER_REPL_ID=rogue0000000000000000000",
	)
	require.NoError(t, rogue.Start())
	t.Cleanup(func() {
		_ = rogue.Process.Kill()
		_, _ = rogue.Process.Wait()
	})

	// Wait until the rogue is listening on the socket.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		require.False(t, time.Now().After(deadline), "rogue daemon never started listening")
		time.Sleep(50 * time.Millisecond)
	}

	// The next execution must refuse adoption, kill the orphan, and serve
	// from a fresh child with a different repl_id.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(2), r.Result)
	require.NotEqual(t, "rogue0000000000000000000", r.ReplId)

	// The rogue process must be dead (zombie until this test reaps it).
	deadline = time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", rogue.Process.Pid))
		if err != nil {
			break // gone
		}
		state := ""
		if idx := strings.LastIndex(string(data), ")"); idx >= 0 {
			if fields := strings.Fields(string(data)[idx+1:]); len(fields) > 0 {
				state = fields[0]
			}
		}
		if state == "Z" {
			break
		}
		require.False(t, time.Now().After(deadline), "orphaned REPL process %d still alive", rogue.Process.Pid)
		time.Sleep(50 * time.Millisecond)
	}

	// The fresh child keeps serving with the same new repl_id.
	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

// TestBrowserReplOrphanedDaemonKilledOnChildDeath covers the QA finding
// that the orphan kill only ran on the API-restart path: when the owned
// child died and a foreign daemon took the socket before the next request,
// the stale socket was unlinked before the orphan-detection dial, so the
// orphan leaked for the container lifetime. The kill must run before the
// unlink on every lazy-start path.
func TestBrowserReplOrphanedDaemonKilledOnChildDeath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("orphan detection requires /proc")
	}
	script := ensureBrowserReplBundle(t)
	svc := newBrowserReplSvc(t)
	socketPath := browserReplSocketPath()

	// Start the owned child.
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)

	// Kill the API's child out from under it and give the wait goroutine a
	// moment to reap it.
	svc.browserReplMu.Lock()
	child := svc.browserRepl
	svc.browserReplMu.Unlock()
	require.NotNil(t, child)
	require.NoError(t, child.cmd.Process.Kill())
	time.Sleep(300 * time.Millisecond)

	// A foreign daemon takes the socket before the next request.
	require.NoError(t, os.Remove(socketPath))
	rogue := exec.Command("node", script)
	rogue.Env = append(os.Environ(),
		"BROWSER_REPL_SOCKET="+socketPath,
		"BROWSER_REPL_ID=rogue0000000000000000000",
	)
	require.NoError(t, rogue.Start())
	t.Cleanup(func() {
		_ = rogue.Process.Kill()
		_, _ = rogue.Process.Wait()
	})

	// Wait until the rogue is listening on the socket.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		require.False(t, time.Now().After(deadline), "rogue daemon never started listening")
		time.Sleep(50 * time.Millisecond)
	}

	// The next execution must kill the orphan (not just unlink its socket)
	// and serve from a fresh child with a different repl_id.
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1 + 1"})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(2), r.Result)
	require.NotEqual(t, "rogue0000000000000000000", r.ReplId)

	// The rogue process must be dead (zombie until this test reaps it).
	deadline = time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", rogue.Process.Pid))
		if err != nil {
			break // gone
		}
		state := ""
		if idx := strings.LastIndex(string(data), ")"); idx >= 0 {
			if fields := strings.Fields(string(data)[idx+1:]); len(fields) > 0 {
				state = fields[0]
			}
		}
		if state == "Z" {
			break
		}
		require.False(t, time.Now().After(deadline), "orphaned REPL process %d still alive", rogue.Process.Pid)
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBrowserReplRecordingIDAlias covers the QA finding that
// start_recording({recorder_id: ...}) silently ignored the field (the
// Kernel recording API's request field is `id`, while the helpers return
// `recorder_id`), breaking the documented "fresh ID per recording"
// workaround. The helpers must map recorder_id to id.
func TestBrowserReplRecordingIDAlias(t *testing.T) {
	fake := newFakeCDPServer(t)

	var mu sync.Mutex
	var startBody, stopBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		switch r.URL.Path {
		case "/recording/start":
			_ = json.Unmarshal(body, &startBody)
		case "/recording/stop":
			_ = json.Unmarshal(body, &stopBody)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(api.Close)
	apiPort := api.URL[strings.LastIndex(api.URL, ":")+1:]

	t.Setenv("CDP_ENDPOINT", fake.wsURL())
	t.Setenv("KERNEL_API_PORT", apiPort)

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		const started = await start_recording({ recorder_id: "custom-rec-1" });
		const stopped = await stop_recording({ recorder_id: "custom-rec-1" });
		({ started: started.recorder_id, stopped: stopped.recorder_id })
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	res, ok := r.Result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "custom-rec-1", res["started"])
	require.Equal(t, "custom-rec-1", res["stopped"])

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "custom-rec-1", startBody["id"], "recorder_id maps to the API's id field")
	require.NotContains(t, startBody, "recorder_id", "the alias must not leak to the API")
	require.Equal(t, "custom-rec-1", stopBody["id"])
	require.NotContains(t, stopBody, "recorder_id")
}

// TestStrictBrowserExecuteBodyMiddleware verifies that unknown request
// fields are rejected per the schema's additionalProperties: false, while
// valid and otherwise-malformed requests pass through to the strict
// handler's own decoding.
func TestStrictBrowserExecuteBodyMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	handler := StrictBrowserExecuteBodyMiddleware(next)

	// Unknown fields are rejected.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `unknown field \"bogus\"`)

	// A valid body passes through intact.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{"code":"1","timeout_sec":5,"reset":false}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"code":"1","timeout_sec":5,"reset":false}`, rec.Body.String())

	// Malformed JSON is left for the strict handler's own 400 path.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/browser/execute", strings.NewReader(`{nope`)))
	require.Equal(t, http.StatusOK, rec.Code)

	// Other methods and paths pass through untouched.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/browser/execute", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/playwright/execute", strings.NewReader(`{"code":"1","bogus":1}`)))
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestBrowserReplSourceTextModuleCells covers the JavaScript-only evaluator
// contract independently of browser connection behavior.
func TestBrowserReplSourceTextModuleCells(t *testing.T) {
	svc := newBrowserReplSvc(t)
	exec := func(code string) oapi.ExecuteBrowserCode200JSONResponse {
		t.Helper()
		return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
	}

	r := exec(`var cellVar = 1; let cellLet = 2; const cellConst = 3;
		function cellFn() { return cellVar + cellLet; }
		class CellClass {}
		({ cellVar, cellLet, cellConst, fn: cellFn(), className: new CellClass().constructor.name })`)
	require.True(t, r.Success, "error: %v", r.Error)

	r = exec(`cellVar++; cellLet += 2; cellFn = () => 99; cellVar + cellLet + cellFn()`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(105), r.Result)

	r = exec(`cellConst = 4`)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "Assignment to constant variable")

	r = exec(`const cellLet = 9`)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "Identifier 'cellLet' has already been declared")

	r = exec(`let initializedBeforeFailure = 7; throw new Error("cell failure")`)
	require.False(t, r.Success)
	r = exec(`initializedBeforeFailure`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, float64(7), r.Result)

	r = exec(`(await import("node:path")).basename("/tmp/cell")`)
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "cell", r.Result)

	r = exec(`return 1`)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "top-level return is not supported")
}
