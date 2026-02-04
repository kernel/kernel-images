package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
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

	// Helper to execute playwright code and verify success
	executeAndVerify := func(description string) {
		t.Logf("action: %s", description)

		code := `return await page.evaluate(() => navigator.userAgent);`
		req := instanceoapi.ExecutePlaywrightCodeJSONRequestBody{Code: code}

		rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, req)
		require.NoError(t, err, "%s: request error: %v", description, err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "%s: unexpected status: %s body=%s", description, rsp.Status(), string(rsp.Body))
		require.NotNil(t, rsp.JSON200, "%s: expected JSON200 response", description)

		if !rsp.JSON200.Success {
			var errorMsg, stderr string
			if rsp.JSON200.Error != nil {
				errorMsg = *rsp.JSON200.Error
			}
			if rsp.JSON200.Stderr != nil {
				stderr = *rsp.JSON200.Stderr
			}
			t.Fatalf("%s: execution failed. Error: %s, Stderr: %s", description, errorMsg, stderr)
		}

		require.NotNil(t, rsp.JSON200.Result, "%s: expected result to be non-nil", description)
		t.Logf("%s: success", description)
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

	// Step 3: Wait for chromium to be ready again
	t.Log("waiting for chromium to be ready after restart")
	time.Sleep(2 * time.Second)

	// Step 4: Execute playwright code again - daemon should recover
	executeAndVerify("execution after chromium restart (daemon should recover)")

	t.Log("playwright daemon recovery test passed")
}
