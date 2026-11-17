package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dockedTint2Config = `panel_dock = 1
panel_position = bottom center horizontal
panel_size = 100% 30
`

// getNetWorkarea reads the first desktop's _NET_WORKAREA as x, y, width, height.
func getNetWorkarea(ctx context.Context, c *TestContainer) ([4]int, error) {
	var area [4]int
	out, err := execCombinedOutput(ctx, c, "bash", []string{"-c", "DISPLAY=:1 xprop -root _NET_WORKAREA"})
	if err != nil {
		return area, fmt.Errorf("xprop exec: %w (out=%q)", err, out)
	}
	_, values, ok := strings.Cut(out, "=")
	if !ok {
		return area, fmt.Errorf("unexpected xprop output %q", out)
	}
	fields := strings.Split(values, ",")
	if len(fields) < 4 {
		return area, fmt.Errorf("unexpected _NET_WORKAREA %q", out)
	}
	for i := range area {
		if area[i], err = strconv.Atoi(strings.TrimSpace(fields[i])); err != nil {
			return area, fmt.Errorf("parse _NET_WORKAREA %q: %w", out, err)
		}
	}
	return area, nil
}

// TestHeadfulDockedTint2KeepsFullWorkarea starts a docked tint2 panel through
// the process API and checks that it does not reserve any of the work area, so
// a maximized browser still covers the whole display.
func TestHeadfulDockedTint2KeepsFullWorkarea(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headfulImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		"CHROMIUM_FLAGS": "--user-data-dir=/home/kernel/user-data --disable-dev-shm-usage --disable-gpu --start-maximized --remote-allow-origins=*",
	}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	waitForWindowState(t, ctx, c, "maximized", "baseline")
	rootW, rootH, err := getXRootResolution(ctx, c)
	require.NoError(t, err, "x root resolution")

	client, err := c.APIClient()
	require.NoError(t, err)
	out, err := execCombinedOutput(ctx, c, "bash", []string{"-c", "cat > /tmp/tint2rc <<'EOF'\n" + dockedTint2Config + "EOF"})
	require.NoError(t, err, "write tint2rc: %s", out)
	spawn, err := client.ProcessSpawnWithResponse(ctx, instanceoapi.ProcessSpawnRequest{
		Command: "tint2",
		Args:    lo.ToPtr([]string{"-c", "/tmp/tint2rc"}),
		Env:     lo.ToPtr(map[string]string{"DISPLAY": ":1"}),
	})
	require.NoError(t, err, "spawn tint2")
	require.NotNil(t, spawn.JSON200, "spawn tint2: %s", spawn.Body)
	defer client.ProcessKillWithResponse(context.Background(), spawn.JSON200.ProcessId.String(), instanceoapi.ProcessKillRequest{Signal: "KILL"})

	require.Eventually(t, func() bool {
		out, err := execCombinedOutput(ctx, c, "bash", []string{"-c", "DISPLAY=:1 xwininfo -root -tree"})
		return err == nil && strings.Contains(out, "tint2")
	}, 20*time.Second, 250*time.Millisecond, "tint2 panel never mapped")
	// Give openbox time to dock the panel and publish any strut it causes.
	time.Sleep(2 * time.Second)

	area, err := getNetWorkarea(ctx, c)
	require.NoError(t, err, "read _NET_WORKAREA")
	assert.Equal(t, [4]int{0, 0, rootW, rootH}, area, "docked tint2 reserved part of the work area")

	win := waitForWindowState(t, ctx, c, "maximized", "with tint2")
	assert.Equal(t, rootW, win.Width, "maximized window is narrower than the display")
	assert.Equal(t, rootH, win.Height, "maximized window is shorter than the display")
}
