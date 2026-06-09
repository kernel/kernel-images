package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/policy"
	"github.com/stretchr/testify/require"
)

// TestPinnedExtensionInstallation verifies that uploading an extension with
// pinned=true force-pins it to the toolbar via ExtensionSettings.toolbar_pin.
//
// It settles the two open questions behind the pinning design:
//  1. The toolbar_pin entry is keyed by the real extension ID, so Chrome applies
//     it (a --load-extension entry keyed by name would be ignored).
//  2. The ID we compute for the install path matches the ID Chrome actually
//     assigns to the loaded extension.
func TestPinnedExtensionInstallation(t *testing.T) {
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
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runPinnedExtensionTest(t, tc.image)
		})
	}
}

func runPinnedExtensionTest(t *testing.T, image string) {
	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	c := NewTestContainer(t, image)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	const extName = "pinned-test"
	uploadPinnedExtension(t, ctx, c, extName)

	// Wait for Chrome to restart with the extension loaded and the policy applied.
	time.Sleep(5 * time.Second)
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready after pinned extension")

	// The toolbar_pin entry must be keyed by the ID Chrome derives from the install path.
	expectedID := policy.UnpackedExtensionID("/home/kernel/extensions/" + extName)
	require.Regexp(t, `^[a-p]{32}$`, expectedID)

	policyPinID := readToolbarPinnedID(t, ctx, c)
	require.Equal(t, expectedID, policyPinID,
		"policy.json toolbar_pin must be keyed by the computed unpacked extension ID")

	chromeID := readLoadedExtensionID(t, ctx, c, "MV3 Service Worker Test")
	require.Equal(t, expectedID, chromeID,
		"Chrome's assigned extension ID must match our computed ID")
}

func uploadPinnedExtension(t *testing.T, ctx context.Context, c *TestContainer, name string) {
	t.Helper()

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	extDir, err := filepath.Abs("test-extension")
	require.NoError(t, err, "failed to resolve test-extension path")

	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err, "failed to zip test extension")

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("extensions.zip_file", "pinned-ext.zip")
	require.NoError(t, err)
	_, err = io.Copy(fw, bytes.NewReader(extZip))
	require.NoError(t, err)
	require.NoError(t, w.WriteField("extensions.name", name))
	require.NoError(t, w.WriteField("extensions.pinned", "true"))
	require.NoError(t, w.Close())

	rsp, err := client.UploadExtensionsAndRestartWithBodyWithResponse(ctx, w.FormDataContentType(), &body)
	require.NoError(t, err, "uploadExtensionsAndRestart request error")
	require.Equal(t, http.StatusCreated, rsp.StatusCode(),
		"expected 201 Created but got %d. Body: %s", rsp.StatusCode(), string(rsp.Body))
}

// readToolbarPinnedID reads policy.json and returns the single ExtensionSettings
// key carrying toolbar_pin=force_pinned.
func readToolbarPinnedID(t *testing.T, ctx context.Context, c *TestContainer) string {
	t.Helper()

	content, err := execCombinedOutputWithClient(ctx, c, "cat", []string{policy.PolicyPath})
	require.NoError(t, err, "failed to read policy.json")
	t.Logf("[policy] content=%s", content)

	var parsed struct {
		ExtensionSettings map[string]struct {
			ToolbarPin string `json:"toolbar_pin"`
		} `json:"ExtensionSettings"`
	}
	require.NoError(t, json.Unmarshal([]byte(content), &parsed), "failed to parse policy.json")

	pinnedIDs := make([]string, 0)
	for id, s := range parsed.ExtensionSettings {
		if s.ToolbarPin == "force_pinned" {
			pinnedIDs = append(pinnedIDs, id)
		}
	}
	require.Len(t, pinnedIDs, 1, "expected exactly one force_pinned entry")
	return pinnedIDs[0]
}

// readLoadedExtensionID returns the ID Chrome assigned to the loaded extension
// whose display name matches extName, read from chrome://extensions.
func readLoadedExtensionID(t *testing.T, ctx context.Context, c *TestContainer, extName string) string {
	t.Helper()

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

  const info = await page.evaluate(() => {
    const manager = document.querySelector('extensions-manager');
    if (!manager || !manager.shadowRoot) return { error: 'no extensions-manager' };
    const itemList = manager.shadowRoot.querySelector('extensions-item-list');
    if (!itemList || !itemList.shadowRoot) return { error: 'no item-list' };
    const items = itemList.shadowRoot.querySelectorAll('extensions-item');
    const out = [];
    for (const item of items) {
      if (!item.shadowRoot) continue;
      const nameEl = item.shadowRoot.querySelector('#name');
      out.push({ name: nameEl?.textContent?.trim() || 'unknown', id: item.getAttribute('id') });
    }
    return { out };
  });

  if (info.error) { console.log('ERROR: ' + info.error); process.exit(1); }
  const match = (info.out || []).find((e) => e.name === %q);
  if (!match) { console.log('FAIL: extension not found. Got: ' + JSON.stringify(info.out)); process.exit(1); }
  console.log('EXT_ID=' + match.id);

  await browser.close();
})();
`, c.CDPURL(), extName))
	cmd.Dir = getPlaywrightPath()
	out, err := cmd.CombinedOutput()
	t.Logf("[playwright] output=%s", string(out))
	require.NoError(t, err, "failed to read extension id from chrome://extensions")

	m := regexp.MustCompile(`EXT_ID=([a-p]{32})`).FindStringSubmatch(string(out))
	require.Len(t, m, 2, "EXT_ID marker not found in playwright output")
	return m[1]
}
