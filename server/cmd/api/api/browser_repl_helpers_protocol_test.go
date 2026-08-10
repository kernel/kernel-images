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

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

func TestBrowserReplHelperErgonomics(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())
	svc := newBrowserReplSvc(t)

	requireExecError(t, svc, `await press_key("a", "Control")`, "press_key: modifiers must be an array")
	requireExec(t, svc, `await press_key("a", {ctrl: true}); "ok"`, "ok")
	keyEv := fake.lastKeyEventParams()
	require.NotNil(t, keyEv)
	require.Equal(t, float64(2), keyEv["modifiers"])
	requireExecError(t, svc, `await press_key("a", {bogus: true})`, "press_key: unknown modifier")
	requireExecError(t, svc, `await js("1", {target: "target-page-1"})`, "js: target must be a target id string")
	requireExec(t, svc, `await js("via-target", "target-page-1")`, "via-target")
	requireExecError(t, svc, `await wait_for_element("#never", false, 2)`, "wait_for_element: opts must be an object")

	timeoutSec := 3
	start := time.Now()
	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await wait_for_element("#never")`, TimeoutSec: &timeoutSec})
	require.Less(t, time.Since(start), 3*time.Second)
	require.False(t, r.Success)
	require.Contains(t, *r.Error, "timed out waiting for element")
	require.Nil(t, r.ReplTerminated)
	requireExec(t, svc, "repl.id", r.ReplId)
}

func TestBrowserReplFrozenRendererRecovery(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await page_info()).title`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "Example Domain", r.Result)

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

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await list_tabs()).length`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, frozenID, r.ReplId, "the REPL must survive the frozen renderer")

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

	fake.hangSession.Store(false)
	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await page_info()).title`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "Example Domain", r.Result)
	require.Equal(t, frozenID, r.ReplId, "recovery must not replace the REPL")
}

func TestBrowserReplCrashDuringExecutionResponse(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r1.Success)

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

	r3 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
	require.True(t, r3.Success)
	require.NotEqual(t, r1.ReplId, r3.ReplId)
}

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

func TestBrowserReplEventRingBounded(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "attached"`})
	require.True(t, r.Success, "error: %v", r.Error)

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

	fake.Restart()
	require.Eventually(t, func() bool { return fake.connCount() == 0 },
		5*time.Second, 10*time.Millisecond, "daemon connection must close")

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

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `({a: [1, 2, 3], b: "str"})`})
	require.True(t, r.Success, "error: %v", r.Error)
	res, ok := r.Result.(map[string]any)
	require.True(t, ok, "expected object result, got %T", r.Result)
	require.Equal(t, []any{float64(1), float64(2), float64(3)}, res["a"])
	require.Equal(t, "str", res["b"])

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `repl.write("frame-ok"); "done"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "done", r.Result)
	require.NotNil(t, r.Content)
	require.Len(t, *r.Content, 1)
	txt, err := (*r.Content)[0].AsBrowserExecutionTextContent()
	require.NoError(t, err)
	require.Equal(t, "frame-ok", txt.Text)
}

func TestBrowserReplPageInfoReportsPendingDialog(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "attached"`})
	require.True(t, r.Success, "error: %v", r.Error)

	fake.frozen.Store(true)
	fake.queueEvent(map[string]any{
		"method":    "Page.javascriptDialogOpening",
		"params":    map[string]any{"type": "alert", "message": "hello-dialog"},
		"sessionId": "session-target-page-1",
	})

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await cdp("Target.getTargets", undefined, null); "flushed"`})
	require.True(t, r.Success, "error: %v", r.Error)
	time.Sleep(200 * time.Millisecond)

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

func TestBrowserReplAttachRetriesStaleTarget(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await ensure_real_tab(); "attached"`})
	require.True(t, r.Success, "error: %v", r.Error)

	fake.Restart()
	require.Eventually(t, func() bool { return fake.connCount() == 0 },
		5*time.Second, 10*time.Millisecond, "daemon connection must close")
	fake.failNextAttach.Store(1)

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `(await page_info()).title`})
	require.True(t, r.Success, "a transient stale-target attach must be retried: %v", r.Error)
	require.Equal(t, "Example Domain", r.Result)
	require.Equal(t, int32(0), fake.failNextAttach.Load(), "the first attach attempt failed as planned")
}

func TestBrowserReplRetriesCommandOnFreshConnectionClose(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r1 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{
		Code: `var retryToken = "pre-restart"; await ensure_real_tab(); retryToken`,
	})
	require.True(t, r1.Success, "error: %v", r1.Error)
	require.Equal(t, int32(1), fake.totalConns.Load())

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

func TestBrowserReplUnhandledRejectionSurvives(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `
		var kept = 'state-kept';
		setTimeout(() => { Promise.reject(new Error('boom-floating')); }, 20);
		'submitted'
	`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "submitted", r.Result)

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

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

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

	r = execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `typeof doomed`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "undefined", r.Result)
	require.NotEqual(t, doomedID, r.ReplId)

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

func TestBrowserReplRequestLineCapEnforced(t *testing.T) {
	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
	require.True(t, r.Success)

	conn, err := net.Dial("unix", browserReplSocketPath())
	require.NoError(t, err)
	defer conn.Close()

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

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}

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

func TestBrowserReplScrollFallback(t *testing.T) {
	fake := newFakeCDPServer(t)
	t.Setenv("CDP_ENDPOINT", fake.wsURL())

	svc := newBrowserReplSvc(t)

	r := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: `await scroll(100, 100, 0, 240); "ok"`})
	require.True(t, r.Success, "error: %v", r.Error)
	require.Equal(t, "ok", r.Result)
	require.False(t, fake.sawScrollBy.Load(), "no fallback when mouseWheel answers")

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

	r2 := execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: "repl.id"})
	require.True(t, r2.Success)
	require.Equal(t, r.ReplId, r2.Result)
}
