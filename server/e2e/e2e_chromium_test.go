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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/glebarez/sqlite"
	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

const (
	defaultHeadfulImage  = "onkernel/chromium-headful-test:latest"
	defaultHeadlessImage = "onkernel/chromium-headless-test:latest"
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
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"WIDTH":  "1024",
			"HEIGHT": "768",
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Get initial Xvfb resolution
	t.Log("getting initial Xvfb resolution")
	initialWidth, initialHeight, err := getXvfbResolution(ctx, c)
	require.NoError(t, err, "failed to get initial Xvfb resolution")
	t.Logf("initial_resolution: %dx%d", initialWidth, initialHeight)
	require.Equal(t, 1024, initialWidth, "expected initial width 1024")
	require.Equal(t, 768, initialHeight, "expected initial height 768")

	// Test first resolution change: 1920x1080
	t.Log("changing resolution to 1920x1080")
	width1 := 1920
	height1 := 1080
	req1 := instanceoapi.PatchDisplayJSONRequestBody{
		Width:  &width1,
		Height: &height1,
	}
	rsp1, err := client.PatchDisplayWithResponse(ctx, req1)
	require.NoError(t, err, "PATCH /display request failed")
	require.Equal(t, http.StatusOK, rsp1.StatusCode(), "unexpected status: %s body=%s", rsp1.Status(), string(rsp1.Body))
	require.NotNil(t, rsp1.JSON200, "expected JSON200 response, got nil")
	require.NotNil(t, rsp1.JSON200.Width, "expected width in response")
	require.Equal(t, width1, *rsp1.JSON200.Width, "expected width %d in response", width1)
	require.NotNil(t, rsp1.JSON200.Height, "expected height in response")
	require.Equal(t, height1, *rsp1.JSON200.Height, "expected height %d in response", height1)

	// Wait a bit for Xvfb to fully restart
	t.Log("waiting for Xvfb to stabilize")
	time.Sleep(3 * time.Second)

	// Verify new resolution via ps aux
	t.Log("verifying new Xvfb resolution")
	newWidth1, newHeight1, err := getXvfbResolution(ctx, c)
	require.NoError(t, err, "failed to get new Xvfb resolution")
	t.Logf("new_resolution: %dx%d", newWidth1, newHeight1)
	require.Equal(t, width1, newWidth1, "expected Xvfb resolution %dx%d, got %dx%d", width1, height1, newWidth1, newHeight1)
	require.Equal(t, height1, newHeight1, "expected Xvfb resolution %dx%d, got %dx%d", width1, height1, newWidth1, newHeight1)

	// Test second resolution change: 1280x720
	t.Log("changing resolution to 1280x720")
	width2 := 1280
	height2 := 720
	req2 := instanceoapi.PatchDisplayJSONRequestBody{
		Width:  &width2,
		Height: &height2,
	}
	rsp2, err := client.PatchDisplayWithResponse(ctx, req2)
	require.NoError(t, err, "PATCH /display request failed")
	require.Equal(t, http.StatusOK, rsp2.StatusCode(), "unexpected status: %s body=%s", rsp2.Status(), string(rsp2.Body))
	require.NotNil(t, rsp2.JSON200, "expected JSON200 response, got nil")
	require.NotNil(t, rsp2.JSON200.Width, "expected width in response")
	require.Equal(t, width2, *rsp2.JSON200.Width, "expected width %d in response", width2)
	require.NotNil(t, rsp2.JSON200.Height, "expected height in response")
	require.Equal(t, height2, *rsp2.JSON200.Height, "expected height %d in response", height2)

	// Wait a bit for Xvfb to fully restart
	t.Log("waiting for Xvfb to stabilize")
	time.Sleep(3 * time.Second)

	// Verify second resolution change via ps aux
	t.Log("verifying second Xvfb resolution")
	newWidth2, newHeight2, err := getXvfbResolution(ctx, c)
	require.NoError(t, err, "failed to get second Xvfb resolution")
	t.Logf("final_resolution: %dx%d", newWidth2, newHeight2)
	require.Equal(t, width2, newWidth2, "expected Xvfb resolution %dx%d, got %dx%d", width2, height2, newWidth2, newHeight2)
	require.Equal(t, height2, newHeight2, "expected Xvfb resolution %dx%d, got %dx%d", width2, height2, newWidth2, newHeight2)

	t.Log("all resolution changes verified successfully")
}

func TestExtensionUploadAndActivation(t *testing.T) {
	t.Parallel()
	ensurePlaywrightDeps(t)

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
	err := os.WriteFile(filepath.Join(extDir, "manifest.json"), []byte(manifest), 0600)
	require.NoError(t, err, "write manifest")
	err = os.WriteFile(filepath.Join(extDir, "content-script.js"), []byte(contentScript), 0600)
	require.NoError(t, err, "write content-script")

	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "zip ext")

	// Use new API to upload extension and restart Chromium
	{
		client, err := c.APIClient()
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
		require.NoError(t, err, "uploadExtensionsAndRestart request error")
		require.Equal(t, http.StatusCreated, rsp.StatusCode(), "unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
		t.Logf("/chromium/upload-extensions-and-restart completed in %s (%d ms)", elapsed.String(), elapsed.Milliseconds())
	}

	// Verify the content script updated the title on an allowed URL
	{
		cmd := exec.CommandContext(ctx, "pnpm", "exec", "tsx", "index.ts",
			"verify-title-contains",
			"--url", "https://www.sfmoma.org/",
			"--substr", "Title updated by browser extension",
			"--ws-url", c.CDPURL(),
			"--timeout", "45000",
		)
		cmd.Dir = getPlaywrightPath()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "title verify failed: %v output=%s", err, string(out))
	}
}

func TestScreenshotHeadless(t *testing.T) {
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

	// Whole-screen screenshot
	{
		rsp, err := client.TakeScreenshotWithResponse(ctx, instanceoapi.TakeScreenshotJSONRequestBody{})
		require.NoError(t, err, "screenshot request error")
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for full screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "response is not PNG (len=%d)", len(rsp.Body))
	}

	// Region screenshot (safe small region)
	{
		region := instanceoapi.ScreenshotRegion{X: 0, Y: 0, Width: 50, Height: 50}
		req := instanceoapi.TakeScreenshotJSONRequestBody{Region: &region}
		rsp, err := client.TakeScreenshotWithResponse(ctx, req)
		require.NoError(t, err, "region screenshot request error")
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for region screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "region response is not PNG (len=%d)", len(rsp.Body))
	}
}

func TestScreenshotHeadful(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headfulImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"WIDTH":  "1024",
			"HEIGHT": "768",
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	// Whole-screen screenshot
	{
		rsp, err := client.TakeScreenshotWithResponse(ctx, instanceoapi.TakeScreenshotJSONRequestBody{})
		require.NoError(t, err, "screenshot request error")
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for full screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "response is not PNG (len=%d)", len(rsp.Body))
	}

	// Region screenshot
	{
		region := instanceoapi.ScreenshotRegion{X: 0, Y: 0, Width: 80, Height: 60}
		req := instanceoapi.TakeScreenshotJSONRequestBody{Region: &region}
		rsp, err := client.TakeScreenshotWithResponse(ctx, req)
		require.NoError(t, err, "region screenshot request error")
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for region screenshot: %s body=%s", rsp.Status(), string(rsp.Body))
		require.True(t, isPNG(rsp.Body), "region response is not PNG (len=%d)", len(rsp.Body))
	}
}

func TestInputEndpointsSmoke(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	width, height := 1024, 768

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"WIDTH":  strconv.Itoa(width),
			"HEIGHT": strconv.Itoa(height),
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	// press_key: tap Return
	{
		rsp, err := client.PressKeyWithResponse(ctx, instanceoapi.PressKeyJSONRequestBody{Keys: []string{"Return"}})
		require.NoError(t, err, "press_key request error")
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for press_key: %s body=%s", rsp.Status(), string(rsp.Body))
	}

	// scroll: small vertical and horizontal ticks at center
	cx, cy := width/2, height/2
	{
		rsp, err := client.ScrollWithResponse(ctx, instanceoapi.ScrollJSONRequestBody{X: cx, Y: cy, DeltaX: lo.ToPtr(2), DeltaY: lo.ToPtr(-3)})
		require.NoError(t, err, "scroll request error")
		require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status for scroll: %s body=%s", rsp.Status(), string(rsp.Body))
	}
	// drag_mouse: simple short drag path
	{
		rsp, err := client.DragMouseWithResponse(ctx, instanceoapi.DragMouseJSONRequestBody{
			Path: [][]int{{cx - 10, cy - 10}, {cx + 10, cy + 10}},
		})
		require.NoError(t, err, "drag_mouse request error")
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
func execCombinedOutput(ctx context.Context, c *TestContainer, command string, args []string) (string, error) {
	client, err := c.APIClient()
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
func getXvfbResolution(ctx context.Context, c *TestContainer) (width, height int, err error) {
	// Get ps aux output
	stdout, err := execCombinedOutput(ctx, c, "ps", []string{"aux"})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to execute ps aux: %w, output: %s", err, stdout)
	}

	// Look for Xvfb line
	// Expected format: "root ... Xvfb :1 -screen 0 1920x1080x24 ..."
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if !strings.Contains(line, "Xvfb") {
			continue
		}

		// Parse the screen parameter
		// Look for pattern: "-screen 0 WIDTHxHEIGHTx24"
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "-screen" && i+2 < len(fields) {
				// Next field should be "0", and the one after should be the resolution
				screenSpec := fields[i+2]

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
					return w, h, nil
				}
			}
		}
	}

	return 0, 0, fmt.Errorf("Xvfb process not found in ps aux output")
}

// TestCDPTargetCreation tests that headless browsers can create new targets via CDP.
func TestCDPTargetCreation(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	width, height := 1024, 768

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"WIDTH":  strconv.Itoa(width),
			"HEIGHT": strconv.Itoa(height),
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "CDP endpoint not ready")

	// Wait for Chromium to be fully initialized by checking if CDP responds
	t.Log("waiting for Chromium to be fully ready")
	targets, err := listCDPTargets(ctx, c)
	require.NoError(t, err, "failed to list CDP targets")

	// Use CDP HTTP API to list targets (avoids Playwright's implicit page creation)
	t.Log("listing initial targets via CDP HTTP API")
	initialPageCount := 0
	for _, target := range targets {
		if targetType, ok := target["type"].(string); ok && targetType == "page" {
			initialPageCount++
		}
	}
	t.Logf("initial_page_count=%d, total_targets=%d", initialPageCount, len(targets))

	// Headless browser should start with at least 1 page target.
	// If --no-startup-window is enabled, the browser will start with 0 pages,
	// which will cause Target.createTarget to fail with "no browser is open (-32000)".
	require.GreaterOrEqual(t, initialPageCount, 1,
		"headless browser should start with at least 1 page target (got %d). "+
			"This usually means --no-startup-window flag is enabled in wrapper.sh, "+
			"which causes browsers to start without any pages.", initialPageCount)
}

// listCDPTargets lists all CDP targets via the HTTP API (inside the container)
func listCDPTargets(ctx context.Context, c *TestContainer) ([]map[string]interface{}, error) {
	// Use the internal CDP HTTP endpoint (port 9223) inside the container
	stdout, err := execCombinedOutput(ctx, c, "curl", []string{"-s", "http://localhost:9223/json/list"})
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
	require.NoError(t, err, "marshal manifest")

	err = os.WriteFile(filepath.Join(extDir, "manifest.json"), manifestJSON, 0600)
	require.NoError(t, err, "write manifest")

	// Create update.xml required for enterprise policy
	updateXMLContent := `<?xml version="1.0" encoding="UTF-8"?>
<gupdate xmlns="http://www.google.com/update2/response" protocol="2.0">
  <app appid="aaaabbbbccccddddeeeeffffgggghhhh">
    <updatecheck codebase="http://localhost:10001/extensions/web-bot-auth/web-bot-auth.crx" version="1.0.0"/>
  </app>
</gupdate>`

	err = os.WriteFile(filepath.Join(extDir, "update.xml"), []byte(updateXMLContent), 0600)
	require.NoError(t, err, "write update.xml")

	// Create a minimal .crx file (just needs to exist for the test)
	err = os.WriteFile(filepath.Join(extDir, "web-bot-auth.crx"), []byte("mock crx content"), 0600)
	require.NoError(t, err, "write .crx")

	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "zip ext")

	// Upload extension using the API
	{
		client, err := c.APIClient()
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

		t.Log("uploading web-bot-auth extension")
		start := time.Now()
		rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
		elapsed := time.Since(start)
		require.NoError(t, err, "uploadExtensionsAndRestart request error")
		require.Equal(t, http.StatusCreated, rsp.StatusCode(), "unexpected status: %s body=%s", rsp.Status(), string(rsp.Body))
		t.Logf("extension uploaded in %s", elapsed.String())
	}

	// Verify the policy.json file contains the correct web-bot-auth configuration
	{
		t.Log("reading policy.json")
		policyContent, err := execCombinedOutput(ctx, c, "cat", []string{"/etc/chromium/policies/managed/policy.json"})
		require.NoError(t, err, "failed to read policy.json")

		t.Logf("policy_content: %s", policyContent)

		var policy map[string]interface{}
		err = json.Unmarshal([]byte(policyContent), &policy)
		require.NoError(t, err, "failed to parse policy.json")

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

		t.Logf("extension_id=%s, update_url=%s", extensionID, updateURL)
		t.Log("web-bot-auth policy verified successfully")
	}

	// Verify the extension directory exists
	{
		t.Log("checking extension directory")
		dirList, err := execCombinedOutput(ctx, c, "ls", []string{"-la", "/home/kernel/extensions/web-bot-auth/"})
		require.NoError(t, err, "failed to list extension directory")
		t.Logf("extension_directory_contents: %s", dirList)

		// Verify manifest.json exists (uploaded as part of the extension)
		manifestContent, err := execCombinedOutput(ctx, c, "cat", []string{"/home/kernel/extensions/web-bot-auth/manifest.json"})
		require.NoError(t, err, "failed to read manifest.json")
		require.Contains(t, manifestContent, "Web Bot Auth Mock", "manifest.json should contain extension name")

		t.Log("extension directory verified successfully")
	}
}
