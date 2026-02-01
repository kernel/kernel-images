package e2e

import (
	"context"
	"encoding/base64"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// TestAgentBrowserCDPProxy tests that agent-browser can connect to Chrome via the CDP proxy on port 9222.
// This validates the /json and /json/list endpoints that the proxy exposes for target discovery,
// which is required for tools like agent-browser and Playwright's connectOverCDP.
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
	t.Log("Installing agent-browser...")
	timeoutSec := 120 // npm install can take a while
	installResult := execCommandWithTimeout(t, ctx, client, "npm", []string{"install", "-g", "agent-browser"}, &timeoutSec)
	require.Zero(t, installResult.exitCode, "failed to install agent-browser: %s", installResult.output)
	t.Log("agent-browser installed successfully")

	// First test the /json endpoints via curl to verify the proxy is working correctly
	// before we test agent-browser

	// Test that /json endpoint returns proper target list with rewritten URLs
	t.Run("json endpoint returns targets with rewritten URLs", func(t *testing.T) {
		t.Log("Testing /json endpoint via curl")

		result := execCommand(t, ctx, client, "curl", []string{"-s", "http://127.0.0.1:9222/json"})
		require.Zero(t, result.exitCode, "curl /json failed: %s", result.output)

		// The response should be a JSON array containing targets
		require.True(t, strings.HasPrefix(strings.TrimSpace(result.output), "["),
			"expected JSON array from /json, got: %s", result.output)

		// The URLs should point to the proxy (port 9222), not Chrome directly (port 9223)
		require.Contains(t, result.output, "9222",
			"expected target URLs to be rewritten to proxy port 9222, got: %s", result.output)
		require.NotContains(t, result.output, "9223",
			"target URLs should not contain Chrome port 9223, got: %s", result.output)
	})

	// Test that /json/list endpoint also works
	t.Run("json/list endpoint returns targets with rewritten URLs", func(t *testing.T) {
		t.Log("Testing /json/list endpoint via curl")

		result := execCommand(t, ctx, client, "curl", []string{"-s", "http://127.0.0.1:9222/json/list"})
		require.Zero(t, result.exitCode, "curl /json/list failed: %s", result.output)

		// The response should be a JSON array containing targets
		require.True(t, strings.HasPrefix(strings.TrimSpace(result.output), "["),
			"expected JSON array from /json/list, got: %s", result.output)

		// The URLs should point to the proxy (port 9222), not Chrome directly (port 9223)
		require.Contains(t, result.output, "9222",
			"expected target URLs to be rewritten to proxy port 9222, got: %s", result.output)
		require.NotContains(t, result.output, "9223",
			"target URLs should not contain Chrome port 9223, got: %s", result.output)
	})

	// Test that /json/version endpoint works (this was already there)
	t.Run("json/version endpoint works", func(t *testing.T) {
		t.Log("Testing /json/version endpoint via curl")

		result := execCommand(t, ctx, client, "curl", []string{"-s", "http://127.0.0.1:9222/json/version"})
		require.Zero(t, result.exitCode, "curl /json/version failed: %s", result.output)

		// The response should be a JSON object with browser info
		require.True(t, strings.HasPrefix(strings.TrimSpace(result.output), "{"),
			"expected JSON object from /json/version, got: %s", result.output)

		// Should contain webSocketDebuggerUrl pointing to proxy
		require.Contains(t, result.output, "webSocketDebuggerUrl",
			"expected webSocketDebuggerUrl in response, got: %s", result.output)
		require.Contains(t, result.output, "9222",
			"expected webSocketDebuggerUrl to point to proxy port 9222, got: %s", result.output)
	})

	// Now test agent-browser with different CDP connection variations
	// Each test connects to the browser, navigates, and gets the URL to verify connectivity

	testCases := []struct {
		name   string
		cdpArg string
	}{
		{
			name:   "port only (9222)",
			cdpArg: "9222",
		},
		{
			name:   "http URL",
			cdpArg: "http://127.0.0.1:9222",
		},
		{
			name:   "localhost:port",
			cdpArg: "localhost:9222",
		},
		{
			name:   "127.0.0.1:port",
			cdpArg: "127.0.0.1:9222",
		},
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run("agent-browser connect "+tc.name, func(t *testing.T) {
			t.Logf("Testing agent-browser with --cdp %s", tc.cdpArg)

			// Navigate to example.com - this is the key test that verifies:
			// 1. agent-browser can connect to the CDP proxy on port 9222
			// 2. The proxy's /json endpoint works for target discovery
			// 3. The WebSocket connection through the proxy works
			navResult := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", tc.cdpArg, "open", "https://example.com"})
			t.Logf("Navigate result: exit=%d, output=%s", navResult.exitCode, navResult.output)
			require.Zero(t, navResult.exitCode, "agent-browser --cdp %s open failed: %s", tc.cdpArg, navResult.output)

			// Get the current URL to verify we're connected and navigation worked
			urlResult := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", tc.cdpArg, "get", "url", "--json"})
			t.Logf("Get URL result: exit=%d, output=%s", urlResult.exitCode, urlResult.output)
			require.Zero(t, urlResult.exitCode, "agent-browser --cdp %s get url failed: %s", tc.cdpArg, urlResult.output)

			// Verify we got a valid response containing example.com
			require.Contains(t, urlResult.output, "example.com",
				"expected URL to contain example.com, got: %s", urlResult.output)
		})
	}

	// Test agent-browser snapshot command which uses /json for target discovery
	t.Run("agent-browser snapshot via proxy", func(t *testing.T) {
		t.Log("Testing agent-browser snapshot via CDP proxy")

		// Get snapshot - this exercises the /json endpoint to discover targets
		result := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", "9222", "snapshot", "-i", "--json"})
		t.Logf("Snapshot exit code: %d, Output length: %d", result.exitCode, len(result.output))

		require.Zero(t, result.exitCode, "agent-browser snapshot failed: %s", result.output)
		// Verify we got a valid snapshot response
		require.True(t, strings.Contains(result.output, "success") || strings.Contains(result.output, "snapshot") || strings.Contains(result.output, "data"),
			"expected valid snapshot response, got: %s", result.output)
	})

	// Test agent-browser get title to further verify connectivity
	t.Run("agent-browser get title via proxy", func(t *testing.T) {
		t.Log("Testing agent-browser get title via CDP proxy")

		result := execCommand(t, ctx, client, "agent-browser", []string{"--cdp", "9222", "get", "title", "--json"})
		t.Logf("Get title result: exit=%d, output=%s", result.exitCode, result.output)

		require.Zero(t, result.exitCode, "agent-browser get title failed: %s", result.output)
		// Should contain "Example Domain" from previous navigation
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
