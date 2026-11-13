package e2e

import (
	"context"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHeadlessViewportSurvivesChromiumRestart covers a headless viewport across
// configure requests that restart Chromium, such as a profile load. Headless
// Chromium relaunches maximized on its own 800x600 screen, so the server must
// apply the viewport again once the new browser is up, and a same-size
// PATCH /display must repair a viewport that drifted anyway.
func TestHeadlessViewportSurvivesChromiumRestart(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		"WIDTH":          "1024",
		"HEIGHT":         "768",
		"CHROMIUM_FLAGS": "--start-maximized --disable-infobars --test-type",
	}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")
	navigateBlank(t, ctx, c)

	client, err := c.APIClient()
	require.NoError(t, err)

	rsp := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
		display:       `{"width":1024,"height":768,"refresh_rate":60}`,
		chromiumFlags: `{"flags":["--disable-background-networking"]}`,
	})
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "configure restart at boot size: %s", string(rsp.Body))
	navigateBlank(t, ctx, c)
	waitForRendererViewport(t, ctx, c, 1024, 768, headlessViewportPredicate, "after restart at boot size", 30*time.Second)

	patchDisplayExpectingOK(t, ctx, c, 1280, 720, 60)
	waitForRendererViewport(t, ctx, c, 1280, 720, headlessViewportPredicate, "patched", 30*time.Second)

	rsp = chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
		chromiumFlags: `{"flags":["--disable-background-networking"]}`,
	})
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "configure restart: %s", string(rsp.Body))
	navigateBlank(t, ctx, c)
	waitForRendererViewport(t, ctx, c, 1280, 720, headlessViewportPredicate, "after restart", 30*time.Second)

	rsp = chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
		display:       `{"width":1440,"height":900,"refresh_rate":60}`,
		chromiumFlags: `{"flags":["--disable-background-networking"]}`,
	})
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "configure restart with display: %s", string(rsp.Body))
	navigateBlank(t, ctx, c)
	waitForRendererViewport(t, ctx, c, 1440, 900, headlessViewportPredicate, "after restart with display", 30*time.Second)

	// A restart the API doesn't drive (a supervisord crash restart) is not
	// restored, but a same-size PATCH /display repairs it.
	out, err := execCombinedOutput(ctx, c, "supervisorctl", []string{"-c", "/etc/supervisor/supervisord.conf", "restart", "chromium"})
	require.NoError(t, err, "supervisorctl restart chromium: %s", out)
	navigateBlank(t, ctx, c)
	drifted, err := getRendererViewport(ctx, c)
	require.NoError(t, err)
	require.NotEqual(t, 1440, drifted.InnerWidth, "viewport should drift after a supervisord restart")
	patchDisplayExpectingOK(t, ctx, c, 1440, 900, 60)
	waitForRendererViewport(t, ctx, c, 1440, 900, headlessViewportPredicate, "same-size patch after supervisord restart", 30*time.Second)
}
