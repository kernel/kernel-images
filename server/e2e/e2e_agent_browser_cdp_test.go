package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// TestCDPProxyJSONEndpoints tests that the CDP proxy's /json and /json/list endpoints
// correctly return target information with URLs rewritten to point to the proxy (port 9222)
// instead of Chrome directly (port 9223). This is required for tools like agent-browser
// and Playwright's connectOverCDP to work through the proxy.
func TestCDPProxyJSONEndpoints(t *testing.T) {
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
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	// Test that /json endpoint returns proper target list with webSocketDebuggerUrl rewritten
	t.Run("json endpoint returns targets with rewritten webSocketDebuggerUrl", func(t *testing.T) {
		t.Log("Testing /json endpoint via curl")

		result := execCommand(t, ctx, client, "curl", []string{"-s", "http://127.0.0.1:9222/json"})
		require.Zero(t, result.exitCode, "curl /json failed: %s", result.output)

		// The response should be a JSON array containing targets
		require.True(t, strings.HasPrefix(strings.TrimSpace(result.output), "["),
			"expected JSON array from /json, got: %s", result.output)

		// Parse the response and verify webSocketDebuggerUrl is rewritten
		var targets []map[string]interface{}
		err := json.Unmarshal([]byte(result.output), &targets)
		require.NoError(t, err, "failed to parse /json response: %s", result.output)
		require.NotEmpty(t, targets, "expected at least one target")

		// Check that webSocketDebuggerUrl points to port 9222 (proxy), not 9223 (Chrome)
		for i, target := range targets {
			wsURL, ok := target["webSocketDebuggerUrl"].(string)
			if ok && wsURL != "" {
				require.Contains(t, wsURL, "9222",
					"target %d: webSocketDebuggerUrl should contain proxy port 9222, got: %s", i, wsURL)
				require.NotContains(t, wsURL, "9223",
					"target %d: webSocketDebuggerUrl should not contain Chrome port 9223, got: %s", i, wsURL)
			}
		}
		t.Logf("Verified %d targets have correctly rewritten webSocketDebuggerUrl", len(targets))
	})

	// Test that /json/list endpoint also works
	t.Run("json/list endpoint returns targets with rewritten webSocketDebuggerUrl", func(t *testing.T) {
		t.Log("Testing /json/list endpoint via curl")

		result := execCommand(t, ctx, client, "curl", []string{"-s", "http://127.0.0.1:9222/json/list"})
		require.Zero(t, result.exitCode, "curl /json/list failed: %s", result.output)

		// The response should be a JSON array containing targets
		require.True(t, strings.HasPrefix(strings.TrimSpace(result.output), "["),
			"expected JSON array from /json/list, got: %s", result.output)

		// Parse and verify webSocketDebuggerUrl
		var targets []map[string]interface{}
		err := json.Unmarshal([]byte(result.output), &targets)
		require.NoError(t, err, "failed to parse /json/list response")
		require.NotEmpty(t, targets, "expected at least one target")

		for i, target := range targets {
			wsURL, ok := target["webSocketDebuggerUrl"].(string)
			if ok && wsURL != "" {
				require.Contains(t, wsURL, "9222",
					"target %d: webSocketDebuggerUrl should contain proxy port 9222", i)
				require.NotContains(t, wsURL, "9223",
					"target %d: webSocketDebuggerUrl should not contain Chrome port 9223", i)
			}
		}
	})

	// Test that /json/version endpoint works (this was already there)
	t.Run("json/version endpoint works", func(t *testing.T) {
		t.Log("Testing /json/version endpoint via curl")

		result := execCommand(t, ctx, client, "curl", []string{"-s", "http://127.0.0.1:9222/json/version"})
		require.Zero(t, result.exitCode, "curl /json/version failed: %s", result.output)

		// The response should be a JSON object with browser info
		require.True(t, strings.HasPrefix(strings.TrimSpace(result.output), "{"),
			"expected JSON object from /json/version, got: %s", result.output)

		// Parse and verify webSocketDebuggerUrl
		var version map[string]interface{}
		err := json.Unmarshal([]byte(result.output), &version)
		require.NoError(t, err, "failed to parse /json/version response")

		wsURL, ok := version["webSocketDebuggerUrl"].(string)
		require.True(t, ok, "expected webSocketDebuggerUrl in response")
		require.Contains(t, wsURL, "9222",
			"webSocketDebuggerUrl should point to proxy port 9222, got: %s", wsURL)
	})

	// Test that Chrome's /json endpoint on 9223 returns unrewritten URLs (for comparison)
	t.Run("chrome direct json has port 9223", func(t *testing.T) {
		t.Log("Testing Chrome's /json endpoint directly on port 9223")

		result := execCommand(t, ctx, client, "curl", []string{"-s", "http://127.0.0.1:9223/json"})
		require.Zero(t, result.exitCode, "curl /json on 9223 failed: %s", result.output)

		var targets []map[string]interface{}
		err := json.Unmarshal([]byte(result.output), &targets)
		require.NoError(t, err, "failed to parse Chrome's /json response")
		require.NotEmpty(t, targets, "expected at least one target")

		// Chrome's direct response should have port 9223
		wsURL, ok := targets[0]["webSocketDebuggerUrl"].(string)
		require.True(t, ok && wsURL != "", "expected webSocketDebuggerUrl in first target")
		require.Contains(t, wsURL, "9223",
			"Chrome's webSocketDebuggerUrl should contain port 9223, got: %s", wsURL)
	})

	t.Log("All CDP proxy JSON endpoint tests passed")
}

// execResult holds the result of a command execution
type execResult struct {
	exitCode int
	output   string
}

// execCommand runs a command via the container's process exec API and returns the result
func execCommand(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, command string, args []string) execResult {
	t.Helper()
	return execCommandWithTimeout(t, ctx, client, command, args, nil)
}

// execCommandWithTimeout runs a command with an optional timeout
func execCommandWithTimeout(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, command string, args []string, timeoutSec *int) execResult {
	t.Helper()

	req := instanceoapi.ProcessExecJSONRequestBody{
		Command:    command,
		Args:       &args,
		TimeoutSec: timeoutSec,
	}

	rsp, err := client.ProcessExecWithResponse(ctx, req)
	require.NoError(t, err, "process exec request error for %s", command)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for %s: %s body=%s",
		command, rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200, "expected JSON200 response for %s", command)

	var stdout, stderr string
	if rsp.JSON200.StdoutB64 != nil && *rsp.JSON200.StdoutB64 != "" {
		if b, decErr := base64.StdEncoding.DecodeString(*rsp.JSON200.StdoutB64); decErr == nil {
			stdout = string(b)
		}
	}
	if rsp.JSON200.StderrB64 != nil && *rsp.JSON200.StderrB64 != "" {
		if b, decErr := base64.StdEncoding.DecodeString(*rsp.JSON200.StderrB64); decErr == nil {
			stderr = string(b)
		}
	}

	exitCode := 0
	if rsp.JSON200.ExitCode != nil {
		exitCode = *rsp.JSON200.ExitCode
	}

	return execResult{
		exitCode: exitCode,
		output:   stdout + stderr,
	}
}
