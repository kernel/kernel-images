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

// TestBrowserReplExecuteAPI covers the persistent runtime end to end:
// binding persistence, repl_id stability, browser helpers, typed content,
// explicit reset, and timeout termination.
func TestBrowserReplExecuteAPI(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
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
			if img, err := item.AsBrowserExecutionImageContent(); err == nil {
				sawImage = true
				require.Equal(t, "image/png", img.MimeType)
				require.NotEmpty(t, img.DataB64)
			}
		}
		require.True(t, sawImage, "expected an emitted screenshot image in content")
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

// TestBrowserReplTimeoutTerminates verifies that an uninterruptible loop is
// killed by the API parent, the response carries the terminated repl_id, and
// the next request lazily starts a fresh REPL.
func TestBrowserReplTimeoutTerminates(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

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
	require.Less(t, elapsed, 30*time.Second, "timeout must kill the REPL promptly")

	fresh := executeBrowserCode(t, ctx, client, instanceoapi.ExecuteBrowserCodeJSONRequestBody{Code: "'fresh'"})
	require.True(t, fresh.Success)
	require.NotEqual(t, warm.ReplId, fresh.ReplId, "the next request starts a fresh REPL with a new ID")
	fmt.Println("timeout recovery complete, new repl_id:", fresh.ReplId)
}
