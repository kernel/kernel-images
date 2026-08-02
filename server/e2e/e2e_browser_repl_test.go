package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
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

// restartChromium restarts the chromium supervisord service and waits for
// the DevTools proxy to serve the new browser.
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

// TestBrowserReplExecuteAPI covers the persistent runtime end to end on both
// images: binding persistence, repl_id stability (including across a
// Chromium restart), browser helpers, typed content, and explicit reset.
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
		// A modal JavaScript dialog freezes the renderer main thread, so a
		// Runtime.evaluate-based page_info would block until the CDP command
		// timeout. page_info must instead return promptly with the dialog
		// payload for alert, confirm, and prompt alike.
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
					// Let the dialog open before the first page_info so its
					// Runtime.evaluate never races the dialog opening.
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

	t.Run("recording with custom recorder ids", func(t *testing.T) {
		// Custom recorder ids delegate to the existing recording API. (The
		// recording API does not support reusing an id after stop+delete
		// within one API process lifetime, so each cycle uses a fresh id.)
		timeoutSec := 60
		r := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			TimeoutSec: &timeoutSec,
			Code: `
				const first = await start_recording({ id: "e2e-custom-a" });
				await wait(0.5);
				const stopped1 = await stop_recording({ id: "e2e-custom-a" });
				const second = await start_recording({ id: "e2e-custom-b" });
				await wait(0.5);
				const stopped2 = await stop_recording({ id: "e2e-custom-b" });
				({ first: first.recorder_id, stopped1: stopped1.recorder_id, second: second.recorder_id, stopped2: stopped2.recorder_id });
			`,
		})
		require.True(t, r.Success, "error: %s", replError(r))
		res, ok := r.Result.(map[string]interface{})
		require.True(t, ok, "expected object result, got %T", r.Result)
		require.Equal(t, "e2e-custom-a", res["first"])
		require.Equal(t, "e2e-custom-a", res["stopped1"])
		require.Equal(t, "e2e-custom-b", res["second"])
		require.Equal(t, "e2e-custom-b", res["stopped2"])
	})

	t.Run("unknown request fields are rejected", func(t *testing.T) {
		// The schema declares additionalProperties: false.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.APIBaseURL()+"/browser/execute", strings.NewReader(`{"code":"1","bogus":1}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		rsp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer rsp.Body.Close()
		require.Equal(t, http.StatusBadRequest, rsp.StatusCode, "unknown fields must be rejected")
	})

	t.Run("chromium restart preserves repl id and bindings", func(t *testing.T) {
		r1 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code: `var restartToken = "pre-restart"; await ensure_real_tab(); restartToken`,
		})
		require.True(t, r1.Success, "error: %s", replError(r1))
		require.Equal(t, "pre-restart", r1.Result)

		restartChromium(t, ctx, c, client)

		// The next helper call reconnects through the DevTools proxy and
		// reattaches; repl_id and JavaScript bindings survive the restart.
		timeoutSec := 60
		r2 := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{
			Code:       `const info = await page_info(); ({ token: restartToken, url: info.url })`,
			TimeoutSec: &timeoutSec,
		})
		require.True(t, r2.Success, "error: %s", replError(r2))
		require.Equal(t, r1.ReplId, r2.ReplId, "a Chromium restart must not change repl_id")
		res, ok := r2.Result.(map[string]interface{})
		require.True(t, ok, "expected object result, got %T", r2.Result)
		require.Equal(t, "pre-restart", res["token"], "bindings survive a Chromium restart")
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

// TestBrowserReplTimeoutTerminates verifies on both images that timeouts are
// destructive: an uninterruptible loop is killed by the API parent, an
// interruptible unresolved promise is reported by the daemon and still
// killed, each response carries the terminated repl_id with
// repl_terminated: true, and the next request lazily starts a fresh REPL.
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
