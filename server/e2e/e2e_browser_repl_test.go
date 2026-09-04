package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func replError(r *instanceoapi.BrowserReplResult) string {
	if r.Error != nil {
		return *r.Error
	}
	return "<nil>"
}

func replJSONWrite(t *testing.T, r *instanceoapi.BrowserReplResult) any {
	t.Helper()
	require.NotNil(t, r.Content)
	for i := len(*r.Content) - 1; i >= 0; i-- {
		text, err := (*r.Content)[i].AsBrowserReplTextContent()
		if err != nil || text.Channel != instanceoapi.BrowserReplTextContentChannelWrite {
			continue
		}
		var value any
		require.NoError(t, json.Unmarshal([]byte(text.Text), &value))
		return value
	}
	t.Fatal("response has no repl.write content")
	return nil
}

func executeBrowserRepl(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, body instanceoapi.ExecuteBrowserReplJSONRequestBody) *instanceoapi.BrowserReplResult {
	t.Helper()
	rsp, err := client.ExecuteBrowserReplWithResponse(ctx, body)
	require.NoError(t, err, "Browser REPL request error: %v", err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for Browser REPL: %s body=%s", rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200, "expected JSON200 response, got nil")
	return rsp.JSON200
}

func restartChromium(t *testing.T, ctx context.Context, c *TestContainer, client *instanceoapi.ClientWithResponses) {
	t.Helper()
	args := []string{"-c", "/etc/supervisor/supervisord.conf", "restart", "chromium"}
	rsp, err := client.ProcessExecWithResponse(ctx, instanceoapi.ProcessExecJSONRequestBody{
		Command: "supervisorctl",
		Args:    &args,
	})
	require.NoError(t, err, "supervisorctl restart request error: %v", err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "supervisorctl restart unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200)
	if rsp.JSON200.ExitCode != nil {
		require.Equal(t, 0, *rsp.JSON200.ExitCode, "supervisorctl restart chromium failed: stderr=%v", rsp.JSON200.StderrB64)
	}
	require.NoError(t, c.WaitDevTools(ctx), "DevTools not ready after chromium restart")
}

func TestBrowserReplAPI(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	for _, image := range []struct {
		name  string
		image string
	}{
		{"Headless", headlessImage},
		{"Headful", headfulImage},
	} {
		t.Run(image.name, func(t *testing.T) {
			t.Parallel()
			runBrowserReplAPI(t, image.image)
		})
	}
}

func runBrowserReplAPI(t *testing.T, image string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, image)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	t.Run("persistence and stable repl id", func(t *testing.T) {
		r1 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `var counter = 40; counter + 2`,
		})
		require.True(t, r1.Success, "error: %s", replError(r1))
		require.NotEmpty(t, r1.ReplId)

		r2 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `const added = await Promise.resolve(2); repl.write(JSON.stringify(counter + added))`,
		})
		require.True(t, r2.Success, "error: %s", replError(r2))
		require.Equal(t, r1.ReplId, r2.ReplId, "repl_id must be stable across calls")

		resultBytes, _ := replJSONWrite(t, r2).(float64)
		require.Equal(t, float64(42), resultBytes)
	})

	t.Run("playwright core import persists", func(t *testing.T) {
		r1 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				var pwModule = await import("playwright-core");
				var pwBrowserConnection = await pwModule.chromium.connectOverCDP(process.env.CDP_ENDPOINT);
				var pwContext = pwBrowserConnection.contexts()[0];
				var pwImportedPage = await pwContext.newPage();
				await pwImportedPage.setContent("<title>Playwright in REPL</title><main>ready</main>");
				var pwImportedPageIdentity = pwImportedPage;
				repl.write(JSON.stringify({
					connect: typeof pwModule.chromium.connectOverCDP,
					title: await pwImportedPage.title(),
				}));
			`,
		})
		require.True(t, r1.Success, "error: %s", replError(r1))
		first, ok := replJSONWrite(t, r1).(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, "function", first["connect"])
		require.Equal(t, "Playwright in REPL", first["title"])

		r2 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				repl.write(JSON.stringify({
					samePage: pwImportedPage === pwImportedPageIdentity,
					title: await pwImportedPage.title(),
				}));
				await pwImportedPage.close();
			`,
		})
		require.True(t, r2.Success, "error: %s", replError(r2))
		require.Equal(t, r1.ReplId, r2.ReplId)
		second, ok := replJSONWrite(t, r2).(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, true, second["samePage"])
		require.Equal(t, "Playwright in REPL", second["title"])

		reset := true
		resetResult := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{Code: "", Reset: &reset})
		require.True(t, resetResult.Success, "error: %s", replError(resetResult))
	})

	t.Run("browser helpers", func(t *testing.T) {
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				await ensureRealTab();
				await gotoUrl("data:text/html,<title>Browser REPL Helper</title>");
				await waitForLoad();
				const info = await pageInfo();
				repl.write(JSON.stringify(info.title));
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		require.Equal(t, "Browser REPL Helper", replJSONWrite(t, r))
		require.NotNil(t, r.Content)
		require.NotEmpty(t, *r.Content)
		first, err := (*r.Content)[0].AsBrowserReplTextContent()
		require.NoError(t, err)
		require.Equal(t, `"Browser REPL Helper"`, first.Text)
	})

	t.Run("selector interaction and element states", func(t *testing.T) {
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				await ensureRealTab();
				await gotoUrl("data:text/html,<body></body>");
				await waitForLoad();
				await js(() => {
					document.body.innerHTML = ` + "`" + `
						<button class="action" hidden>hidden duplicate</button>
						<button class="action">Search</button>
						<input class="field" hidden>
						<input class="field">
						<div id="status" hidden>ready</div>
						<div id="remove-me"></div>
					` + "`" + `;
					globalThis.__clickCount = 0;
					document.querySelectorAll(".action")[1].addEventListener("click", () => {
						globalThis.__clickCount++;
						const status = document.querySelector("#status");
						status.hidden = false;
						setTimeout(() => { status.hidden = true; }, 250);
					});
				});

				const attached = await waitForElement(".action", {state: "attached", timeoutSec: 2});
				await click(".action");
				const visible = await waitForElement("#status", {state: "visible", timeoutSec: 2});
				await fillInput(".field", "hello", {timeoutSec: 2});
				const hidden = await waitForElement("#status", {state: "hidden", timeoutSec: 2});
				await js(() => setTimeout(() => document.querySelector("#remove-me").remove(), 50));
				const detached = await waitForElement("#remove-me", {state: "detached", timeoutSec: 2});
				const state = await js(() => ({
					clickCount: globalThis.__clickCount,
					value: [...document.querySelectorAll(".field")].find(element => !element.hidden).value,
				}));
				repl.write(JSON.stringify({attached, visible, hidden, detached, ...state}));
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		state, ok := replJSONWrite(t, r).(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, true, state["attached"])
		require.Equal(t, true, state["visible"])
		require.Equal(t, true, state["hidden"])
		require.Equal(t, true, state["detached"])
		require.Equal(t, float64(1), state["clickCount"])
		require.Equal(t, "hello", state["value"])
	})

	t.Run("cross-origin iframe target evaluation", func(t *testing.T) {
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				await gotoUrl("data:text/html,<body></body>");
				await waitForLoad();
				await js(() => {
					const iframe = document.createElement("iframe");
					iframe.src = "http://127.0.0.1:10001/spec.yaml";
					document.body.append(iframe);
				});
				var crossOriginFrame = null;
				for (let attempt = 0; attempt < 50 && !crossOriginFrame; attempt++) {
					crossOriginFrame = await iframeTarget("127.0.0.1:10001/spec.yaml");
					if (!crossOriginFrame) await waitMs(100);
				}
				if (!crossOriginFrame) throw new Error("cross-origin iframe target did not appear");
				var crossOriginState = await js(
					() => ({documentNodeType: document.nodeType}),
					{targetId: crossOriginFrame.targetId},
				);
				repl.write(JSON.stringify({target: crossOriginFrame, state: crossOriginState}));
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		result, ok := replJSONWrite(t, r).(map[string]interface{})
		require.True(t, ok)
		target, ok := result["target"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, "iframe", target["type"])
		require.Contains(t, target["url"], "127.0.0.1:10001/spec.yaml")
		state, ok := result["state"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, float64(9), state["documentNodeType"])
	})

	t.Run("page evaluation modes", func(t *testing.T) {
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				await ensureRealTab();
				await gotoUrl("data:text/html,<title>page-evaluation</title><main>hello</main>");
				await waitForLoad();
				await js("globalThis.__browserReplEvaluationCount = 0");
				const result = {
					expression: await js("1 + 1"),
					promiseExpression: await js("Promise.resolve(3)"),
					asyncFunction: await js(async () => {
						globalThis.__browserReplEvaluationCount++;
						const value = await Promise.resolve(4);
						return { value, count: globalThis.__browserReplEvaluationCount };
					}),
					argument: await js(({ a, b, edge }) => ({
						sum: a + b,
						nan: Number.isNaN(edge.nan),
						negativeZero: Object.is(edge.negativeZero, -0),
						bigint: edge.bigint.toString(),
					}), { arg: { a: 2, b: 3, edge: { nan: NaN, negativeZero: -0, bigint: 42n } } }),
				};
				repl.write(JSON.stringify(result));
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		result, ok := replJSONWrite(t, r).(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, float64(2), result["expression"])
		require.Equal(t, float64(3), result["promiseExpression"])
		asyncResult, ok := result["asyncFunction"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, float64(4), asyncResult["value"])
		require.Equal(t, float64(1), asyncResult["count"], "the page function executes exactly once")
		argument, ok := result["argument"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, float64(5), argument["sum"])
		require.Equal(t, true, argument["nan"])
		require.Equal(t, true, argument["negativeZero"])
		require.Equal(t, "42", argument["bigint"])
	})

	t.Run("US keyboard normalization", func(t *testing.T) {
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				await ensureRealTab();
				await gotoUrl("data:text/html,<input id=q autofocus>");
				await waitForElement("#q", {state: "visible", timeoutSec: 10});
				await js(() => {
					globalThis.__keyEvents = [];
					const input = document.querySelector("#q");
					input.addEventListener("keydown", event => {
						globalThis.__keyEvents.push({ key: event.key, code: event.code, keyCode: event.keyCode, shift: event.shiftKey });
					});
					input.focus();
				});
				await pressKey("ENTER");
				await pressKey("Digit1", ["shift"]);
				await pressKey("Esc");
				const events = await js(() => globalThis.__keyEvents);
				repl.write(JSON.stringify(events));
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		events, ok := replJSONWrite(t, r).([]interface{})
		require.True(t, ok)
		require.Len(t, events, 3)
		for i, expected := range []struct {
			key  string
			code string
		}{
			{key: "Enter", code: "Enter"},
			{key: "!", code: "Digit1"},
			{key: "Escape", code: "Escape"},
		} {
			event, ok := events[i].(map[string]interface{})
			require.True(t, ok)
			require.Equal(t, expected.key, event["key"])
			require.Equal(t, expected.code, event["code"])
		}
	})

	t.Run("tab management and screenshots", func(t *testing.T) {
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				const before = (await listTabs(false)).length;
				const tab = await newTab("data:text/html,<title>Screenshot Test</title><main>ready</main>");
				await waitForLoad();
				const tabs = await listTabs();
				const shot = await captureScreenshot("/tmp/e2e-repl.png", false, 800);
				await repl.emitImage({ path: shot });
				await closeTab(tab);
				({ before, after: tabs.length, shot });
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		require.NotNil(t, r.Content)
		var sawImage bool
		for _, item := range *r.Content {
			if img, err := item.AsBrowserReplImageContent(); err == nil && img.Type == "image" {
				sawImage = true
				require.Equal(t, "image/png", img.MimeType)
				require.NotEmpty(t, img.DataB64)
			}
		}
		require.True(t, sawImage, "expected an emitted screenshot image in content")
	})

	t.Run("pending dialogs reported by pageInfo", func(t *testing.T) {
		timeoutSec := 60
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await ensureRealTab();
				await gotoUrl("data:text/html,<title>dlg</title><p>x</p>");
				const seen = [];
				for (const [type, source] of [
					["alert", 'alert("hello-alert")'],
					["confirm", 'confirm("hello-confirm")'],
					["prompt", 'prompt("hello-prompt")'],
				]) {
					await cdp("Runtime.evaluate", { expression: "setTimeout(() => { " + source + "; }, 100)" });
					await waitMs(1000);
					let info = null;
					for (let i = 0; i < 20; i++) {
						info = await pageInfo();
						if (info.dialog) break;
						await waitMs(200);
					}
					seen.push({ want: type, got: info.dialog && info.dialog.type, message: info.dialog && info.dialog.message, url: info.url });
					if (info.dialog) {
						await cdp("Page.handleJavaScriptDialog", { accept: true });
						await waitMs(300);
					}
				}
				repl.write(JSON.stringify(seen));
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		seen, ok := replJSONWrite(t, r).([]interface{})
		require.True(t, ok, "expected array result, got %T", replJSONWrite(t, r))
		require.Len(t, seen, 3)
		for i, want := range []string{"alert", "confirm", "prompt"} {
			entry, ok := seen[i].(map[string]interface{})
			require.True(t, ok)
			require.Equal(t, want, entry["want"])
			require.Equal(t, want, entry["got"], "pageInfo must report the pending %s dialog", want)
			require.Contains(t, entry["message"], "hello-"+want)
			require.Contains(t, entry["url"], "data:text/html", "pageInfo still reports last-known target metadata")
		}
	})

	t.Run("stale pre-attach dialog does not brick the endpoint", func(t *testing.T) {
		timeoutSec := 60
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await ensureRealTab();
				await gotoUrl("data:text/html,<title>stale-dialog</title><p>x</p>");
				await js("setTimeout(() => alert('stale'), 50)");
				await waitMs(500);
				"dialog-open"
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))

		killArgs := []string{"-f", "browser-repl.js"}
		killRsp, err := client.ProcessExecWithResponse(ctx, instanceoapi.ProcessExecJSONRequestBody{
			Command: "pkill",
			Args:    &killArgs,
		})
		require.NoError(t, err, "pkill request error: %v", err)
		require.Equal(t, http.StatusOK, killRsp.StatusCode(), "pkill unexpected status: %s", killRsp.Status())
		time.Sleep(time.Second)

		shortTimeout := 10
		r2 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			TimeoutSec: &shortTimeout,
			Code:       `await pageInfo()`,
		})
		require.False(t, r2.Success, "pageInfo on a frozen renderer must fail")
		require.NotNil(t, r2.Error)
		require.True(t, r2.ReplTerminated == nil || !*r2.ReplTerminated,
			"a frozen renderer must not destroy the REPL: %s", replError(r2))

		r3 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `(await listTabs()).length`,
		})
		require.True(t, r3.Success, "error: %s", replError(r3))
		require.Equal(t, r2.ReplId, r3.ReplId, "the REPL must survive the frozen renderer")

		r4 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await cdp("Page.reload");
				await waitMs(1000);
				const info = await pageInfo();
				repl.write(JSON.stringify({ url: info.url, title: info.title }))
			`,
		})
		require.True(t, r4.Success, "error: %s", replError(r4))
		require.Equal(t, r2.ReplId, r4.ReplId, "recovery must not replace the REPL")
		recovered, ok := replJSONWrite(t, r4).(map[string]interface{})
		require.True(t, ok, "expected object result, got %T", replJSONWrite(t, r4))
		require.Contains(t, recovered["url"], "data:text/html")
	})

	t.Run("chromium restart preserves repl id and bindings", func(t *testing.T) {
		r1 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				var restartToken = "pre-restart";
				var pwRestartModule = await import("playwright-core");
				var pwRestartBrowser = await pwRestartModule.chromium.connectOverCDP(process.env.CDP_ENDPOINT);
				await ensureRealTab();
				repl.write(JSON.stringify({ token: restartToken, playwrightConnected: pwRestartBrowser.isConnected() }));
			`,
		})
		require.True(t, r1.Success, "error: %s", replError(r1))
		before, ok := replJSONWrite(t, r1).(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, "pre-restart", before["token"])
		require.Equal(t, true, before["playwrightConnected"])

		restartChromium(t, ctx, c, client)

		timeoutSec := 60
		r2 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `
				var restartInfo = await pageInfo();
				for (var pwDisconnectAttempt = 0; pwDisconnectAttempt < 20 && pwRestartBrowser.isConnected(); pwDisconnectAttempt++) {
					await waitMs(50);
				}
				var stalePlaywrightDisconnected = !pwRestartBrowser.isConnected();
				var pwReplacementBrowser = await pwRestartModule.chromium.connectOverCDP(process.env.CDP_ENDPOINT);
				var pwReplacementContext = pwReplacementBrowser.contexts()[0];
				var pwReplacementPage = await pwReplacementContext.newPage();
				await pwReplacementPage.setContent("<title>Playwright reconnected</title>");
				var pwReplacementTitle = await pwReplacementPage.title();
				await pwReplacementPage.close();
				repl.write(JSON.stringify({
					token: restartToken,
					url: restartInfo.url,
					stalePlaywrightDisconnected,
					playwrightTitle: pwReplacementTitle,
				}));
			`,
			TimeoutSec: &timeoutSec,
		})
		require.True(t, r2.Success, "error: %s", replError(r2))
		require.Equal(t, r1.ReplId, r2.ReplId, "a Chromium restart must not change repl_id")
		res, ok := replJSONWrite(t, r2).(map[string]interface{})
		require.True(t, ok, "expected object output, got %T", replJSONWrite(t, r2))
		require.Equal(t, "pre-restart", res["token"], "bindings survive a Chromium restart")
		require.Equal(t, true, res["stalePlaywrightDisconnected"], "the old Playwright connection becomes stale")
		require.Equal(t, "Playwright reconnected", res["playwrightTitle"], "Playwright can reconnect inside the same REPL")

		reset := true
		resetResult := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{Code: "", Reset: &reset})
		require.True(t, resetResult.Success, "error: %s", replError(resetResult))
	})

	t.Run("dialogs stay pending after a chromium restart", func(t *testing.T) {
		restartChromium(t, ctx, c, client)

		timeoutSec := 60
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await ensureRealTab();
				await gotoUrl("data:text/html,<title>dlg-restart</title><p>x</p>");
				await cdp("Runtime.evaluate", { expression: "setTimeout(() => { alert('post-restart'); }, 100)" });
				await waitMs(1000);
				let restartDlgInfo = null;
				for (let i = 0; i < 20; i++) {
					restartDlgInfo = await pageInfo();
					if (restartDlgInfo.dialog) break;
					await waitMs(200);
				}
				const restartDlgType = restartDlgInfo.dialog && restartDlgInfo.dialog.type;
				if (restartDlgInfo.dialog) {
					await cdp("Page.handleJavaScriptDialog", { accept: true });
				}
				repl.write(JSON.stringify(restartDlgType));
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		require.Equal(t, "alert", replJSONWrite(t, r), "pageInfo must report a dialog opened after a chromium restart")
	})

	t.Run("reset clears bindings and changes repl id", func(t *testing.T) {
		before := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `repl.write(JSON.stringify(repl.id))`,
		})
		require.True(t, before.Success)

		reset := true
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code:  "",
			Reset: &reset,
		})
		require.True(t, r.Success)
		require.NotEqual(t, before.ReplId, r.ReplId, "reset must produce a new repl_id")

		r2 := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code: `repl.write(JSON.stringify(typeof counter))`,
		})
		require.True(t, r2.Success)
		require.Equal(t, "undefined", replJSONWrite(t, r2), "reset must clear prior bindings")
	})

	runBrowserReplTimeoutCases(t, ctx, client)
}

func runBrowserReplTimeoutCases(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Run("uninterruptible loop", func(t *testing.T) {
		warm := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{Code: "1"})
		require.True(t, warm.Success)

		timeoutSec := 2
		start := time.Now()
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code:       "while (true) {}",
			TimeoutSec: &timeoutSec,
		})
		elapsed := time.Since(start)

		require.False(t, r.Success)
		require.Equal(t, warm.ReplId, r.ReplId, "timeout response carries the terminated REPL's ID")
		require.NotNil(t, r.ReplTerminated)
		require.True(t, *r.ReplTerminated)
		require.NotNil(t, r.Error)
		require.Contains(t, *r.Error, "execution timed out after 2000ms", "timeout paths share one message")
		require.Less(t, elapsed, 15*time.Second, "timeout must kill the REPL promptly")

		fresh := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{Code: "'fresh'"})
		require.True(t, fresh.Success)
		require.NotEqual(t, warm.ReplId, fresh.ReplId, "the next request starts a fresh REPL with a new ID")
		fmt.Println("timeout recovery complete, new repl_id:", fresh.ReplId)
	})

	t.Run("unresolved promise", func(t *testing.T) {
		warm := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{Code: "1"})
		require.True(t, warm.Success)

		timeoutSec := 2
		start := time.Now()
		r := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{
			Code:       "await new Promise(() => {})",
			TimeoutSec: &timeoutSec,
		})
		elapsed := time.Since(start)

		require.False(t, r.Success)
		require.Equal(t, warm.ReplId, r.ReplId, "timeout response carries the terminated REPL's ID")
		require.NotNil(t, r.ReplTerminated)
		require.True(t, *r.ReplTerminated, "an interruptible timeout is still destructive")
		require.Less(t, elapsed, 30*time.Second, "timeout must kill the REPL promptly")

		fresh := executeBrowserRepl(t, ctx, client, instanceoapi.ExecuteBrowserReplJSONRequestBody{Code: "'fresh'"})
		require.True(t, fresh.Success)
		require.NotEqual(t, warm.ReplId, fresh.ReplId, "the next request starts a fresh REPL with a new ID")
	})
}
