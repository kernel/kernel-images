package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func replError(r *instanceoapi.ExecuteBrowserCodeResult) string {
	if r.Error != nil {
		return *r.Error
	}
	return "<nil>"
}

func executeBrowserCode(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, body instanceoapi.ExecuteBrowserCodeJSONRequestBody) *instanceoapi.ExecuteBrowserCodeResult {
	t.Helper()
	rsp, err := client.ExecuteBrowserCodeWithResponse(ctx, body)
	require.NoError(t, err, "browser execute request error: %v", err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for browser execute: %s body=%s", rsp.Status(), string(rsp.Body))
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

func TestBrowserReplExecuteAPI(t *testing.T) {
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
			runBrowserReplExecuteAPI(t, image.image)
		})
	}
}

func runBrowserReplExecuteAPI(t *testing.T, image string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, image)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	t.Run("persistence and stable repl id", func(t *testing.T) {
		r1 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `var counter = 40; counter + 2`,
		})
		require.True(t, r1.Success, "error: %s", replError(r1))
		require.NotEmpty(t, r1.ReplId)

		r2 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `const added = await Promise.resolve(2); counter + added`,
		})
		require.True(t, r2.Success, "error: %s", replError(r2))
		require.Equal(t, r1.ReplId, r2.ReplId, "repl_id must be stable across calls")

		resultBytes, _ := r2.Result.(float64)
		require.Equal(t, float64(42), resultBytes)
	})

	t.Run("browser helpers", func(t *testing.T) {
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `
				await ensure_real_tab();
				await goto_url("https://example.com");
				await wait_for_load();
				const info = await page_info();
				repl.write(info.title);
				info.title;
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		require.Equal(t, "Example Domain", r.Result)
		require.NotNil(t, r.Content)
		require.NotEmpty(t, *r.Content)
		first, err := (*r.Content)[0].AsBrowserExecutionTextContent()
		require.NoError(t, err)
		require.Equal(t, "Example Domain", first.Text)
	})

	t.Run("tab management and screenshots", func(t *testing.T) {
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `
				const before = (await list_tabs()).length;
				const tab = await new_tab("https://example.com");
				await wait_for_load();
				const tabs = await list_tabs();
				const shot = await capture_screenshot("/tmp/e2e-repl.png", false, 800);
				await repl.emitImage({ path: shot });
				await close_tab(tab.id);
				({ before, after: tabs.length, shot });
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		require.NotNil(t, r.Content)
		var sawImage bool
		for _, item := range *r.Content {
			if img, err := item.AsBrowserExecutionImageContent(); err == nil && img.Type == "image" {
				sawImage = true
				require.Equal(t, "image/png", img.MimeType)
				require.NotEmpty(t, img.DataB64)
			}
		}
		require.True(t, sawImage, "expected an emitted screenshot image in content")
	})

	t.Run("pending dialogs reported by page_info", func(t *testing.T) {
		timeoutSec := 60
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await ensure_real_tab();
				await goto_url("data:text/html,<title>dlg</title><p>x</p>");
				const seen = [];
				for (const [type, source] of [
					["alert", 'alert("hello-alert")'],
					["confirm", 'confirm("hello-confirm")'],
					["prompt", 'prompt("hello-prompt")'],
				]) {
					await cdp("Runtime.evaluate", { expression: "setTimeout(() => { " + source + "; }, 100)" });
					await wait(1);
					let info = null;
					for (let i = 0; i < 20; i++) {
						info = await page_info();
						if (info.dialog) break;
						await wait(0.2);
					}
					seen.push({ want: type, got: info.dialog && info.dialog.type, message: info.dialog && info.dialog.message, url: info.url });
					if (info.dialog) {
						await cdp("Page.handleJavaScriptDialog", { accept: true });
						await wait(0.3);
					}
				}
				seen;
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		seen, ok := r.Result.([]interface{})
		require.True(t, ok, "expected array result, got %T", r.Result)
		require.Len(t, seen, 3)
		for i, want := range []string{"alert", "confirm", "prompt"} {
			entry, ok := seen[i].(map[string]interface{})
			require.True(t, ok)
			require.Equal(t, want, entry["want"])
			require.Equal(t, want, entry["got"], "page_info must report the pending %s dialog", want)
			require.Contains(t, entry["message"], "hello-"+want)
			require.Contains(t, entry["url"], "data:text/html", "page_info still reports last-known target metadata")
		}
	})

	t.Run("stale pre-attach dialog does not brick the endpoint", func(t *testing.T) {
		timeoutSec := 60
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await ensure_real_tab();
				await goto_url("data:text/html,<title>stale-dialog</title><p>x</p>");
				await js("setTimeout(() => alert('stale'), 50)");
				await wait(0.5);
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
		r2 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			TimeoutSec: &shortTimeout,
			Code:       `await page_info()`,
		})
		require.False(t, r2.Success, "page_info on a frozen renderer must fail")
		require.NotNil(t, r2.Error)
		require.True(t, r2.ReplTerminated == nil || !*r2.ReplTerminated,
			"a frozen renderer must not destroy the REPL: %s", replError(r2))

		r3 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `(await list_tabs()).length`,
		})
		require.True(t, r3.Success, "error: %s", replError(r3))
		require.Equal(t, r2.ReplId, r3.ReplId, "the REPL must survive the frozen renderer")

		r4 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await cdp("Page.reload");
				await wait(1);
				const info = await page_info();
				({ url: info.url, title: info.title })
			`,
		})
		require.True(t, r4.Success, "error: %s", replError(r4))
		require.Equal(t, r2.ReplId, r4.ReplId, "recovery must not replace the REPL")
		recovered, ok := r4.Result.(map[string]interface{})
		require.True(t, ok, "expected object result, got %T", r4.Result)
		require.Contains(t, recovered["url"], "data:text/html")
	})

	t.Run("recording with custom recorder id", func(t *testing.T) {
		timeoutSec := 60
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				const started = await start_recording({ id: "e2e-custom" });
				await wait(0.5);
				const stopped = await stop_recording({ id: "e2e-custom" });
				({ started: started.recorder_id, stopped: stopped.recorder_id });
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		res, ok := r.Result.(map[string]interface{})
		require.True(t, ok, "expected object result, got %T", r.Result)
		require.Equal(t, "e2e-custom", res["started"])
		require.Equal(t, "e2e-custom", res["stopped"])
	})

	t.Run("chromium restart preserves repl id and bindings", func(t *testing.T) {
		r1 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `var restartToken = "pre-restart"; await ensure_real_tab(); restartToken`,
		})
		require.True(t, r1.Success, "error: %s", replError(r1))
		require.Equal(t, "pre-restart", r1.Result)

		restartChromium(t, ctx, c, client)

		timeoutSec := 60
		r2 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code:       `const restartInfo = await page_info(); ({ token: restartToken, url: restartInfo.url })`,
			TimeoutSec: &timeoutSec,
		})
		require.True(t, r2.Success, "error: %s", replError(r2))
		require.Equal(t, r1.ReplId, r2.ReplId, "a Chromium restart must not change repl_id")
		res, ok := r2.Result.(map[string]interface{})
		require.True(t, ok, "expected object result, got %T", r2.Result)
		require.Equal(t, "pre-restart", res["token"], "bindings survive a Chromium restart")
	})

	t.Run("dialogs stay pending after a chromium restart", func(t *testing.T) {
		restartChromium(t, ctx, c, client)

		timeoutSec := 60
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				await ensure_real_tab();
				await goto_url("data:text/html,<title>dlg-restart</title><p>x</p>");
				await cdp("Runtime.evaluate", { expression: "setTimeout(() => { alert('post-restart'); }, 100)" });
				await wait(1);
				let restartDlgInfo = null;
				for (let i = 0; i < 20; i++) {
					restartDlgInfo = await page_info();
					if (restartDlgInfo.dialog) break;
					await wait(0.2);
				}
				const restartDlgType = restartDlgInfo.dialog && restartDlgInfo.dialog.type;
				if (restartDlgInfo.dialog) {
					await cdp("Page.handleJavaScriptDialog", { accept: true });
				}
				restartDlgType;
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		require.Equal(t, "alert", r.Result, "page_info must report a dialog opened after a chromium restart")
	})

	t.Run("reset clears bindings and changes repl id", func(t *testing.T) {
		before := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `repl.id`,
		})
		require.True(t, before.Success)

		reset := true
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code:  "",
			Reset: &reset,
		})
		require.True(t, r.Success)
		require.NotEqual(t, before.Result, r.ReplId, "reset must produce a new repl_id")

		r2 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `typeof counter`,
		})
		require.True(t, r2.Success)
		require.Equal(t, "undefined", r2.Result, "reset must clear prior bindings")
	})
}

func TestBrowserReplTimeoutTerminates(t *testing.T) {
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
			runBrowserReplTimeoutTerminates(t, image.image)
		})
	}
}

func runBrowserReplTimeoutTerminates(t *testing.T, image string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, image)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	t.Run("uninterruptible loop", func(t *testing.T) {
		warm := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
		require.True(t, warm.Success)

		timeoutSec := 2
		start := time.Now()
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
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

		fresh := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
		require.True(t, fresh.Success)
		require.NotEqual(t, warm.ReplId, fresh.ReplId, "the next request starts a fresh REPL with a new ID")
		fmt.Println("timeout recovery complete, new repl_id:", fresh.ReplId)
	})

	t.Run("unresolved promise", func(t *testing.T) {
		warm := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{Code: "1"})
		require.True(t, warm.Success)

		timeoutSec := 2
		start := time.Now()
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code:       "await new Promise(() => {})",
			TimeoutSec: &timeoutSec,
		})
		elapsed := time.Since(start)

		require.False(t, r.Success)
		require.Equal(t, warm.ReplId, r.ReplId, "timeout response carries the terminated REPL's ID")
		require.NotNil(t, r.ReplTerminated)
		require.True(t, *r.ReplTerminated, "an interruptible timeout is still destructive")
		require.Less(t, elapsed, 30*time.Second, "timeout must kill the REPL promptly")

		fresh := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
		require.True(t, fresh.Success)
		require.NotEqual(t, warm.ReplId, fresh.ReplId, "the next request starts a fresh REPL with a new ID")
	})
}
