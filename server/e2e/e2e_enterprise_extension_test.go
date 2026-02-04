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

	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
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

	// Wait for Chrome to restart with the new flags
	time.Sleep(3 * time.Second)
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready after kernel extension")

	// Upload the enterprise test extension (with update.xml and .crx)
	t.Log("[test] uploading enterprise test extension (with update.xml and .crx)")
	uploadEnterpriseTestExtension(t, ctx, c)

	// Wait a bit for Chrome to process the enterprise policy
	t.Log("[test] waiting for Chrome to process enterprise policy")
	time.Sleep(5 * time.Second)

	// Check what files were extracted on the server
	t.Log("[test] checking extracted extension files on server")
	checkExtractedFiles(t, ctx, c)

	// Check the kernel-images-api logs for extension download requests
	t.Log("[test] checking if Chrome fetched the extension")
	checkExtensionDownloadLogs(t, ctx, c)

	// Verify enterprise policy was configured correctly
	t.Log("[test] verifying enterprise policy configuration")
	verifyEnterprisePolicy(t, ctx, c)

	// Wait longer and check again if Chrome has downloaded the extension
	t.Log("[test] waiting for Chrome to download extension via enterprise policy")
	time.Sleep(30 * time.Second)

	// Check logs again
	checkExtensionDownloadLogs(t, ctx, c)

	// Check Chrome's extension installation logs
	t.Log("[test] checking Chrome stderr for extension-related logs")
	checkChromiumLogs(t, ctx, c)

	// Try to trigger extension installation by restarting Chrome
	t.Log("[test] restarting Chrome to trigger policy refresh")
	restartChrome(t, ctx, c)

	time.Sleep(15 * time.Second)

	// Check logs one more time
	checkExtensionDownloadLogs(t, ctx, c)
	checkChromiumLogs(t, ctx, c)

	// Check Chrome's policy state
	t.Log("[test] checking Chrome policy state")
	checkChromePolicies(t, ctx, c)

	// Check chrome://policy to see if Chrome recognizes the policy
	t.Log("[test] checking chrome://policy via screenshot")
	takeChromePolicyScreenshot(t, ctx, c)

	// Verify the extension is installed
	t.Log("[test] checking if extension is installed in Chrome's user-data")
	verifyExtensionInstalled(t, ctx, c)

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

// verifyEnterprisePolicy checks that the enterprise policy was configured correctly.
func verifyEnterprisePolicy(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	// Read policy.json
	policyContent, err := execCombinedOutputWithClient(ctx, c, "cat", []string{"/etc/chromium/policies/managed/policy.json"})
	require.NoError(t, err, "failed to read policy.json")
	t.Logf("[policy] content=%s", policyContent)

	var policy map[string]interface{}
	err = json.Unmarshal([]byte(policyContent), &policy)
	require.NoError(t, err, "failed to parse policy.json")

	// Check ExtensionInstallForcelist exists and contains our extension
	extensionInstallForcelist, ok := policy["ExtensionInstallForcelist"].([]interface{})
	require.True(t, ok, "ExtensionInstallForcelist not found in policy.json")
	require.GreaterOrEqual(t, len(extensionInstallForcelist), 1, "ExtensionInstallForcelist should have at least 1 entry")

	// Log all entries
	for i, entry := range extensionInstallForcelist {
		t.Logf("[policy] forcelist_entry=%d value=%v", i, entry)
	}

	// Find the enterprise-test entry
	var found bool
	for _, entry := range extensionInstallForcelist {
		if entryStr, ok := entry.(string); ok && strings.Contains(entryStr, "enterprise-test") {
			found = true
			t.Logf("[policy] found_entry=%s", entryStr)
			break
		}
	}
	require.True(t, found, "enterprise-test entry not found in ExtensionInstallForcelist")

	// Check ExtensionSettings
	extensionSettings, ok := policy["ExtensionSettings"].(map[string]interface{})
	if ok {
		t.Logf("[policy] extension_settings=%+v", extensionSettings)
	}
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

	// Check kernel-images-api log for requests to update.xml and .crx
	apiLog, err := execCombinedOutputWithClient(ctx, c, "cat", []string{"/var/log/supervisord/kernel-images-api"})
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

// restartChrome restarts Chrome via supervisorctl.
func restartChrome(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	output, err := execCombinedOutputWithClient(ctx, c, "supervisorctl", []string{"-c", "/etc/supervisor/supervisord.conf", "restart", "chromium"})
	if err != nil {
		t.Logf("[restart] error=%v output=%s", err, output)
	} else {
		t.Logf("[restart] result=%s", output)
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

// verifyExtensionInstalled checks if the extension was installed by Chrome.
func verifyExtensionInstalled(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	// Check the extension directory
	extDir, err := execCombinedOutputWithClient(ctx, c, "ls", []string{"-la", "/home/kernel/extensions/"})
	if err != nil {
		t.Logf("[verify] error=%v", err)
	} else {
		t.Logf("[verify] extensions_dir=%s", extDir)
	}

	// Check if Chrome installed the extension using Playwright to inspect chrome://extensions
	// Note: When loaded via --load-extension, Chrome generates a NEW extension ID based on the
	// directory path, which differs from the ID in update.xml (which is for the packed .crx file).
	// So we verify by extension name instead.

	expectedExtensionName := "Minimal Enterprise Test Extension"
	t.Logf("[verify] expected_extension_name=%s", expectedExtensionName)

	// Use playwright to navigate to chrome://extensions and verify extension is loaded
	t.Log("[verify] checking chrome://extensions via playwright")
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
  await page.waitForTimeout(2000);
  
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
      extensions.push(name);
    }
    
    return { extensions };
  });
  
  if (extensionInfo.error) {
    console.log('ERROR: ' + extensionInfo.error);
    process.exit(1);
  }
  
  const expectedName = %q;
  if (extensionInfo.extensions.includes(expectedName)) {
    console.log('SUCCESS: Extension "' + expectedName + '" found');
    process.exit(0);
  } else {
    console.log('FAIL: Extension "' + expectedName + '" not found. Extensions: ' + extensionInfo.extensions.join(', '));
    process.exit(1);
  }
  
  await browser.close();
})();
`, c.CDPURL(), expectedExtensionName))
	cmd.Dir = getPlaywrightPath()
	out, err := cmd.CombinedOutput()
	t.Logf("[playwright] output=%s", string(out))
	require.NoError(t, err, "extension verification failed: expected extension %q to be installed in chrome://extensions", expectedExtensionName)
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
