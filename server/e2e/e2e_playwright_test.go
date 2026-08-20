package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestPlaywrightExecuteAPI(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	playwrightCode := `
		await page.goto('https://example.com');
		const title = await page.title();
		return title;
	`

	t.Log("executing playwright code")
	req := instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: playwrightCode,
	}

	rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, req)
	require.NoError(t, err, "playwright execute request error: %v", err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for playwright execute: %s body=%s", rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200, "expected JSON200 response, got nil")

	if !rsp.JSON200.Success {
		var errorMsg string
		if rsp.JSON200.Error != nil {
			errorMsg = *rsp.JSON200.Error
		}
		var stdout, stderr string
		if rsp.JSON200.Stdout != nil {
			stdout = *rsp.JSON200.Stdout
		}
		if rsp.JSON200.Stderr != nil {
			stderr = *rsp.JSON200.Stderr
		}
		t.Logf("error=%s stdout=%s stderr=%s", errorMsg, stdout, stderr)
	}

	require.True(t, rsp.JSON200.Success, "expected success=true, got success=false. Error: %s", func() string {
		if rsp.JSON200.Error != nil {
			return *rsp.JSON200.Error
		}
		return "nil"
	}())
	require.NotNil(t, rsp.JSON200.Result, "expected result to be non-nil")

	resultBytes, err := json.Marshal(rsp.JSON200.Result)
	require.NoError(t, err, "failed to marshal result: %v", err)
	resultStr := string(resultBytes)
	t.Logf("result=%s", resultStr)
	require.Contains(t, resultStr, "Example Domain", "expected result to contain 'Example Domain'")

	t.Log("playwright execute API test passed")

	// Reuse the same container/warm daemon connection to verify tab-binding
	// behavior: `page` must bind to the browser's actual foreground tab, not
	// just the most recently opened one (resolveActivePage in
	// playwright-daemon.ts, backed by the image's Chrome 150+ CDP `tabActive`
	// signal). Uses a `data:` URL for the second tab so the assertion doesn't
	// depend on a second outbound network request.
	t.Log("verifying page binds to the foreground tab")
	openSecondTabRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: `
			const second = await context.newPage();
			await second.goto('data:text/html,second-tab');
			return context.pages().length;
		`,
	})
	require.NoError(t, err, "open second tab request error: %v", err)
	require.Equal(t, http.StatusOK, openSecondTabRsp.StatusCode(), "unexpected status: %s body=%s", openSecondTabRsp.Status(), string(openSecondTabRsp.Body))
	require.NotNil(t, openSecondTabRsp.JSON200)
	require.True(t, openSecondTabRsp.JSON200.Success, "expected open-second-tab success=true")
	require.EqualValues(t, 2, openSecondTabRsp.JSON200.Result, "expected two open tabs")

	// A fresh execute call must bind `page` to the newest tab, which is also
	// the foreground one right after opening it.
	urlRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: `return page.url();`,
	})
	require.NoError(t, err, "url request error: %v", err)
	require.Equal(t, http.StatusOK, urlRsp.StatusCode(), "unexpected status: %s body=%s", urlRsp.Status(), string(urlRsp.Body))
	require.NotNil(t, urlRsp.JSON200)
	require.True(t, urlRsp.JSON200.Success, "expected url request success=true")
	require.Equal(t, "data:text/html,second-tab", urlRsp.JSON200.Result, "expected injected page to be the foreground tab")

	// Bringing the original tab back to the foreground must flip which page
	// gets injected. Select it by URL rather than context.pages()[0] -- relying
	// on index/creation order would reintroduce the same undocumented ordering
	// assumption this change removes. Tab-creation order alone can't
	// distinguish this case from the one above -- only the CDP `tabActive`
	// signal can.
	bringToFrontRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: `
			const first = context.pages().find(candidate =>
				candidate.url().includes('example.com')
			);
			if (!first) throw new Error('original page not found');
			await first.bringToFront();
			return first.url();
		`,
	})
	require.NoError(t, err, "bring-to-front request error: %v", err)
	require.Equal(t, http.StatusOK, bringToFrontRsp.StatusCode(), "unexpected status: %s body=%s", bringToFrontRsp.Status(), string(bringToFrontRsp.Body))
	require.NotNil(t, bringToFrontRsp.JSON200)
	require.True(t, bringToFrontRsp.JSON200.Success, "expected bring-to-front success=true")

	refocusedUrlRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: `return page.url();`,
	})
	require.NoError(t, err, "refocused url request error: %v", err)
	require.Equal(t, http.StatusOK, refocusedUrlRsp.StatusCode(), "unexpected status: %s body=%s", refocusedUrlRsp.Status(), string(refocusedUrlRsp.Body))
	require.NotNil(t, refocusedUrlRsp.JSON200)
	require.True(t, refocusedUrlRsp.JSON200.Success, "expected refocused url request success=true")
	require.Contains(t, refocusedUrlRsp.JSON200.Result, "example.com", "expected injected page to follow foreground focus, not tab-creation order")

	t.Log("playwright foreground-tab binding test passed")
}

func TestPlaywrightExecuteTimeoutReturnsPromptlyAndRecovers(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	setupReq := instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: `return await page.evaluate(() => document.body.dataset.timeoutMutation = "initial");`,
	}
	setupRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, setupReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, setupRsp.StatusCode())
	require.NotNil(t, setupRsp.JSON200)
	require.True(t, setupRsp.JSON200.Success)

	timeoutSec := 1
	timeoutReq := instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: `
await page.waitForTimeout(2500);
await page.evaluate(() => document.body.dataset.timeoutMutation = "late");
`,
		TimeoutSec: &timeoutSec,
	}

	t.Log("executing playwright code expected to exceed timeout")
	start := time.Now()
	timeoutRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, timeoutReq)
	elapsed := time.Since(start)

	require.NoError(t, err, "playwright timeout request should return an API response")
	require.Less(t, elapsed, 4*time.Second, "timeout response should arrive before the API socket read deadline")
	require.Equal(t, http.StatusOK, timeoutRsp.StatusCode(), "unexpected status for timed-out playwright execute: %s body=%s", timeoutRsp.Status(), string(timeoutRsp.Body))
	require.NotNil(t, timeoutRsp.JSON200, "expected JSON200 timeout response, got nil")
	require.False(t, timeoutRsp.JSON200.Success, "expected success=false for timed-out playwright execution")
	require.NotNil(t, timeoutRsp.JSON200.Error, "expected timeout error message")

	errorMsg := strings.ToLower(*timeoutRsp.JSON200.Error)
	require.True(t, strings.Contains(errorMsg, "timeout") || strings.Contains(errorMsg, "timed out"), "expected timeout error, got %q", *timeoutRsp.JSON200.Error)
	require.NotContains(t, errorMsg, "i/o timeout", "daemon should return a timeout response before the API socket read deadline")

	recoveryReq := instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: `return await page.evaluate(() => document.body.dataset.timeoutMutation);`,
	}

	t.Log("executing normal playwright code after timed-out request")
	recoveryRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, recoveryReq)
	require.NoError(t, err, "playwright recovery request error: %v", err)
	require.Equal(t, http.StatusOK, recoveryRsp.StatusCode(), "unexpected status for playwright recovery execute: %s body=%s", recoveryRsp.Status(), string(recoveryRsp.Body))
	require.NotNil(t, recoveryRsp.JSON200, "expected JSON200 recovery response, got nil")
	require.True(t, recoveryRsp.JSON200.Success, "expected recovery request success=true, got error: %s", func() string {
		if recoveryRsp.JSON200.Error != nil {
			return *recoveryRsp.JSON200.Error
		}
		return "nil"
	}())
	require.NotNil(t, recoveryRsp.JSON200.Result, "expected recovery result to be non-nil")
	require.Equal(t, "initial", recoveryRsp.JSON200.Result)

	// Wait past the abandoned script's delay. A response-only timeout lets that
	// script wake up and mutate the page after the caller has moved on.
	time.Sleep(2 * time.Second)
	lateRsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, recoveryReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, lateRsp.StatusCode())
	require.NotNil(t, lateRsp.JSON200)
	require.True(t, lateRsp.JSON200.Success)
	require.NotNil(t, lateRsp.JSON200.Result)
	require.Equal(t, "initial", lateRsp.JSON200.Result, "timed-out execution mutated the page after returning")

	t.Log("playwright timeout regression test passed")
}

// TestPlaywrightDaemonRecovery tests that the playwright daemon recovers after chromium is restarted.
// The daemon maintains a warm CDP connection, but when chromium restarts, that connection breaks.
// The daemon should detect the disconnection and reconnect on the next request.
func TestPlaywrightDaemonRecovery(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	executeUserAgent := func() error {
		code := `return await page.evaluate(() => navigator.userAgent);`
		req := instanceoapi.ExecutePlaywrightCodeJSONRequestBody{Code: code}

		rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, req)
		if err != nil {
			return fmt.Errorf("request error: %w", err)
		}
		if rsp.StatusCode() != http.StatusOK {
			return fmt.Errorf("unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
		}
		if rsp.JSON200 == nil {
			return fmt.Errorf("expected JSON200 response")
		}

		if !rsp.JSON200.Success {
			var errorMsg, stderr string
			if rsp.JSON200.Error != nil {
				errorMsg = *rsp.JSON200.Error
			}
			if rsp.JSON200.Stderr != nil {
				stderr = *rsp.JSON200.Stderr
			}
			return fmt.Errorf("execution failed. Error: %s, Stderr: %s", errorMsg, stderr)
		}

		if rsp.JSON200.Result == nil {
			return fmt.Errorf("expected result to be non-nil")
		}
		return nil
	}

	executeAndVerify := func(description string) {
		t.Logf("action: %s", description)
		require.NoError(t, executeUserAgent(), "%s", description)
		t.Logf("%s: success", description)
	}

	waitForExecution := func(description string, timeout time.Duration) {
		t.Logf("action: %s", description)
		deadline := time.Now().Add(timeout)
		var lastErr error
		for attempt := 1; ; attempt++ {
			if err := executeUserAgent(); err != nil {
				lastErr = err
			} else {
				t.Logf("%s: success after %d attempt(s)", description, attempt)
				return
			}

			if time.Now().After(deadline) {
				require.NoError(t, lastErr, "%s did not recover within %s", description, timeout)
				return
			}

			select {
			case <-ctx.Done():
				require.NoError(t, ctx.Err(), "%s context cancelled while waiting for recovery", description)
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	// Step 1: Execute playwright code to start the daemon and establish CDP connection
	executeAndVerify("initial execution (starts daemon)")

	// Step 2: Restart chromium via supervisorctl
	t.Log("restarting chromium via supervisorctl")
	{
		args := []string{"-c", "/etc/supervisor/supervisord.conf", "restart", "chromium"}
		req := instanceoapi.ProcessExecJSONRequestBody{
			Command: "supervisorctl",
			Args:    &args,
		}
		rsp, err := client.ProcessExecWithResponse(ctx, req)
		require.NoError(t, err, "supervisorctl restart request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "supervisorctl restart unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))

		if rsp.JSON200.StdoutB64 != nil {
			t.Logf("supervisorctl stdout_b64: %s", *rsp.JSON200.StdoutB64)
		}
		if rsp.JSON200.StderrB64 != nil {
			t.Logf("supervisorctl stderr_b64: %s", *rsp.JSON200.StderrB64)
		}
	}

	// Step 3: Wait for chromium and the playwright daemon to be ready again
	t.Log("waiting for chromium to be ready after restart")
	require.NoError(t, c.WaitDevTools(ctx), "DevTools not ready after chromium restart")

	// Step 4: Execute playwright code again - daemon should recover
	waitForExecution("execution after chromium restart (daemon should recover)", 30*time.Second)

	t.Log("playwright daemon recovery test passed")
}
