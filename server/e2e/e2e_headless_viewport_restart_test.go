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
// apply the viewport again once the new browser is up.
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

	patchDisplayExpectingOK(t, ctx, c, 1280, 720, 60)
	waitForRendererViewport(t, ctx, c, 1280, 720, headlessViewportPredicate, "patched", 30*time.Second)

	rsp := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
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
}
