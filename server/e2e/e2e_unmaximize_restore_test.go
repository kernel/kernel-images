package e2e

import (
	"context"
	"os/exec"
	"testing"
	"time"

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

// TestHeadfulUnmaximizeRestoreGeometry covers the restore geometry of a
// --start-maximized headful window. Chromium boots maximized without any
// normal-state geometry of its own; the launcher seeds --window-size to the X
// root so the first un-maximize (CDP Browser.setWindowBounds from an
// automation, or a demote before a viewport resize) restores to a root-sized
// window instead of chromium's default ~1050x1080 sliver with the desktop
// background showing beside it.
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

	// Demote the window exactly the way customer CDP traffic (or the
	// viewport-resize demote) does.
	require.NoError(t, setWindowBoundsCDP(ctx, c, map[string]any{"windowState": "normal"}), "setWindowBounds normal")
	restored := waitForWindowState(t, ctx, c, "normal", "un-maximized")

	// The restored window must cover the desktop: no desktop background may
	// show beside it. The seeded bounds are the root size, and the CSD window
	// carries no WM decoration, so demand near-exact coverage.
	assert.GreaterOrEqual(t, restored.Width, rootW-40, "restored width leaves desktop background visible")
	assert.GreaterOrEqual(t, restored.Height, rootH-40, "restored height leaves desktop background visible")
	assert.LessOrEqual(t, restored.Left, 40, "restored window starts off-screen horizontally")
	assert.LessOrEqual(t, restored.Top, 40, "restored window starts off-screen vertically")

	// Maximizing again must still work — un-maximize never wedges the
	// window in normal state.
	require.NoError(t, setWindowBoundsCDP(ctx, c, map[string]any{"windowState": "maximized"}), "setWindowBounds maximized")
	waitForWindowState(t, ctx, c, "maximized", "re-maximized")
}
