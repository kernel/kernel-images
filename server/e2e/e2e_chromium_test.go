package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"log/slog"

	_ "github.com/glebarez/sqlite"
	logctx "github.com/onkernel/kernel-images/server/lib/logger"
	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

const (
	defaultHeadfulImage  = "onkernel/chromium-headful-test:latest"
	defaultHeadlessImage = "onkernel/chromium-headless-test:latest"
	containerName        = "server-e2e-test"
	// With host networking, the API listens on 10001 directly on the host
	apiBaseURL = "http://127.0.0.1:10001"
)

var (
	headfulImage  = defaultHeadfulImage
	headlessImage = defaultHeadlessImage
)

func init() {
	// Prefer fully-specified images if provided
	if v := os.Getenv("E2E_CHROMIUM_HEADFUL_IMAGE"); v != "" {
		headfulImage = v
	}
	if v := os.Getenv("E2E_CHROMIUM_HEADLESS_IMAGE"); v != "" {
		headlessImage = v
	}
	// Otherwise, if a tag/sha is provided, use the CI-built images
	tag := os.Getenv("E2E_IMAGE_TAG")
	if tag == "" {
		tag = os.Getenv("E2E_IMAGE_SHA")
	}
	if tag != "" {
		if os.Getenv("E2E_CHROMIUM_HEADFUL_IMAGE") == "" {
			headfulImage = "onkernel/chromium-headful:" + tag
		}
		if os.Getenv("E2E_CHROMIUM_HEADLESS_IMAGE") == "" {
			headlessImage = "onkernel/chromium-headless:" + tag
		}
	}
}

// getPlaywrightPath returns the path to the playwright script
func getPlaywrightPath() string {
	return "./playwright"
}

// ensurePlaywrightDeps ensures playwright dependencies are installed
func ensurePlaywrightDeps(t *testing.T) {
	t.Helper()
	nodeModulesPath := getPlaywrightPath() + "/node_modules"
	if _, err := os.Stat(nodeModulesPath); os.IsNotExist(err) {
		t.Log("Installing playwright dependencies...")
		cmd := exec.Command("pnpm", "install")
		cmd.Dir = getPlaywrightPath()
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "Failed to install playwright dependencies: %v\nOutput: %s", err, string(output))
		t.Log("Playwright dependencies installed successfully")
	}
}

func TestDisplayResolutionChange(t *testing.T) {
	image := headlessImage
	name := containerName + "-display"

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	baseCtx := logctx.AddToContext(context.Background(), logger)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Clean slate
	_ = stopContainer(baseCtx, name)

	// Start with default resolution
	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	// Start container
	_, exitCh, err := runContainer(baseCtx, image, name, env)
	require.NoError(t, err, "failed to start container: %v", err)
	defer stopContainer(baseCtx, name)

	ctx, cancel := context.WithTimeout(baseCtx, 3*time.Minute)
	defer cancel()

	logger.Info("[setup]", "action", "waiting for API", "url", apiBaseURL+"/spec.yaml")
	require.NoError(t, waitHTTPOrExitWithLogs(ctx, apiBaseURL+"/spec.yaml", exitCh, name), "api not ready: %v", err)

	client, err := apiClient()
	require.NoError(t, err, "failed to create API client: %v", err)

	// Get initial Xvfb resolution
	logger.Info("[test]", "action", "getting initial Xvfb resolution")
	initialWidth, initialHeight, err := getXvfbResolution(ctx)
	require.NoError(t, err, "failed to get initial Xvfb resolution: %v", err)
	logger.Info("[test]", "initial_resolution", fmt.Sprintf("%dx%d", initialWidth, initialHeight))
	require.Equal(t, 1024, initialWidth, "expected initial width 1024")
	require.Equal(t, 768, initialHeight, "expected initial height 768")

	// Test first resolution change: 1920x1080
	logger.Info("[test]", "action", "changing resolution to 1920x1080")
	width1 := 1920
	height1 := 1080
	req1 := instanceoapi.PatchDisplayJSONRequestBody{
		Width:  &width1,
		Height: &height1,
	}
	rsp1, err := client.PatchDisplayWithResponse(ctx, req1)
	require.NoError(t, err, "PATCH /display request failed: %v", err)
	require.Equal(t, http.StatusOK, rsp1.StatusCode(), "unexpected status: %s body=%s", rsp1.Status(), string(rsp1.Body))
	require.NotNil(t, rsp1.JSON200, "expected JSON200 response, got nil")
	require.NotNil(t, rsp1.JSON200.Width, "expected width in response")
	require.Equal(t, width1, *rsp1.JSON200.Width, "expected width %d in response", width1)
	require.NotNil(t, rsp1.JSON200.Height, "expected height in response")
	require.Equal(t, height1, *rsp1.JSON200.Height, "expected height %d in response", height1)

	// Wait a bit for Xvfb to fully restart
	logger.Info("[test]", "action", "waiting for Xvfb to stabilize")
	time.Sleep(3 * time.Second)

	// Verify new resolution via ps aux
	logger.Info("[test]", "action", "verifying new Xvfb resolution")
	newWidth1, newHeight1, err := getXvfbResolution(ctx)
	require.NoError(t, err, "failed to get new Xvfb resolution: %v", err)
	logger.Info("[test]", "new_resolution", fmt.Sprintf("%dx%d", newWidth1, newHeight1))
	require.Equal(t, width1, newWidth1, "expected Xvfb resolution %dx%d, got %dx%d", width1, height1, newWidth1, newHeight1)
	require.Equal(t, height1, newHeight1, "expected Xvfb resolution %dx%d, got %dx%d", width1, height1, newWidth1, newHeight1)

	// Test second resolution change: 1280x720
	logger.Info("[test]", "action", "changing resolution to 1280x720")
	width2 := 1280
	height2 := 720
	req2 := instanceoapi.PatchDisplayJSONRequestBody{
		Width:  &width2,
		Height: &height2,
	}
	rsp2, err := client.PatchDisplayWithResponse(ctx, req2)
	require.NoError(t, err, "PATCH /display request failed: %v", err)
	require.Equal(t, http.StatusOK, rsp2.StatusCode(), "unexpected status: %s body=%s", rsp2.Status(), string(rsp2.Body))
	require.NotNil(t, rsp2.JSON200, "expected JSON200 response, got nil")
	require.NotNil(t, rsp2.JSON200.Width, "expected width in response")
	require.Equal(t, width2, *rsp2.JSON200.Width, "expected width %d in response", width2)
	require.NotNil(t, rsp2.JSON200.Height, "expected height in response")
	require.Equal(t, height2, *rsp2.JSON200.Height, "expected height %d in response", height2)

	// Wait a bit for Xvfb to fully restart
	logger.Info("[test]", "action", "waiting for Xvfb to stabilize")
	time.Sleep(3 * time.Second)

	// Verify second resolution change via ps aux
	logger.Info("[test]", "action", "verifying second Xvfb resolution")
	newWidth2, newHeight2, err := getXvfbResolution(ctx)
	require.NoError(t, err, "failed to get second Xvfb resolution: %v", err)
	logger.Info("[test]", "final_resolution", fmt.Sprintf("%dx%d", newWidth2, newHeight2))
	require.Equal(t, width2, newWidth2, "expected Xvfb resolution %dx%d, got %dx%d", width2, height2, newWidth2, newHeight2)
	require.Equal(t, height2, newHeight2, "expected Xvfb resolution %dx%d, got %dx%d", width2, height2, newWidth2, newHeight2)

	logger.Info("[test]", "result", "all resolution changes verified successfully")
}

func TestExtensionUploadAndActivation(t *testing.T) {
	ensurePlaywrightDeps(t)
	image := headlessImage
	name := containerName + "-ext"

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	baseCtx := logctx.AddToContext(context.Background(), logger)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Clean slate
	_ = stopContainer(baseCtx, name)

	env := map[string]string{}
	// headless uses stealth defaults; no need to set CHROMIUM_FLAGS here

	// Start container
	_, exitCh, err := runContainer(baseCtx, image, name, env)
	require.NoError(t, err, "failed to start container: %v", err)
	defer stopContainer(baseCtx, name)

	ctx, cancel := context.WithTimeout(baseCtx, 3*time.Minute)
	defer cancel()

	require.NoError(t, waitHTTPOrExitWithLogs(ctx, apiBaseURL+"/spec.yaml", exitCh, name), "api not ready: %v", err)

	// Wait for DevTools
	_, err = waitDevtoolsWS(ctx)
	require.NoError(t, err, "devtools not ready: %v", err)

	// Build simple MV3 extension zip in-memory
	extDir := t.TempDir()
	manifest := `{
    "manifest_version": 3,
    "version": "1.0",
    "name": "My Test Extension",
    "description": "Test of a simple browser extension",
    "content_scripts": [
        {
            "matches": [
                "https://www.sfmoma.org/*"
            ],
            "js": [
                "content-script.js"
            ]
        }
    ]
}`
	contentScript := "document.title += \" -- Title updated by browser extension\";\n"
	err = os.WriteFile(filepath.Join(extDir, "manifest.json"), []byte(manifest), 0600)
	require.NoError(t, err, "write manifest: %v", err)
	err = os.WriteFile(filepath.Join(extDir, "content-script.js"), []byte(contentScript), 0600)
	require.NoError(t, err, "write content-script: %v", err)

	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "zip ext: %v", err)

	// Use new API to upload extension and restart Chromium
	{
		client, err := apiClient()
		require.NoError(t, err)
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		fw, err := w.CreateFormFile("extensions.zip_file", "ext.zip")
		require.NoError(t, err)
		_, err = io.Copy(fw, bytes.NewReader(extZip))
		require.NoError(t, err)
		err = w.WriteField("extensions.name", "testext")
		require.NoError(t, err)
		err = w.Close()
		require.NoError(t, err)
		start := time.Now()
		rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
		elapsed := time.Since(start)
		require.NoError(t, err, "uploadExtensionsAndRestart request error: %v", err)
		require.Equal(t, http.StatusCreated, rsp.StatusCode(), "unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
		t.Logf("/chromium/upload-extensions-and-restart completed in %s (%d ms)", elapsed.String(), elapsed.Milliseconds())
	}

	// Verify the content script updated the title on an allowed URL
	{
		cmd := exec.CommandContext(ctx, "pnpm", "exec", "tsx", "index.ts",
			"verify-title-contains",
			"--url", "https://www.sfmoma.org/",
			"--substr", "Title updated by browser extension",
			"--ws-url", "ws://127.0.0.1:9222/",
			"--timeout", "45000",
		)
		cmd.Dir = getPlaywrightPath()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "title verify failed: %v output=%s", err, string(out))
	}
}

func TestScreenshotHeadless(t *testing.T) {
	image := headlessImage
	name := containerName + "-screenshot-headless"

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	baseCtx := logctx.AddToContext(context.Background(), logger)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Clean slate
	_ = stopContainer(baseCtx, name)

	env := map[string]string{}

	// Start container
	_, exitCh, err := runContainer(baseCtx, image, name, env)
	require.NoError(t, err, "failed to start container: %v", err)
	defer stopContainer(baseCtx, name)

	ctx, cancel := context.WithTimeout(baseCtx, 2*time.Minute)
	defer cancel()

	require.NoError(t, waitHTTPOrExitWithLogs(ctx, apiBaseURL+"/spec.yaml", exitCh, name), "api not ready: %v", err)

	client, err := apiClient()
	require.NoError(t, err)

	// Whole-screen screenshot
	{
		rsp, err := client.TakeScreenshotWithResponse(ctx, instanceoapi.TakeScreenshotJSONRequestBody{})
		require.NoError(t, err, "screenshot request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for full screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "response is not PNG (len=%d)", len(rsp.Body))
	}

	// Region screenshot (safe small region)
	{
		region := instanceoapi.ScreenshotRegion{X: 0, Y: 0, Width: 50, Height: 50}
		req := instanceoapi.TakeScreenshotJSONRequestBody{Region: &region}
		rsp, err := client.TakeScreenshotWithResponse(ctx, req)
		require.NoError(t, err, "region screenshot request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for region screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "region response is not PNG (len=%d)", len(rsp.Body))
	}
}

func TestScreenshotHeadful(t *testing.T) {
	image := headfulImage
	name := containerName + "-screenshot-headful"

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	baseCtx := logctx.AddToContext(context.Background(), logger)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Clean slate
	_ = stopContainer(baseCtx, name)

	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	// Start container
	_, exitCh, err := runContainer(baseCtx, image, name, env)
	require.NoError(t, err, "failed to start container: %v", err)
	defer stopContainer(baseCtx, name)

	ctx, cancel := context.WithTimeout(baseCtx, 2*time.Minute)
	defer cancel()

	require.NoError(t, waitHTTPOrExitWithLogs(ctx, apiBaseURL+"/spec.yaml", exitCh, name), "api not ready: %v", err)

	client, err := apiClient()
	require.NoError(t, err)

	// Whole-screen screenshot
	{
		rsp, err := client.TakeScreenshotWithResponse(ctx, instanceoapi.TakeScreenshotJSONRequestBody{})
		require.NoError(t, err, "screenshot request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for full screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "response is not PNG (len=%d)", len(rsp.Body))
	}

	// Region screenshot
	{
		region := instanceoapi.ScreenshotRegion{X: 0, Y: 0, Width: 80, Height: 60}
		req := instanceoapi.TakeScreenshotJSONRequestBody{Region: &region}
		rsp, err := client.TakeScreenshotWithResponse(ctx, req)
		require.NoError(t, err, "region screenshot request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for region screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "region response is not PNG (len=%d)", len(rsp.Body))
	}
}

func TestInputEndpointsSmoke(t *testing.T) {
	image := headlessImage
	name := containerName + "-input-smoke"

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	baseCtx := logctx.AddToContext(context.Background(), logger)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	_ = stopContainer(baseCtx, name)

	width, height := 1024, 768
	_, exitCh, err := runContainer(baseCtx, image, name, map[string]string{"WIDTH": strconv.Itoa(width), "HEIGHT": strconv.Itoa(height)})
	require.NoError(t, err, "failed to start container: %v", err)
	defer stopContainer(baseCtx, name)

	ctx, cancel := context.WithTimeout(baseCtx, 2*time.Minute)
	defer cancel()

	require.NoError(t, waitHTTPOrExitWithLogs(ctx, apiBaseURL+"/spec.yaml", exitCh, name), "api not ready: %v", err)

	client, err := apiClient()
	require.NoError(t, err)

	// press_key: tap Return
	{
		rsp, err := client.PressKeyWithResponse(ctx, instanceoapi.PressKeyJSONRequestBody{Keys: []string{"Return"}})
		require.NoError(t, err, "press_key request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for press_key: %s body=%s", rsp.Status(), string(rsp.Body))
	}

	// scroll: small vertical and horizontal ticks at center
	cx, cy := width/2, height/2
	{
		rsp, err := client.ScrollWithResponse(ctx, instanceoapi.ScrollJSONRequestBody{X: cx, Y: cy, DeltaX: lo.ToPtr(2), DeltaY: lo.ToPtr(-3)})
		require.NoError(t, err, "scroll request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for scroll: %s body=%s", rsp.Status(), string(rsp.Body))
	}
	// drag_mouse: simple short drag path
	{
		rsp, err := client.DragMouseWithResponse(ctx, instanceoapi.DragMouseJSONRequestBody{
			Path: [][]int{{cx - 10, cy - 10}, {cx + 10, cy + 10}},
		})
		require.NoError(t, err, "drag_mouse request error: %v", err)
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for drag_mouse: %s body=%s", rsp.Status(), string(rsp.Body))
	}
}

// isPNG returns true if data starts with the PNG magic header
func isPNG(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	sig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	for i := 0; i < 8; i++ {
		if data[i] != sig[i] {
			return false
		}
	}
	return true
}

// ContainerOptions configures container startup behavior
type ContainerOptions struct {
	// HostAccess adds --add-host=host.docker.internal:host-gateway for tests
	// that need to reach services on the host machine
	HostAccess bool
}

func runContainer(ctx context.Context, image, name string, env map[string]string) (*exec.Cmd, <-chan error, error) {
	return runContainerWithOptions(ctx, image, name, env, ContainerOptions{})
}

func runContainerWithOptions(ctx context.Context, image, name string, env map[string]string, opts ContainerOptions) (*exec.Cmd, <-chan error, error) {
	logger := logctx.FromContext(ctx)
	args := []string{
		"run",
		"--name", name,
		"--privileged",
		"-p", "10001:10001", // API server
		"-p", "9222:9222",   // DevTools proxy
		"--tmpfs", "/dev/shm:size=2g,mode=1777",
	}

	if opts.HostAccess {
		args = append(args, "--add-host=host.docker.internal:host-gateway")
	}

	// Ensure CHROMIUM_FLAGS includes --no-sandbox for CI environments where
	// unprivileged user namespaces may be disabled (e.g., Ubuntu 24.04 GitHub Actions)
	// Create a copy to avoid mutating the caller's map
	envCopy := make(map[string]string)
	for k, v := range env {
		envCopy[k] = v
	}
	if _, ok := envCopy["CHROMIUM_FLAGS"]; !ok {
		envCopy["CHROMIUM_FLAGS"] = "--no-sandbox"
	} else if !strings.Contains(envCopy["CHROMIUM_FLAGS"], "--no-sandbox") {
		envCopy["CHROMIUM_FLAGS"] = envCopy["CHROMIUM_FLAGS"] + " --no-sandbox"
	}

	for k, v := range envCopy {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, image)

	logger.Info("[docker]", "action", "run", "args", strings.Join(args, " "))
	cmd := exec.Command("docker", args...)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
	}()

	return cmd, exitCh, nil
}

func stopContainer(ctx context.Context, name string) error {
	_ = exec.CommandContext(ctx, "docker", "kill", name).Run()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()

	// Wait loop to ensure the container is actually gone
	const maxWait = 10 * time.Second
	const pollInterval = 200 * time.Millisecond
	deadline := time.Now().Add(maxWait)
	var lastCheckErr error
	for {
		cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", fmt.Sprintf("name=%s", name), "--format", "{{.Names}}")
		out, err := cmd.Output()
		if err != nil {
			// If docker itself fails, break out (maybe docker is gone)
			lastCheckErr = err
			break
		}
		names := strings.Fields(string(out))
		found := false
		for _, n := range names {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			break // container is gone
		}
		if time.Now().After(deadline) {
			lastCheckErr = fmt.Errorf("timeout waiting for container %s to be removed", name)
			break // give up after maxWait
		}
		time.Sleep(pollInterval)
	}

	if lastCheckErr != nil {
		return lastCheckErr
	}
	return nil
}

// getContainerLogs retrieves the last N lines of container logs for debugging.
// Uses a fresh context with its own timeout to avoid issues when the parent context is cancelled.
func getContainerLogs(_ context.Context, name string, tailLines int) string {
	// Use a fresh context with generous timeout - the parent context may be cancelled
	logCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(logCtx, "docker", "logs", "--tail", fmt.Sprintf("%d", tailLines), name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("failed to get container logs: %v", err)
	}
	return string(output)
}

// waitHTTPOrExitWithLogs waits for HTTP endpoint and captures container logs on failure.
// It also periodically logs container status during the wait for better visibility.
func waitHTTPOrExitWithLogs(ctx context.Context, url string, exitCh <-chan error, containerName string) error {
	logger := logctx.FromContext(ctx)

	// Start a background goroutine to periodically show container status
	// Use a separate stopCh that we close to signal the goroutine to stop,
	// avoiding the race condition of sending to a potentially closed channel
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				// Check if container is still running
				checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				cmd := exec.CommandContext(checkCtx, "docker", "inspect", "--format", "{{.State.Status}} (pid={{.State.Pid}}, started={{.State.StartedAt}})", containerName)
				out, err := cmd.Output()
				cancel()
				if err == nil {
					logger.Info("[container-status]", "container", containerName, "status", strings.TrimSpace(string(out)))
				}
				// Also show last few log lines
				recentLogs := getContainerLogs(ctx, containerName, 10)
				if recentLogs != "" && !strings.Contains(recentLogs, "failed to get") {
					logger.Info("[container-logs]", "recent_output", strings.TrimSpace(recentLogs))
				}
			}
		}
	}()

	err := waitHTTPOrExit(ctx, url, exitCh)

	// Signal the status goroutine to stop and wait for it to finish
	close(stopCh)
	<-doneCh

	if err != nil {
		// Capture container logs for debugging
		logs := getContainerLogs(ctx, containerName, 100)
		return fmt.Errorf("%w\n\nContainer logs (last 100 lines):\n%s", err, logs)
	}
	return nil
}

func waitHTTPOrExit(ctx context.Context, url string, exitCh <-chan error) error {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 500 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}
		if resp != nil && resp.Body != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exitCh:
			if err != nil {
				return fmt.Errorf("container exited while waiting for %s: %w", url, err)
			}
			return fmt.Errorf("container exited while waiting for %s", url)
		case <-ticker.C:
		}
	}
}

func waitTCP(ctx context.Context, hostport string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := d.DialContext(ctx, "tcp", hostport)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitDevtoolsWS(ctx context.Context) (string, error) {
	if err := waitTCP(ctx, "127.0.0.1:9222"); err != nil {
		return "", err
	}
	return "ws://127.0.0.1:9222/", nil
}

func apiClient() (*instanceoapi.ClientWithResponses, error) {
	return instanceoapi.NewClientWithResponses(apiBaseURL, instanceoapi.WithHTTPClient(http.DefaultClient))
}

// RemoteExecError represents a non-zero exit from a remote exec, exposing exit code and combined output
type RemoteExecError struct {
	Command  string
	Args     []string
	ExitCode int
	Output   string
}

func (e *RemoteExecError) Error() string {
	return fmt.Sprintf("remote exec %s exited with code %d", e.Command, e.ExitCode)
}

// execCombinedOutput runs a command via the remote API and returns combined stdout+stderr and an error if exit code != 0
func execCombinedOutput(ctx context.Context, command string, args []string) (string, error) {
	client, err := apiClient()
	if err != nil {
		return "", err
	}

	req := instanceoapi.ProcessExecJSONRequestBody{
		Command: command,
		Args:    &args,
	}

	rsp, err := client.ProcessExecWithResponse(ctx, req)
	if err != nil {
		return "", err
	}
	if rsp.JSON200 == nil {
		return "", fmt.Errorf("remote exec failed: %s body=%s", rsp.Status(), string(rsp.Body))
	}

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
	combined := stdout + stderr

	exitCode := 0
	if rsp.JSON200.ExitCode != nil {
		exitCode = *rsp.JSON200.ExitCode
	}
	if exitCode != 0 {
		return combined, &RemoteExecError{Command: command, Args: args, ExitCode: exitCode, Output: combined}
	}
	return combined, nil
}

// zipDirToBytes zips the contents of dir (no extra top-level folder) to bytes
func zipDirToBytes(dir string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	defer zw.Close()

	// Walk dir
	root := filepath.Clean(dir)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			_, err := zw.Create(rel + "/")
			return err
		}
		fh, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		fh.Name = rel
		fh.Method = zip.Deflate
		w, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, f)
		f.Close()
		return copyErr
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// getXvfbResolution extracts the Xvfb resolution from the ps aux output
// It looks for the Xvfb command line which contains "-screen 0 WIDTHxHEIGHTx24"
func getXvfbResolution(ctx context.Context) (width, height int, err error) {
	logger := logctx.FromContext(ctx)

	// Get ps aux output
	stdout, err := execCombinedOutput(ctx, "ps", []string{"aux"})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to execute ps aux: %w, output: %s", err, stdout)
	}

	logger.Info("[xvfb-resolution]", "action", "parsing ps aux output")

	// Look for Xvfb line
	// Expected format: "root ... Xvfb :1 -screen 0 1920x1080x24 ..."
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if !strings.Contains(line, "Xvfb") {
			continue
		}
		logger.Info("[xvfb-resolution]", "line", line)

		// Parse the screen parameter
		// Look for pattern: "-screen 0 WIDTHxHEIGHTx24"
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "-screen" && i+2 < len(fields) {
				// Next field should be "0", and the one after should be the resolution
				screenSpec := fields[i+2]
				logger.Info("[xvfb-resolution]", "screen_spec", screenSpec)

				// Parse WIDTHxHEIGHTx24
				parts := strings.Split(screenSpec, "x")
				if len(parts) >= 2 {
					w, err := strconv.Atoi(parts[0])
					if err != nil {
						return 0, 0, fmt.Errorf("failed to parse width from %q: %w", screenSpec, err)
					}
					h, err := strconv.Atoi(parts[1])
					if err != nil {
						return 0, 0, fmt.Errorf("failed to parse height from %q: %w", screenSpec, err)
					}
					logger.Info("[xvfb-resolution]", "parsed", fmt.Sprintf("%dx%d", w, h))
					return w, h, nil
				}
			}
		}
	}

	return 0, 0, fmt.Errorf("Xvfb process not found in ps aux output")
}

// TestCDPTargetCreation tests that headless browsers can create new targets via CDP.
func TestCDPTargetCreation(t *testing.T) {
	image := headlessImage
	name := containerName + "-cdp-target"

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	baseCtx := logctx.AddToContext(context.Background(), logger)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Clean slate
	_ = stopContainer(baseCtx, name)

	// Start container
	width, height := 1024, 768
	_, exitCh, err := runContainer(baseCtx, image, name, map[string]string{"WIDTH": strconv.Itoa(width), "HEIGHT": strconv.Itoa(height)})
	require.NoError(t, err, "failed to start container: %v", err)
	defer stopContainer(baseCtx, name)

	ctx, cancel := context.WithTimeout(baseCtx, 2*time.Minute)
	defer cancel()

	logger.Info("[test]", "action", "waiting for API")
	require.NoError(t, waitHTTPOrExitWithLogs(ctx, apiBaseURL+"/spec.yaml", exitCh, name), "api not ready")

	// Wait for CDP endpoint to be ready (via the devtools proxy)
	logger.Info("[test]", "action", "waiting for CDP endpoint")
	require.NoError(t, waitTCP(ctx, "127.0.0.1:9222"), "CDP endpoint not ready")

	// Wait for Chromium to be fully initialized by checking if CDP responds
	logger.Info("[test]", "action", "waiting for Chromium to be fully ready")
	targets, err := listCDPTargets(ctx)
	if err != nil {
		logger.Error("[test]", "error", err.Error())
		require.Fail(t, "failed to list CDP targets")
	}

	// Use CDP HTTP API to list targets (avoids Playwright's implicit page creation)
	logger.Info("[test]", "action", "listing initial targets via CDP HTTP API")
	initialPageCount := 0
	for _, target := range targets {
		if targetType, ok := target["type"].(string); ok && targetType == "page" {
			initialPageCount++
		}
	}
	logger.Info("[test]", "initial_page_count", initialPageCount, "total_targets", len(targets))

	// Headless browser should start with at least 1 page target.
	// If --no-startup-window is enabled, the browser will start with 0 pages,
	// which will cause Target.createTarget to fail with "no browser is open (-32000)".
	require.GreaterOrEqual(t, initialPageCount, 1,
		"headless browser should start with at least 1 page target (got %d). "+
			"This usually means --no-startup-window flag is enabled in wrapper.sh, "+
			"which causes browsers to start without any pages.", initialPageCount)
}

// listCDPTargets lists all CDP targets via the HTTP API (inside the container)
func listCDPTargets(ctx context.Context) ([]map[string]interface{}, error) {
	// Use the internal CDP HTTP endpoint (port 9223) inside the container
	stdout, err := execCombinedOutput(ctx, "curl", []string{"-s", "http://localhost:9223/json/list"})
	if err != nil {
		return nil, fmt.Errorf("curl failed: %w, output: %s", err, stdout)
	}

	var targets []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &targets); err != nil {
		return nil, fmt.Errorf("failed to parse targets JSON: %w, output: %s", err, stdout)
	}

	return targets, nil
}

func TestWebBotAuthInstallation(t *testing.T) {
	image := headlessImage
	name := containerName + "-web-bot-auth"

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	baseCtx := logctx.AddToContext(context.Background(), logger)

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Clean slate
	_ = stopContainer(baseCtx, name)

	env := map[string]string{}

	// Start container
	_, exitCh, err := runContainer(baseCtx, image, name, env)
	require.NoError(t, err, "failed to start container: %v", err)
	defer stopContainer(baseCtx, name)

	ctx, cancel := context.WithTimeout(baseCtx, 3*time.Minute)
	defer cancel()

	logger.Info("[setup]", "action", "waiting for API", "url", apiBaseURL+"/spec.yaml")
	require.NoError(t, waitHTTPOrExitWithLogs(ctx, apiBaseURL+"/spec.yaml", exitCh, name), "api not ready: %v", err)

	// Build mock web-bot-auth extension zip in-memory
	extDir := t.TempDir()

	// Create manifest with webRequest permissions to trigger enterprise policy requirement
	manifest := map[string]interface{}{
		"manifest_version": 3,
		"version":          "1.0.0",
		"name":             "Web Bot Auth Mock",
		"description":      "Mock web-bot-auth extension for testing",
		"permissions":      []string{"webRequest", "webRequestBlocking"},
		"host_permissions": []string{"<all_urls>"},
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err, "marshal manifest: %v", err)

	err = os.WriteFile(filepath.Join(extDir, "manifest.json"), manifestJSON, 0600)
	require.NoError(t, err, "write manifest: %v", err)

	// Create update.xml required for enterprise policy
	updateXMLContent := `<?xml version="1.0" encoding="UTF-8"?>
<gupdate xmlns="http://www.google.com/update2/response" protocol="2.0">
  <app appid="aaaabbbbccccddddeeeeffffgggghhhh">
    <updatecheck codebase="http://localhost:10001/extensions/web-bot-auth/web-bot-auth.crx" version="1.0.0"/>
  </app>
</gupdate>`

	err = os.WriteFile(filepath.Join(extDir, "update.xml"), []byte(updateXMLContent), 0600)
	require.NoError(t, err, "write update.xml: %v", err)

	// Create a minimal .crx file (just needs to exist for the test)
	err = os.WriteFile(filepath.Join(extDir, "web-bot-auth.crx"), []byte("mock crx content"), 0600)
	require.NoError(t, err, "write .crx: %v", err)

	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "zip ext: %v", err)

	// Upload extension using the API
	{
		client, err := apiClient()
		require.NoError(t, err)
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		fw, err := w.CreateFormFile("extensions.zip_file", "web-bot-auth.zip")
		require.NoError(t, err)
		_, err = io.Copy(fw, bytes.NewReader(extZip))
		require.NoError(t, err)
		err = w.WriteField("extensions.name", "web-bot-auth")
		require.NoError(t, err)
		err = w.Close()
		require.NoError(t, err)

		logger.Info("[test]", "action", "uploading web-bot-auth extension")
		start := time.Now()
		rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
		elapsed := time.Since(start)
		require.NoError(t, err, "uploadExtensionsAndRestart request error: %v", err)
		require.Equal(t, http.StatusCreated, rsp.StatusCode(), "unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
		logger.Info("[test]", "action", "extension uploaded", "elapsed", elapsed.String())
	}

	// Verify the policy.json file contains the correct web-bot-auth configuration
	{
		logger.Info("[test]", "action", "reading policy.json")
		policyContent, err := execCombinedOutput(ctx, "cat", []string{"/etc/chromium/policies/managed/policy.json"})
		require.NoError(t, err, "failed to read policy.json: %v", err)

		logger.Info("[test]", "policy_content", policyContent)

		var policy map[string]interface{}
		err = json.Unmarshal([]byte(policyContent), &policy)
		require.NoError(t, err, "failed to parse policy.json: %v", err)

		// Check ExtensionInstallForcelist exists
		extensionInstallForcelist, ok := policy["ExtensionInstallForcelist"].([]interface{})
		require.True(t, ok, "ExtensionInstallForcelist not found in policy.json")
		require.GreaterOrEqual(t, len(extensionInstallForcelist), 1, "ExtensionInstallForcelist should have at least 1 entry")

		// Find the web-bot-auth entry in the forcelist
		var webBotAuthEntry string
		for _, entry := range extensionInstallForcelist {
			if entryStr, ok := entry.(string); ok && strings.Contains(entryStr, "web-bot-auth") {
				webBotAuthEntry = entryStr
				break
			}
		}
		require.NotEmpty(t, webBotAuthEntry, "web-bot-auth entry not found in ExtensionInstallForcelist")

		// Verify the entry format: "extension-id;update_url"
		parts := strings.Split(webBotAuthEntry, ";")
		require.Len(t, parts, 2, "expected web-bot-auth entry to have format 'extension-id;update_url'")

		extensionID := parts[0]
		updateURL := parts[1]

		logger.Info("[test]", "extension_id", extensionID, "update_url", updateURL)
		logger.Info("[test]", "result", "web-bot-auth policy verified successfully")
	}

	// Verify the extension directory exists
	{
		logger.Info("[test]", "action", "checking extension directory")
		dirList, err := execCombinedOutput(ctx, "ls", []string{"-la", "/home/kernel/extensions/web-bot-auth/"})
		require.NoError(t, err, "failed to list extension directory: %v", err)
		logger.Info("[test]", "extension_directory_contents", dirList)

		// Verify manifest.json exists (uploaded as part of the extension)
		manifestContent, err := execCombinedOutput(ctx, "cat", []string{"/home/kernel/extensions/web-bot-auth/manifest.json"})
		require.NoError(t, err, "failed to read manifest.json: %v", err)
		require.Contains(t, manifestContent, "Web Bot Auth Mock", "manifest.json should contain extension name")

		logger.Info("[test]", "result", "extension directory verified successfully")
	}
}
