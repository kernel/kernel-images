package e2e

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMV3ServiceWorkerRegistration tests that MV3 extensions with service workers
// are properly loaded and their service workers are active and responsive.
//
// This test verifies:
// 1. Extension can be uploaded and Chromium restarts successfully
// 2. Extension appears in chrome://extensions with an active service worker
// 3. Service worker responds to messages from the popup
func TestMV3ServiceWorkerRegistration(t *testing.T) {
	t.Parallel()
	ensurePlaywrightDeps(t)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	c := NewTestContainer(t, headlessImage)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	err := c.Start(ctx, ContainerConfig{})
	require.NoError(t, err, "failed to start container")
	defer c.Stop(ctx)

	t.Logf("[setup] waiting for API url=%s", c.APIBaseURL()+"/spec.yaml")
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	// Wait for DevTools to be ready
	err = c.WaitDevTools(ctx)
	require.NoError(t, err, "devtools not ready")

	// Upload the MV3 test extension
	t.Log("[test] uploading MV3 service worker test extension")
	uploadMV3TestExtension(t, ctx, c)

	// Run playwright script to verify service worker
	t.Log("[test] verifying MV3 service worker via playwright")
	verifyMV3ServiceWorker(t, ctx, c.CDPURL())

	t.Log("[test] MV3 service worker test passed")
}

// uploadMV3TestExtension uploads the test extension from test-extension directory.
func uploadMV3TestExtension(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Get the path to the test extension
	// The test extension is in server/e2e/test-extension
	extDir, err := filepath.Abs("test-extension")
	require.NoError(t, err, "failed to get absolute path to test-extension")

	// Create zip of the extension
	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "failed to zip test extension")

	// Upload extension
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("extensions.zip_file", "mv3-test-ext.zip")
	require.NoError(t, err)
	_, err = io.Copy(fw, bytes.NewReader(extZip))
	require.NoError(t, err)
	err = w.WriteField("extensions.name", "mv3-service-worker-test")
	require.NoError(t, err)
	err = w.Close()
	require.NoError(t, err)

	start := time.Now()
	rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
	elapsed := time.Since(start)
	require.NoError(t, err, "uploadExtensionsAndRestart request error")
	require.Equal(t, http.StatusCreated, rsp.StatusCode(), "unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
	t.Logf("[extension] uploaded elapsed=%s", elapsed.String())
}

// verifyMV3ServiceWorker runs the playwright script to verify the service worker.
func verifyMV3ServiceWorker(t *testing.T, ctx context.Context, cdpURL string) {
	t.Helper()

	cmd := exec.CommandContext(ctx, "pnpm", "exec", "tsx", "index.ts",
		"verify-mv3-service-worker",
		"--ws-url", cdpURL,
		"--timeout", "60000",
	)
	cmd.Dir = getPlaywrightPath()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("[playwright] error output: %s", string(out))
	}
	require.NoError(t, err, "MV3 service worker verification failed: %v\noutput=%s", err, string(out))
	t.Logf("[playwright] output: %s", string(out))
}
