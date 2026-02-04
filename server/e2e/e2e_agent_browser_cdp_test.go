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

// TestAgentBrowserCDPProxy tests that agent-browser can connect to Chrome via the CDP proxy on port 9222.
// This is the primary use case for the /json endpoint - enabling tools like agent-browser to work
// naturally within the container using `agent-browser --cdp 9222` instead of having to use port 9223.
func TestAgentBrowserCDPProxy(t *testing.T) {
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
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	// Install agent-browser globally inside the container
	// agent-browser is a CLI tool that uses playwright-core's connectOverCDP under the hood
	t.Log("Installing agent-browser...")
	timeoutSec := 120 // npm install can take a while
	installResult := execCommandWithTimeout(t, ctx, client, "npm", []string{"install", "-g", "agent-browser"}, &timeoutSec)
	require.Zero(t, installResult.exitCode, "failed to install agent-browser: %s", installResult.output)
	t.Log("agent-browser installed successfully")

	// Test agent-browser with different CDP connection formats
	// All of these should work through the proxy on port 9222

	// Test 1: Using just the port number (most common usage)
	t.Run("agent-browser --cdp 9222 snapshot", func(t *testing.T) {
		t.Log("Testing agent-browser snapshot with --cdp 9222")

		// Get a snapshot of the current page - this exercises:
		// 1. CDP connection via connectOverCDP
		// 2. Fetching /json to discover targets
		// 3. WebSocket connection through the proxy
		result := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", "9222", "snapshot", "--json"})
		t.Logf("Snapshot result: exit=%d, output_length=%d", result.exitCode, len(result.output))

		require.Zero(t, result.exitCode, "agent-browser snapshot failed: %s", result.output)
		// The output should be valid JSON containing the snapshot
		require.True(t, strings.Contains(result.output, "{") || strings.Contains(result.output, "["),
			"expected JSON output from snapshot, got: %s", result.output)
	})

	// Test 2: Using http:// URL format
	t.Run("agent-browser --cdp http://127.0.0.1:9222 snapshot", func(t *testing.T) {
		t.Log("Testing agent-browser snapshot with --cdp http://127.0.0.1:9222")

		result := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", "http://127.0.0.1:9222", "snapshot", "--json"})
		t.Logf("Snapshot result: exit=%d, output_length=%d", result.exitCode, len(result.output))

		require.Zero(t, result.exitCode, "agent-browser snapshot with http URL failed: %s", result.output)
	})

	// Test 3: Navigate to a URL and verify it works
	t.Run("agent-browser --cdp 9222 navigate and get url", func(t *testing.T) {
		t.Log("Testing agent-browser navigation via CDP proxy")

		// Navigate to example.com
		navResult := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", "9222", "open", "https://example.com"})
		t.Logf("Navigate result: exit=%d, output=%s", navResult.exitCode, navResult.output)
		require.Zero(t, navResult.exitCode, "agent-browser open failed: %s", navResult.output)

		// Get the current URL to verify navigation worked
		urlResult := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", "9222", "get", "url", "--json"})
		t.Logf("Get URL result: exit=%d, output=%s", urlResult.exitCode, urlResult.output)
		require.Zero(t, urlResult.exitCode, "agent-browser get url failed: %s", urlResult.output)

		// The URL should contain example.com
		require.Contains(t, urlResult.output, "example.com",
			"expected URL to contain example.com, got: %s", urlResult.output)
	})

	// Test 4: Get page title to verify page loaded correctly
	t.Run("agent-browser --cdp 9222 get title", func(t *testing.T) {
		t.Log("Testing agent-browser get title via CDP proxy")

		result := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", "9222", "get", "title", "--json"})
		t.Logf("Get title result: exit=%d, output=%s", result.exitCode, result.output)

		require.Zero(t, result.exitCode, "agent-browser get title failed: %s", result.output)
		// Should contain "Example" from example.com's title "Example Domain"
		require.Contains(t, result.output, "Example",
			"expected title to contain 'Example', got: %s", result.output)
	})

	t.Log("All agent-browser CDP proxy tests passed")
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
