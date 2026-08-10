package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
