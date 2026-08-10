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
	"strings"
	"testing"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/stretchr/testify/require"
)

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
