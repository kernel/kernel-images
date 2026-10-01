package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// Chromium terminates a renderer whose frame binds the WebMCP host from two
// documents (bad IPC reason 346). Pages that read modelContext on a scratch
// document, as some sandboxing libraries do at startup, have already made
// the first bind, so tool discovery must not make a second one.
const webMCPDuplicateBind = "Terminating renderer for bad IPC message, reason 346"

// testWebMCPScratchDocument lists tools once on pages that already read
// modelContext on a scratch document and asserts the renderer survives. It
// runs last on the shared container: a killed foreground tab stalls
// /playwright/execute, so a regression must not take later subtests with it.
func testWebMCPScratchDocument(t *testing.T, ctx context.Context, c *TestContainer, client *instanceoapi.ClientWithResponses) {
	t.Helper()

	for _, test := range []struct {
		name string
		file string
	}{
		{name: "top_level", file: "scratch-document.html"},
		{name: "sandbox_frame", file: "scratch-document-frame.html"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, err := os.ReadFile("testdata/webmcp/" + test.file)
			require.NoError(t, err)
			written, err := client.WriteFileWithBodyWithResponse(ctx,
				&instanceoapi.WriteFileParams{Path: "/tmp/" + test.file}, "text/html", bytes.NewReader(fixture))
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, written.StatusCode(), "%s", written.Body)

			pageURL := "file:///tmp/" + test.file
			var frames int
			executeWebMCPPlaywright(t, ctx, client, fmt.Sprintf(`
				await page.goto(%q, { waitUntil: 'load' });
				await page.evaluate(() => { window.__alive = true; });
				return page.frames().length;
			`, pageURL), &frames)
			t.Logf("%s loaded with %d frames", pageURL, frames)

			kills := strings.Count(chromiumLog(t, ctx, c), webMCPDuplicateBind)

			rsp, err := client.GetWebMCPToolsWithResponse(ctx, &instanceoapi.GetWebMCPToolsParams{})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rsp.StatusCode(), "%s", rsp.Body)
			t.Logf("GET /webmcp/tools: %s", rsp.Body)

			// Chromium logs the kill before the listing returns.
			log := chromiumLog(t, ctx, c)
			require.Equal(t, kills, strings.Count(log, webMCPDuplicateBind),
				"GET /webmcp/tools killed the renderer:\n%s", tailLines(log, 20))

			var alive bool
			executeWebMCPPlaywright(t, ctx, client, `return page.evaluate(() => window.__alive === true);`, &alive)
			require.True(t, alive, "the page reloaded during GET /webmcp/tools")
		})
	}
}

func chromiumLog(t *testing.T, ctx context.Context, c *TestContainer) string {
	t.Helper()
	log, err := execCombinedOutputWithClient(ctx, c, "cat", []string{"/var/log/supervisord/chromium"})
	require.NoError(t, err)
	return log
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
