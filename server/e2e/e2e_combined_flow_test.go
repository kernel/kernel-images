package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// TestExtensionViewportThenCDPConnection tests that CDP connections work correctly
// after back-to-back Chromium restarts triggered by extension upload and viewport change.
//
// This reproduces the race condition where profile loading fails to connect to CDP
// after the sequence: extension upload (restart) -> viewport change (restart) -> CDP connect.
func TestExtensionViewportThenCDPConnection(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Start with specific resolution to verify viewport change works
	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	c := NewTestContainer(t, headlessImage)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	err := c.Start(ctx, ContainerConfig{Env: env})
	require.NoError(t, err, "failed to start container: %v", err)
	defer c.Stop(ctx)

	t.Logf("[setup] waiting for API: %s/spec.yaml", c.APIBaseURL())
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	// Wait for DevTools to be ready initially
	err = c.WaitDevTools(ctx)
	require.NoError(t, err, "devtools not ready initially")

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Step 1: Upload extension (triggers Chromium restart)
	t.Log("[test] step 1: uploading extension")
	uploadExtension(t, ctx, client)

	// Wait briefly for the system to stabilize after extension upload restart
	// The extension upload waits for DevTools, but the API may need a moment
	t.Log("[test] verifying API is still responsive after extension upload")
	err = waitForAPIHealth(ctx, c.APIBaseURL(), t)
	require.NoError(t, err, "API not healthy after extension upload")

	// Create a fresh API client to avoid connection reuse issues after restart
	// The previous client's connection may have been closed by the server
	client, err = c.APIClientNoKeepAlive()
	require.NoError(t, err, "failed to create fresh API client")

	// Step 2: Change viewport (triggers another Chromium restart)
	t.Log("[test] step 2: changing viewport to 1920x1080")
	changeViewport(t, ctx, client, 1920, 1080)

	// Wait for API to be healthy after viewport change
	t.Log("[test] verifying API is still responsive after viewport change")
	err = waitForAPIHealth(ctx, c.APIBaseURL(), t)
	require.NoError(t, err, "API not healthy after viewport change")

	// Step 3: Immediately attempt CDP connection (this may fail due to race condition)
	t.Log("[test] step 3: attempting CDP connection immediately after restarts")

	// Try connecting without any delay - this is the most aggressive test case
	err = attemptCDPConnection(ctx, c.CDPURL(), t)
	if err != nil {
		t.Logf("[test] step 3: CDP connection failed: %v", err)
		// Log additional diagnostics
		logCDPDiagnostics(ctx, t)
	}
	require.NoError(t, err, "CDP connection failed after extension upload + viewport change")

	t.Log("[test] result: CDP connection successful after back-to-back restarts")
}

// TestMultipleCDPConnectionsAfterRestart tests that multiple rapid CDP connections
// work correctly after Chromium restart.
func TestMultipleCDPConnectionsAfterRestart(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	env := map[string]string{}

	c := NewTestContainer(t, headlessImage)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	err := c.Start(ctx, ContainerConfig{Env: env})
	require.NoError(t, err, "failed to start container: %v", err)
	defer c.Stop(ctx)

	t.Log("[setup] waiting for API")
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	err = c.WaitDevTools(ctx)
	require.NoError(t, err, "devtools not ready initially")

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Upload extension to trigger a restart
	t.Log("[test] uploading extension to trigger restart")
	uploadExtension(t, ctx, client)

	// Rapidly attempt multiple CDP connections in sequence
	t.Log("[test] attempting 5 rapid CDP connections")
	for i := 1; i <= 5; i++ {
		t.Logf("[test] connection attempt %d", i)
		err := attemptCDPConnection(ctx, c.CDPURL(), t)
		require.NoError(t, err, "CDP connection %d failed", i)
		t.Logf("[test] connection attempt %d: success", i)
	}

	t.Log("[test] result: all CDP connections successful")
}

// uploadExtension uploads a simple MV3 extension and waits for Chromium to restart.
func uploadExtension(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses) {
	t.Helper()

	// Build simple MV3 extension zip in-memory
	extDir := t.TempDir()
	manifest := `{
    "manifest_version": 3,
    "version": "1.0",
    "name": "Test Extension for Combined Flow",
    "description": "Minimal extension for testing CDP connections after restart"
}`
	err := os.WriteFile(filepath.Join(extDir, "manifest.json"), []byte(manifest), 0600)
	require.NoError(t, err, "write manifest")

	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "zip ext")

	// Upload extension
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("extensions.zip_file", "ext.zip")
	require.NoError(t, err)
	_, err = io.Copy(fw, bytes.NewReader(extZip))
	require.NoError(t, err)
	err = w.WriteField("extensions.name", "combined-flow-test-ext")
	require.NoError(t, err)
	err = w.Close()
	require.NoError(t, err)

	start := time.Now()
	rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
	elapsed := time.Since(start)
	require.NoError(t, err, "uploadExtensionsAndRestart request error")
	require.Equal(t, http.StatusCreated, rsp.StatusCode(), "unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
	t.Logf("[extension] uploaded in %s", elapsed)
}

// changeViewport changes the display resolution, which triggers Chromium restart.
func changeViewport(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, width, height int) {
	t.Helper()

	req := instanceoapi.PatchDisplayJSONRequestBody{
		Width:  &width,
		Height: &height,
	}
	start := time.Now()
	rsp, err := client.PatchDisplayWithResponse(ctx, req)
	elapsed := time.Since(start)
	require.NoError(t, err, "PATCH /display request failed")
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200, "expected JSON200 response")
	t.Logf("[viewport] changed to %dx%d in %s", width, height, elapsed)
}

// attemptCDPConnection tries to establish a CDP WebSocket connection and run a simple command.
func attemptCDPConnection(ctx context.Context, wsURL string, t *testing.T) error {
	t.Helper()

	// Set a timeout for the connection attempt
	connCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	t.Logf("[cdp] connecting to %s", wsURL)

	// Establish WebSocket connection to CDP proxy
	conn, _, err := websocket.Dial(connCtx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial CDP WebSocket: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	t.Logf("[cdp] connected to %s", wsURL)

	// Send a simple CDP command: Browser.getVersion
	// This validates that the proxy can communicate with the browser
	cdpRequest := map[string]any{
		"id":     1,
		"method": "Browser.getVersion",
	}
	reqBytes, err := json.Marshal(cdpRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal CDP request: %w", err)
	}

	t.Log("[cdp] sending Browser.getVersion")

	if err := conn.Write(connCtx, websocket.MessageText, reqBytes); err != nil {
		return fmt.Errorf("failed to send CDP command: %w", err)
	}

	// Read response
	_, respBytes, err := conn.Read(connCtx)
	if err != nil {
		return fmt.Errorf("failed to read CDP response: %w", err)
	}

	var cdpResponse map[string]any
	if err := json.Unmarshal(respBytes, &cdpResponse); err != nil {
		return fmt.Errorf("failed to unmarshal CDP response: %w", err)
	}

	// Check for error in response
	if errField, ok := cdpResponse["error"]; ok {
		return fmt.Errorf("CDP command returned error: %v", errField)
	}

	// Verify we got a result
	result, ok := cdpResponse["result"].(map[string]any)
	if !ok {
		return fmt.Errorf("CDP response missing result field: %v", cdpResponse)
	}

	// Log some version info for debugging
	if product, ok := result["product"].(string); ok {
		t.Logf("[cdp] version received: %s", product)
	}

	t.Log("[cdp] command successful")
	return nil
}

// waitForAPIHealth waits until the API server is responsive.
func waitForAPIHealth(ctx context.Context, apiBaseURL string, t *testing.T) error {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	maxAttempts := 30
	for i := 0; i < maxAttempts; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/spec.yaml", nil)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			t.Logf("[health] API healthy after %d attempts", i+1)
			return nil
		}
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if i < maxAttempts-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return fmt.Errorf("API not healthy after %d attempts", maxAttempts)
}

// logCDPDiagnostics logs diagnostic information when CDP connection fails.
func logCDPDiagnostics(ctx context.Context, t *testing.T) {
	t.Helper()

	// Try to get the internal CDP endpoint status
	stdout, err := execCombinedOutput(ctx, "curl", []string{"-s", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:9223/json/version"})
	if err != nil {
		t.Logf("[diagnostics] internal CDP status: failed (%v)", err)
	} else {
		t.Logf("[diagnostics] internal CDP status: %s", stdout)
	}

	// Check if Chromium process is running
	psOutput, err := execCombinedOutput(ctx, "pgrep", []string{"-a", "chromium"})
	if err != nil {
		t.Logf("[diagnostics] chromium process: not found or error (%v)", err)
	} else {
		t.Logf("[diagnostics] chromium process: %s", psOutput)
	}

	// Check supervisord status
	supervisorOutput, err := execCombinedOutput(ctx, "supervisorctl", []string{"-c", "/etc/supervisor/supervisord.conf", "status"})
	if err != nil {
		t.Logf("[diagnostics] supervisor status: error (%v)", err)
	} else {
		t.Logf("[diagnostics] supervisor status: %s", supervisorOutput)
	}
}
