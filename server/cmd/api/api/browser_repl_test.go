package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
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

	http *httptest.Server
}

// fakeCDPTinyPNG is a valid 1x1 PNG; capture_screenshot writes it to disk
// and repl.emitImage sniffs its magic bytes.
const fakeCDPTinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func newFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()
	f := &fakeCDPServer{
		t:     t,
		conns: map[*websocket.Conn]struct{}{},
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

func (f *fakeCDPServer) handler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
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
		result, events, dispatchErr := f.dispatch(req.Method, req.Params)
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
	}
}

// dispatch handles one CDP command, returning the command result plus any
// events to emit after the response.
func (f *fakeCDPServer) dispatch(method string, params json.RawMessage) (any, []map[string]any, error) {
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
	case "Input.dispatchMouseEvent", "Input.insertText", "Input.dispatchKeyEvent":
		return map[string]any{}, nil, nil
	case "DOM.getDocument":
		return map[string]any{"root": map[string]any{"nodeId": 1}}, nil, nil
	case "DOM.querySelector":
		return map[string]any{"nodeId": 42}, nil, nil
	case "DOM.setFileInputFiles":
		return map[string]any{}, nil, nil
	case "Runtime.evaluate":
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(params, &p)
		return map[string]any{
			"result": map[string]any{"type": "object", "value": f.evalExpression(p.Expression)},
		}, nil, nil
	}
	return nil, nil, fmt.Errorf("fake CDP: unhandled method %s", method)
}

// evalExpression returns canned values for the expressions the helpers
// evaluate in the page, and echoes anything else so js() round-trips can be
// verified.
func (f *fakeCDPServer) evalExpression(expr string) any {
	switch {
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
	case strings.HasPrefix(expr, "(function (selector, value)"),
		strings.HasPrefix(expr, "(function (selector, key, opts)"),
		strings.HasPrefix(expr, "(function (selector, requireVisible)"):
		// fill_input, dispatch_key, wait_for_element IIFEs.
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
