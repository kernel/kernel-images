package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setWindowBoundsCDP sends Browser.setWindowBounds for the first page target's
// window.
func setWindowBoundsCDP(ctx context.Context, c *TestContainer, bounds map[string]any) error {
	cdp, err := newCDPClient(ctx, c.CDPURL())
	if err != nil {
		return err
	}
	defer cdp.Close()
	win, err := getChromiumWindowBoundsCDP(ctx, c)
	if err != nil {
		return err
	}
	_, err = cdp.Call(ctx, "Browser.setWindowBounds", map[string]any{
		"windowId": win.WindowID,
		"bounds":   bounds,
	}, "")
	return err
}

// setWindowBoundsPlaywrightCDP sends Browser.setWindowBounds through the
// Playwright executor, which is a separate client path from the CDP proxy.
func setWindowBoundsPlaywrightCDP(ctx context.Context, c *TestContainer) error {
	client, err := c.APIClient()
	if err != nil {
		return err
	}
	timeout := 5
	code := `
		const cdp = await context.newCDPSession(page);
		const {windowId} = await cdp.send('Browser.getWindowForTarget');
		await cdp.send('Browser.setWindowBounds', {
			windowId,
			bounds: {windowState: 'normal'},
		});
		return true;
	`
	rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, nil, instanceoapi.ExecutePlaywrightRequest{
		Code:       code,
		TimeoutSec: &timeout,
	})
	if err != nil {
		return err
	}
	if rsp.JSON200 == nil || !rsp.JSON200.Success {
		return fmt.Errorf("playwright window demotion failed: %s", string(rsp.Body))
	}
	return nil
}

// waitForWindowState polls the CDP window bounds until the window reports the
// wanted state, returning the matching bounds.
func waitForWindowState(t *testing.T, ctx context.Context, c *TestContainer, state string, label string) chromiumWindowBounds {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		win, err := getChromiumWindowBoundsCDP(ctx, c)
		if err == nil && win.WindowState == state {
			return win
		}
		if time.Now().After(deadline) {
			t.Fatalf("[%s] window never reached state %q: last=%+v err=%v", label, state, win, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("[%s] context cancelled waiting for state %q: %v", label, state, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// TestHeadfulUnmaximizeRestoreGeometry covers the recovery of a
// --start-maximized headful window after both supported client paths demote it.
func TestHeadfulUnmaximizeRestoreGeometry(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headfulImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		// Production-equivalent: the platform passes --start-maximized with
		// no explicit window geometry. container.go:51 always appends
		// --no-sandbox.
		"CHROMIUM_FLAGS": "--user-data-dir=/home/kernel/user-data --disable-dev-shm-usage --disable-gpu --start-maximized --remote-allow-origins=*",
	}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	waitForWindowState(t, ctx, c, "maximized", "baseline")
	rootW, rootH, err := getXRootResolution(ctx, c)
	require.NoError(t, err, "x root resolution")
	t.Logf("x root: %dx%d", rootW, rootH)

	// Demote the window through the CDP proxy, as direct customer CDP traffic
	// does. The running guard must restore the maximized state.
	require.NoError(t, setWindowBoundsCDP(ctx, c, map[string]any{"windowState": "normal"}), "setWindowBounds normal")
	restored := waitForWindowState(t, ctx, c, "maximized", "re-maximized after direct CDP")

	// The corrected window must cover the desktop. Check every edge so a large
	// window shifted off-screen cannot satisfy the width and height checks while
	// still exposing desktop background.
	assert.GreaterOrEqual(t, restored.Width, rootW-40, "restored width leaves desktop background visible")
	assert.GreaterOrEqual(t, restored.Height, rootH-40, "restored height leaves desktop background visible")
	assert.LessOrEqual(t, restored.Left, 40, "restored window starts off-screen horizontally")
	assert.LessOrEqual(t, restored.Top, 40, "restored window starts off-screen vertically")
	assert.GreaterOrEqual(t, restored.Left+restored.Width, rootW-40, "restored window does not reach the right edge; desktop background visible")
	assert.GreaterOrEqual(t, restored.Top+restored.Height, rootH-40, "restored window does not reach the bottom edge; desktop background visible")

	// Repeat the same demotion through the Playwright executor. This path does
	// not pass through the CDP proxy, so it must still be caught by X state.
	require.NoError(t, setWindowBoundsPlaywrightCDP(ctx, c), "playwright setWindowBounds normal")
	second := waitForWindowState(t, ctx, c, "maximized", "re-maximized after Playwright")
	assert.GreaterOrEqual(t, second.Width, rootW-40, "Playwright demotion leaves desktop background visible")
	assert.GreaterOrEqual(t, second.Height, rootH-40, "Playwright demotion leaves desktop background visible")
}
