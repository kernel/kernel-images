package e2e

import (
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
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// TestEnterpriseExtensionInstallation tests that enterprise policy extensions
// (with update.xml and .crx files) are installed correctly via ExtensionInstallForcelist.
//
// This test verifies:
// 1. Extension with webRequest permission and update.xml/.crx files is uploaded successfully
// 2. Enterprise policy (ExtensionInstallForcelist) is correctly configured
// 3. Chrome fetches the update.xml and downloads the .crx file
// 4. Extension is installed and appears in chrome://extensions
//
// This test uses a real built extension (web-bot-auth) to reproduce production behavior.
// It runs against both headless and headful Chrome images.
func TestEnterpriseExtensionInstallation(t *testing.T) {
	t.Parallel()
	ensurePlaywrightDeps(t)

	testCases := []struct {
		name  string
		image string
	}{
		{"Headless", headlessImage},
		{"Headful", headfulImage},
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runEnterpriseExtensionTest(t, tc.image)
		})
	}
}

func runEnterpriseExtensionTest(t *testing.T, image string) {
	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	// Create and start container with dynamic ports
	c := NewTestContainer(t, image)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Use default CHROMIUM_FLAGS - the images now have --disable-background-networking removed
	// (headless) or never had it (headful), allowing Chrome to fetch extensions via
	// ExtensionInstallForcelist enterprise policy
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	t.Logf("[setup] waiting for API image=%s url=%s/spec.yaml", image, c.APIBaseURL())
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	// Wait for DevTools to be ready
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	// First upload a simple extension to simulate the kernel extension in production.
	// This causes Chrome to be launched with --load-extension, which mirrors production
	// where the kernel extension is always loaded before any enterprise extensions.
	t.Log("[test] uploading kernel-like extension first (to simulate prod)")
	uploadKernelLikeExtension(t, ctx, c)

	downloadLogBaseline := extensionDownloadLogSnapshot(t, ctx, c)

	// Upload the enterprise test extension (with update.xml and .crx)
	t.Log("[test] uploading enterprise test extension (with update.xml and .crx)")
	uploadEnterpriseTestExtension(t, ctx, c)

	// Check what files were extracted on the server
	t.Log("[test] checking extracted extension files on server")
	checkExtractedFiles(t, ctx, c)

	// Verify enterprise policy was configured correctly
	t.Log("[test] verifying enterprise policy configuration")
	waitForEnterprisePolicy(t, ctx, c, 10*time.Second)

	t.Log("[test] waiting for Chrome to download extension via enterprise policy")
	waitForExtensionDownload(t, ctx, c, downloadLogBaseline, 30*time.Second)

	// Check Chrome's extension installation logs
	checkChromiumLogs(t, ctx, c)

	// Check Chrome's policy state
	t.Log("[test] checking Chrome policy state")
	checkChromePolicies(t, ctx, c)

	// Check chrome://policy to see if Chrome recognizes the policy
	t.Log("[test] checking chrome://policy via screenshot")
	takeChromePolicyScreenshot(t, ctx, c)

	// Verify the extension is installed
	t.Log("[test] checking if extension is installed in Chrome's user-data")
	waitForExtensionInstalled(t, ctx, c, 30*time.Second)

	t.Log("[test] enterprise extension installation test completed")
}

// uploadKernelLikeExtension uploads a simple extension to simulate the kernel extension.
// In production, the kernel extension is always loaded before any enterprise extensions,
// so this ensures the test mirrors that behavior.
func uploadKernelLikeExtension(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Get the path to the simple test extension (no webRequest, so no enterprise policy)
	extDir, err := filepath.Abs("test-extension")
	require.NoError(t, err, "failed to get absolute path to test-extension")

	// Create zip of the extension
	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "failed to zip test extension")

	// Upload extension
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("extensions.zip_file", "kernel-like-ext.zip")
	require.NoError(t, err)
	_, err = io.Copy(fw, bytes.NewReader(extZip))
	require.NoError(t, err)
	err = w.WriteField("extensions.name", "kernel")
	require.NoError(t, err)
	err = w.Close()
	require.NoError(t, err)

	start := time.Now()
	rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
	elapsed := time.Since(start)
	require.NoError(t, err, "uploadExtensionsAndRestart request error")

	require.Equal(t, http.StatusCreated, rsp.StatusCode(),
		"expected 201 Created but got %d. Body: %s",
		rsp.StatusCode(), string(rsp.Body))

	t.Logf("[kernel-ext] uploaded kernel-like extension elapsed=%s", elapsed.String())
}

// uploadEnterpriseTestExtension uploads the test extension with update.xml and .crx files.
// This should trigger enterprise policy handling via ExtensionInstallForcelist.
func uploadEnterpriseTestExtension(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Get the path to the test extension
	extDir, err := filepath.Abs("test-extension-enterprise")
	require.NoError(t, err, "failed to get absolute path to test-extension-enterprise")

	// Read and log the manifest
	manifestPath := filepath.Join(extDir, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	require.NoError(t, err, "failed to read manifest.json")
	t.Logf("[extension] manifest=%s", string(manifestData))

	// Read and log the update.xml
	updateXMLPath := filepath.Join(extDir, "update.xml")
	updateXMLData, err := os.ReadFile(updateXMLPath)
	require.NoError(t, err, "failed to read update.xml")
	t.Logf("[extension] update.xml=%s", string(updateXMLData))

	// Verify .crx exists
	crxPath := filepath.Join(extDir, "extension.crx")
	crxInfo, err := os.Stat(crxPath)
	require.NoError(t, err, "failed to stat .crx file")
	t.Logf("[extension] crx_size=%d", crxInfo.Size())

	// Create zip of the extension
	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "failed to zip test extension")

	// Upload extension
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("extensions.zip_file", "enterprise-test-ext.zip")
	require.NoError(t, err)
	_, err = io.Copy(fw, bytes.NewReader(extZip))
	require.NoError(t, err)
	err = w.WriteField("extensions.name", "enterprise-test")
	require.NoError(t, err)
	err = w.Close()
	require.NoError(t, err)

	start := time.Now()
	rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
	elapsed := time.Since(start)
	require.NoError(t, err, "uploadExtensionsAndRestart request error")

	// The key assertion: this should return 201
	require.Equal(t, http.StatusCreated, rsp.StatusCode(),
		"expected 201 Created but got %d. Body: %s",
		rsp.StatusCode(), string(rsp.Body))

	t.Logf("[extension] uploaded elapsed=%s", elapsed.String())
}

func waitForEnterprisePolicy(t *testing.T, ctx context.Context, c *TestContainer, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastContent string
	var lastErr error
	for {
		policyContent, err := enterprisePolicyContent(ctx, c)
		lastContent, lastErr = policyContent, err
		if err == nil {
			lastErr = assertEnterprisePolicy(policyContent)
			if lastErr == nil {
				t.Logf("[policy] configured content=%s", policyContent)
				return
			}
		}
		if time.Now().After(deadline) {
			require.NoError(t, lastErr, "enterprise policy did not become ready within %s; last_content=%s", timeout, lastContent)
			return
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "context cancelled waiting for enterprise policy")
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func enterprisePolicyContent(ctx context.Context, c *TestContainer) (string, error) {
	// Read policy.json
	policyContent, err := execCombinedOutputWithClient(ctx, c, "cat", []string{"/etc/chromium/policies/managed/policy.json"})
	if err != nil {
		return "", fmt.Errorf("failed to read policy.json: %w", err)
	}
	return policyContent, nil
}

func assertEnterprisePolicy(policyContent string) error {
	var policy map[string]interface{}
	if err := json.Unmarshal([]byte(policyContent), &policy); err != nil {
		return fmt.Errorf("failed to parse policy.json: %w", err)
	}

	maxConnectionsPerProxy, ok := policy["MaxConnectionsPerProxy"].(float64)
	if !ok {
		return fmt.Errorf("MaxConnectionsPerProxy not found in policy.json")
	}
	if maxConnectionsPerProxy != float64(16) {
		return fmt.Errorf("unexpected MaxConnectionsPerProxy value: %v", maxConnectionsPerProxy)
	}

	// Check ExtensionInstallForcelist exists and contains our extension
	extensionInstallForcelist, ok := policy["ExtensionInstallForcelist"].([]interface{})
	if !ok {
		return fmt.Errorf("ExtensionInstallForcelist not found in policy.json")
	}
	if len(extensionInstallForcelist) < 1 {
		return fmt.Errorf("ExtensionInstallForcelist should have at least 1 entry")
	}

	// Find the enterprise-test entry
	var found bool
	for _, entry := range extensionInstallForcelist {
		if entryStr, ok := entry.(string); ok && strings.Contains(entryStr, "enterprise-test") {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("enterprise-test entry not found in ExtensionInstallForcelist")
	}
	return nil
}

// checkExtractedFiles checks what files were extracted on the server side.
func checkExtractedFiles(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	// List all files in the extension directory
	output, err := execCombinedOutputWithClient(ctx, c, "ls", []string{"-la", "/home/kernel/extensions/enterprise-test/"})
	if err != nil {
		t.Logf("[files] error=%v", err)
	} else {
		t.Logf("[files] extension_dir=%s", output)
	}

	// Check if update.xml exists
	updateXML, err := execCombinedOutputWithClient(ctx, c, "cat", []string{"/home/kernel/extensions/enterprise-test/update.xml"})
	if err != nil {
		t.Logf("[files] update_xml_error=%v", err)
	} else {
		t.Logf("[files] update.xml=%s", updateXML)
	}

	// Check if .crx exists
	crxOutput, err := execCombinedOutputWithClient(ctx, c, "ls", []string{"-la", "/home/kernel/extensions/enterprise-test/*.crx"})
	if err != nil {
		t.Logf("[files] crx_error=%v", err)
	} else {
		t.Logf("[files] crx_files=%s", crxOutput)
	}

	// Check file types
	fileOutput, err := execCombinedOutputWithClient(ctx, c, "file", []string{"/home/kernel/extensions/enterprise-test/extension.crx"})
	if err != nil {
		t.Logf("[files] file_type_error=%v", err)
	} else {
		t.Logf("[files] crx_file_type=%s", fileOutput)
	}
}

// checkExtensionDownloadLogs checks the kernel-images-api logs for extension download requests.
func checkExtensionDownloadLogs(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	apiLog, err := extensionDownloadLog(ctx, c)
	if err != nil {
		t.Logf("[logs] error=%v", err)
		return
	}

	lines := strings.Split(apiLog, "\n")
	for _, line := range lines {
		if strings.Contains(line, "update.xml") || strings.Contains(line, ".crx") || strings.Contains(line, "extension") {
			t.Logf("[logs] line=%s", line)
		}
	}

	// Check specifically for GET requests to our extension
	if strings.Contains(apiLog, "GET") && strings.Contains(apiLog, "enterprise-test") {
		t.Log("[logs] Chrome made GET requests to fetch the extension!")
	} else {
		t.Log("[logs] No GET requests to enterprise-test extension found")
	}

	// Log all GET requests
	for _, line := range lines {
		if strings.Contains(line, "GET") {
			t.Logf("[logs] GET_request=%s", line)
		}
	}
}

func extensionDownloadLogSnapshot(t *testing.T, ctx context.Context, c *TestContainer) string {
	t.Helper()
	apiLog, err := extensionDownloadLog(ctx, c)
	if err != nil {
		t.Logf("[logs] baseline_error=%v", err)
		return ""
	}
	return apiLog
}

func waitForExtensionDownload(t *testing.T, ctx context.Context, c *TestContainer, baseline string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastLog string
	var lastErr error
	for {
		apiLog, err := extensionDownloadLog(ctx, c)
		lastLog, lastErr = extensionDownloadLogSince(apiLog, baseline), err
		if err == nil && extensionDownloadObserved(lastLog) {
			t.Log("[logs] Chrome made GET requests to fetch the enterprise extension")
			checkExtensionDownloadLogs(t, ctx, c)
			return
		}
		if time.Now().After(deadline) {
			require.NoError(t, lastErr, "extension download was not observed within %s; last_log=%s", timeout, lastLog)
			require.True(t, extensionDownloadObserved(lastLog), "extension download was not observed within %s", timeout)
			return
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "context cancelled waiting for extension download")
			return
		case <-time.After(1 * time.Second):
		}
	}
}

func extensionDownloadLog(ctx context.Context, c *TestContainer) (string, error) {
	return execCombinedOutputWithClient(ctx, c, "cat", []string{"/var/log/supervisord/kernel-images-api"})
}

func extensionDownloadLogSince(apiLog, baseline string) string {
	if baseline != "" && strings.HasPrefix(apiLog, baseline) {
		return apiLog[len(baseline):]
	}
	return apiLog
}

func extensionDownloadObserved(apiLog string) bool {
	var sawUpdateXML, sawCRX bool
	for _, line := range strings.Split(apiLog, "\n") {
		if !strings.Contains(line, "GET") || !strings.Contains(line, "enterprise-test") {
			continue
		}
		if strings.Contains(line, "update.xml") {
			sawUpdateXML = true
		}
		if strings.Contains(line, ".crx") {
			sawCRX = true
		}
	}
	return sawUpdateXML && sawCRX
}

func TestExtensionDownloadObservedRequiresUpdateXMLAndCRX(t *testing.T) {
	t.Parallel()

	require.False(t, extensionDownloadObserved(""))
	require.False(t, extensionDownloadObserved(`GET http://127.0.0.1/extensions/enterprise-test/update.xml HTTP/1.1`))
	require.False(t, extensionDownloadObserved(`GET http://127.0.0.1/extensions/enterprise-test/extension.crx HTTP/1.1`))
	require.False(t, extensionDownloadObserved(`GET http://127.0.0.1/extensions/kernel/update.xml HTTP/1.1
GET http://127.0.0.1/extensions/kernel/extension.crx HTTP/1.1`))
	require.True(t, extensionDownloadObserved(`GET http://127.0.0.1/extensions/enterprise-test/update.xml HTTP/1.1
GET http://127.0.0.1/extensions/enterprise-test/extension.crx HTTP/1.1`))
}

func TestExtensionDownloadLogSince(t *testing.T) {
	t.Parallel()

	baseline := "line 1\nline 2\n"
	require.Equal(t, "line 3\n", extensionDownloadLogSince(baseline+"line 3\n", baseline))
	require.Equal(t, "line 3\n", extensionDownloadLogSince("line 3\n", baseline))
	require.Equal(t, "line 3\n", extensionDownloadLogSince("line 3\n", ""))
}

// checkChromePolicies checks how Chrome sees the policies.
func checkChromePolicies(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	// Check Chrome's local state for policy info
	localState, err := execCombinedOutputWithClient(ctx, c, "cat", []string{"/home/kernel/user-data/Local State"})
	if err != nil {
		t.Logf("[policies] local_state_error=%v", err)
	} else {
		// Try to parse and look for extension-related info
		var state map[string]interface{}
		if err := json.Unmarshal([]byte(localState), &state); err != nil {
			t.Logf("[policies] parse_error=%v", err)
		} else {
			// Look for extensions in local state
			if ext, ok := state["extensions"]; ok {
				t.Logf("[policies] extensions_in_local_state=%+v", ext)
			}
		}
	}

	// Check if Chrome has read the policy file
	// chrome://policy data could be extracted via CDP but that's complex
	// Instead, let's check if there's any extension component data
	extSettingsPath := "/home/kernel/user-data/Default/Extension Settings"
	extSettings, err := execCombinedOutputWithClient(ctx, c, "ls", []string{"-la", extSettingsPath})
	if err != nil {
		t.Logf("[policies] ext_settings_dir_error=%v", err)
	} else {
		t.Logf("[policies] ext_settings_dir=%s", extSettings)
	}
}

// checkChromiumLogs checks Chrome's logs for extension-related messages.
func checkChromiumLogs(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	// Check chromium supervisor log for extension-related messages
	chromiumLog, err := execCombinedOutputWithClient(ctx, c, "cat", []string{"/var/log/supervisord/chromium"})
	if err != nil {
		t.Logf("[chromium-log] error=%v", err)
		return
	}

	lines := strings.Split(chromiumLog, "\n")
	for _, line := range lines {
		lowLine := strings.ToLower(line)
		if strings.Contains(lowLine, "extension") ||
			strings.Contains(lowLine, "policy") ||
			strings.Contains(lowLine, "crx") ||
			strings.Contains(lowLine, "update") ||
			strings.Contains(lowLine, "error") ||
			strings.Contains(lowLine, "fail") {
			t.Logf("[chromium-log] line=%s", line)
		}
	}

	// Also check stdout/stderr for the last 100 lines
	t.Log("[chromium-log] checking last 100 lines of chromium log")
	tailOutput, err := execCombinedOutputWithClient(ctx, c, "tail", []string{"-n", "100", "/var/log/supervisord/chromium"})
	if err != nil {
		t.Logf("[chromium-log] tail_error=%v", err)
	} else {
		t.Logf("[chromium-log] last_100_lines=%s", tailOutput)
	}
}

// takeChromePolicyScreenshot takes a screenshot of chrome://policy to debug what Chrome sees
func takeChromePolicyScreenshot(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	// Navigate using playwright then take screenshot
	cmd := exec.CommandContext(ctx, "pnpm", "exec", "tsx", "-e", fmt.Sprintf(`
const { chromium } = require('playwright-core');

(async () => {
  const browser = await chromium.connectOverCDP('%s');
  const contexts = browser.contexts();
  const ctx = contexts[0] || await browser.newContext();
  const pages = ctx.pages();
  const page = pages[0] || await ctx.newPage();
  
  // Go to extensions page first to check for extension errors
  console.log('=== CHECKING EXTENSIONS ===');
  await page.goto('chrome://extensions');
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(2000);
  
  // Use evaluate to pierce shadow DOM and get extension info
  const extensionInfo = await page.evaluate(() => {
    const manager = document.querySelector('extensions-manager');
    if (!manager || !manager.shadowRoot) return { error: 'no extensions-manager' };
    
    const itemList = manager.shadowRoot.querySelector('extensions-item-list');
    if (!itemList || !itemList.shadowRoot) return { error: 'no item-list' };
    
    const items = itemList.shadowRoot.querySelectorAll('extensions-item');
    const extensions = [];
    
    for (const item of items) {
      if (!item.shadowRoot) continue;
      const nameEl = item.shadowRoot.querySelector('#name');
      const name = nameEl?.textContent?.trim() || 'unknown';
      const id = item.getAttribute('id');
      
      // Check for errors
      const warningsEl = item.shadowRoot.querySelector('.warning-list');
      const warnings = warningsEl?.textContent?.trim() || '';
      
      extensions.push({ name, id, warnings });
    }
    
    return { extensions };
  });
  
  console.log('Extensions found:', JSON.stringify(extensionInfo, null, 2));
  
  await browser.close();
})();
`, c.CDPURL()))
	cmd.Dir = getPlaywrightPath()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("[policy-screenshot] error=%v output=%s", err, string(out))
	} else {
		t.Logf("[policy-screenshot] output=%s", string(out))
	}
}

func waitForExtensionInstalled(t *testing.T, ctx context.Context, c *TestContainer, timeout time.Duration) {
	t.Helper()
	expectedExtensionName := "Minimal Enterprise Test Extension"
	deadline := time.Now().Add(timeout)
	var lastOut []byte
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Logf("[verify] last_output=%s", string(lastOut))
			require.NoError(t, lastErr, "extension %q was not installed within %s", expectedExtensionName, timeout)
			return
		}

		attemptTimeout := 15 * time.Second
		if remaining < attemptTimeout {
			attemptTimeout = remaining
		}
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		lastOut, lastErr = chromeExtensionsCheckOutput(attemptCtx, c, expectedExtensionName)
		cancel()
		if lastErr == nil {
			t.Logf("[verify] extension installed output=%s", string(lastOut))
			return
		}

		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "context cancelled waiting for extension installation")
			return
		case <-time.After(1 * time.Second):
		}
	}
}

func chromeExtensionsCheckOutput(ctx context.Context, c *TestContainer, expectedExtensionName string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "pnpm", "exec", "tsx", "-e", fmt.Sprintf(`
const { chromium } = require('playwright-core');

(async () => {
  const browser = await chromium.connectOverCDP('%s');
  const contexts = browser.contexts();
  const ctx = contexts[0] || await browser.newContext();
  const pages = ctx.pages();
  const page = pages[0] || await ctx.newPage();

  await page.goto('chrome://extensions');
  await page.waitForLoadState('networkidle');

  const expectedName = %q;

  const readExtensions = async () => await page.evaluate(() => {
    const manager = document.querySelector('extensions-manager');
    if (!manager || !manager.shadowRoot) return { error: 'no extensions-manager' };

    const itemList = manager.shadowRoot.querySelector('extensions-item-list');
    if (!itemList || !itemList.shadowRoot) return { error: 'no item-list' };

    const items = itemList.shadowRoot.querySelectorAll('extensions-item');
    const extensions = [];

    for (const item of items) {
      if (!item.shadowRoot) continue;
      const nameEl = item.shadowRoot.querySelector('#name');
      const name = nameEl?.textContent?.trim() || 'unknown';
      extensions.push(name);
    }

    return { extensions };
  });

  try {
    await page.waitForFunction((expectedName) => {
      const manager = document.querySelector('extensions-manager');
      if (!manager || !manager.shadowRoot) return false;

      const itemList = manager.shadowRoot.querySelector('extensions-item-list');
      if (!itemList || !itemList.shadowRoot) return false;

      const items = itemList.shadowRoot.querySelectorAll('extensions-item');
      for (const item of items) {
        if (!item.shadowRoot) continue;
        const nameEl = item.shadowRoot.querySelector('#name');
        const name = nameEl?.textContent?.trim() || 'unknown';
        if (name === expectedName) return true;
      }
      return false;
    }, expectedName, { timeout: 2000 });

    const extensionInfo = await readExtensions();
    console.log('SUCCESS: Extension "' + expectedName + '" found. Extensions: ' + extensionInfo.extensions.join(', '));
    await browser.close();
    process.exit(0);
  } catch (err) {
    const extensionInfo = await readExtensions();
    if (extensionInfo.error) {
      console.log('ERROR: ' + extensionInfo.error);
    } else {
      console.log('FAIL: Extension "' + expectedName + '" not found. Extensions: ' + extensionInfo.extensions.join(', '));
    }
    console.log('wait_error=' + err.message);
    await browser.close();
    process.exit(1);
  }
})();
`, c.CDPURL(), expectedExtensionName))
	cmd.Dir = getPlaywrightPath()
	return cmd.CombinedOutput()
}

// execCombinedOutputWithClient executes a command in the container via the API.
func execCombinedOutputWithClient(ctx context.Context, c *TestContainer, command string, args []string) (string, error) {
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
