package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// rendererViewport captures the dimensions visible to JS inside the page. It is
// the cheapest end-to-end signal that the OS window has been resized to fill
// the new X root: the renderer can't report a larger outerWidth/outerHeight
// than the actual chromium window the WM gave it.
type rendererViewport struct {
	InnerWidth  int `json:"innerWidth"`
	InnerHeight int `json:"innerHeight"`
	OuterWidth  int `json:"outerWidth"`
	OuterHeight int `json:"outerHeight"`
	ScreenWidth int `json:"screenWidth"`
	ScreenHght  int `json:"screenHeight"`
	DPR         int `json:"devicePixelRatio"`
}

// getRendererViewport evaluates window.* sizes in the active page via the
// playwright daemon.
func getRendererViewport(ctx context.Context, c *TestContainer) (rendererViewport, error) {
	client, err := c.APIClient()
	if err != nil {
		return rendererViewport{}, err
	}
	code := `
		return await page.evaluate(() => ({
			innerWidth: window.innerWidth,
			innerHeight: window.innerHeight,
			outerWidth: window.outerWidth,
			outerHeight: window.outerHeight,
			screenWidth: screen.width,
			screenHeight: screen.height,
			devicePixelRatio: window.devicePixelRatio,
		}));
	`
	timeout := 5
	rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightRequest{
		Code:       code,
		TimeoutSec: &timeout,
	})
	if err != nil {
		return rendererViewport{}, err
	}
	if rsp.JSON200 == nil || !rsp.JSON200.Success {
		body := ""
		if rsp != nil {
			body = string(rsp.Body)
		}
		return rendererViewport{}, fmt.Errorf("playwright eval failed: %s", body)
	}
	raw, err := json.Marshal(rsp.JSON200.Result)
	if err != nil {
		return rendererViewport{}, err
	}
	var v rendererViewport
	if err := json.Unmarshal(raw, &v); err != nil {
		return rendererViewport{}, fmt.Errorf("unmarshal viewport %q: %w", string(raw), err)
	}
	return v, nil
}

// getXRootResolution reads the live X root size via xrandr inside the
// container. Works for both Xorg (headful) and Xvfb (headless). The parse
// pulls from xrandr's `Screen 0: ... current W x H, ...` header — a
// direct read of the X root size that doesn't depend on which connected
// output happens to own the active mode. The server's
// getCurrentResolutionFromXrandr greps for `*` on a per-output mode
// line; both approaches converge on the same value on this image, but
// the `Screen 0: current` form is the most direct.
func getXRootResolution(ctx context.Context, c *TestContainer) (int, int, error) {
	out, err := execCombinedOutput(ctx, c, "bash", []string{"-c", "DISPLAY=:1 xrandr"})
	if err != nil {
		return 0, 0, fmt.Errorf("xrandr exec: %w (out=%q)", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, "current ")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("current "):]
		end := strings.Index(rest, ",")
		if end > 0 {
			rest = rest[:end]
		}
		fields := strings.Fields(rest) // ["W", "x", "H"]
		if len(fields) < 3 {
			return 0, 0, fmt.Errorf("unexpected current segment %q", rest)
		}
		w, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, 0, fmt.Errorf("parse width %q: %w", fields[0], err)
		}
		h, err := strconv.Atoi(fields[2])
		if err != nil {
			return 0, 0, fmt.Errorf("parse height %q: %w", fields[2], err)
		}
		return w, h, nil
	}
	return 0, 0, fmt.Errorf("could not find 'current WxH' in xrandr output: %s", strings.TrimSpace(out))
}

// chromiumWindowBounds is the subset of Browser.getWindowBounds we care
// about. windowState distinguishes the "maximized"/"fullscreen" case
// (where width/height reflect the live OS window size, which the WM
// aligns with the X root) from "normal" (where width/height reflect the
// saved-restore bounds that would be applied on un-maximize).
type chromiumWindowBounds struct {
	WindowID    int    `json:"windowId"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Left        int    `json:"left"`
	Top         int    `json:"top"`
	WindowState string `json:"windowState"`
}

// getChromiumWindowBoundsCDP queries Browser.getWindowForTarget +
// Browser.getWindowBounds for the first page target. Used to assert that
// the OS window stays in maximized/fullscreen state across a resize.
func getChromiumWindowBoundsCDP(ctx context.Context, c *TestContainer) (chromiumWindowBounds, error) {
	cdp, err := newCDPClient(ctx, c.CDPURL())
	if err != nil {
		return chromiumWindowBounds{}, fmt.Errorf("dial cdp: %w", err)
	}
	defer cdp.Close()

	targetsRaw, err := cdp.Call(ctx, "Target.getTargets", map[string]any{}, "")
	if err != nil {
		return chromiumWindowBounds{}, fmt.Errorf("Target.getTargets: %w", err)
	}
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(targetsRaw, &targets); err != nil {
		return chromiumWindowBounds{}, fmt.Errorf("unmarshal targets: %w", err)
	}
	var pageID string
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			pageID = t.TargetID
			break
		}
	}
	if pageID == "" {
		return chromiumWindowBounds{}, fmt.Errorf("no page target found")
	}

	winRaw, err := cdp.Call(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": pageID}, "")
	if err != nil {
		return chromiumWindowBounds{}, fmt.Errorf("Browser.getWindowForTarget: %w", err)
	}
	var winResp struct {
		WindowID int `json:"windowId"`
		Bounds   struct {
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			Left        int    `json:"left"`
			Top         int    `json:"top"`
			WindowState string `json:"windowState"`
		} `json:"bounds"`
	}
	if err := json.Unmarshal(winRaw, &winResp); err != nil {
		return chromiumWindowBounds{}, fmt.Errorf("unmarshal window: %w", err)
	}
	return chromiumWindowBounds{
		WindowID:    winResp.WindowID,
		Width:       winResp.Bounds.Width,
		Height:      winResp.Bounds.Height,
		Left:        winResp.Bounds.Left,
		Top:         winResp.Bounds.Top,
		WindowState: winResp.Bounds.WindowState,
	}, nil
}

// viewportPredicate decides when a rendererViewport reading is "close enough"
// to the requested size. The headless and headful paths use different
// predicates because --headless=new uses Chrome's internal window size
// (screen.width/height) while headful renders into a real OS window owned by
// Mutter (outerWidth/outerHeight equal to the X root).
type viewportPredicate func(v rendererViewport, wantW, wantH int) bool

// waitForRendererViewport polls the renderer until the given predicate
// succeeds. Returns the matching viewport.
func waitForRendererViewport(t *testing.T, ctx context.Context, c *TestContainer, wantW, wantH int, pred viewportPredicate, label string, timeout time.Duration) rendererViewport {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last rendererViewport
	var lastErr error
	for {
		v, err := getRendererViewport(ctx, c)
		lastErr = err
		if err == nil {
			last = v
			if pred(v, wantW, wantH) {
				t.Logf("[%s] renderer_viewport: outer=%dx%d inner=%dx%d screen=%dx%d dpr=%d (matches want=%dx%d)",
					label, v.OuterWidth, v.OuterHeight, v.InnerWidth, v.InnerHeight, v.ScreenWidth, v.ScreenHght, v.DPR, wantW, wantH)
				return v
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("[%s] renderer viewport never matched want=%dx%d: last=%+v lastErr=%v", label, wantW, wantH, last, lastErr)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("[%s] context cancelled waiting for renderer viewport %dx%d: %v", label, wantW, wantH, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// headlessViewportPredicate asserts innerWidth/innerHeight match the request.
// In --headless=new the renderer uses Chrome's internal window
// (Emulation.setDeviceMetricsOverride drives inner*) — screen.* and outer*
// stay at Chrome's startup defaults and do not move on resize, so they are
// intentionally not asserted.
func headlessViewportPredicate(v rendererViewport, wantW, wantH int) bool {
	return v.InnerWidth == wantW && v.InnerHeight == wantH
}

// makeHeadfulMaximizedPredicate is the strict headful check: screen.*
// matches AND outerWidth/outerHeight is within tolerance of the request.
// Tolerance absorbs mutter chrome (borders/titlebar) — kiosk fullscreen has
// none, so tolerance 0 is correct there; --start-maximized passes through
// mutter SSDs so a small slack is kinder.
func makeHeadfulMaximizedPredicate(tolerance int) viewportPredicate {
	return func(v rendererViewport, wantW, wantH int) bool {
		if v.ScreenWidth != wantW || v.ScreenHght != wantH {
			return false
		}
		return abs(v.OuterWidth-wantW) <= tolerance && abs(v.OuterHeight-wantH) <= tolerance
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// resizeScenario describes a single image+chromium-config combination the
// resize tests exercise. extraEnv is applied at container start (e.g. to
// inject --kiosk into the default CHROMIUM_FLAGS). predicate decides when a
// renderer viewport reading is acceptable for this scenario.
type resizeScenario struct {
	name       string
	image      string
	extraEnv   map[string]string
	predicate  viewportPredicate
	skipReason string
	// assertXRoot toggles X-root convergence checks.
	assertXRoot bool
	// up/down override the default 1920x1080 → 1280x720 resize targets.
	// Headful with Neko already starts at 1920x1080 so the up resize must
	// pick a different size to actually exercise the screen change.
	up   [2]int
	down [2]int
	// requireBaselineState asserts the window's CDP windowState at startup
	// equals this value before any resize. Empty = no assertion.
	requireBaselineState string
}

func (sc resizeScenario) upSize() (int, int) {
	if sc.up != [2]int{} {
		return sc.up[0], sc.up[1]
	}
	return 1920, 1080
}

func (sc resizeScenario) downSize() (int, int) {
	if sc.down != [2]int{} {
		return sc.down[0], sc.down[1]
	}
	return 1280, 720
}

// TestDisplayResizeChromiumWindow exercises PATCH /display and asserts that
// after the resize the X root, the chromium OS window (CDP view), and the
// renderer's outer{Width,Height} all converge on the new size, with the
// window state (maximized / fullscreen) preserved across the resize and the
// CDP windowId stable (proving no chromium restart).
func TestDisplayResizeChromiumWindow(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	scenarios := []resizeScenario{
		{
			name:        "headless_default",
			image:       headlessImage,
			predicate:   headlessViewportPredicate,
			assertXRoot: true, // background Xvfb resize must converge
		},
		{
			// Production-equivalent scenario: --start-maximized + neko +
			// the CHROMIUM_FLAGS_DEFAULT mirror from run-docker.sh:17.
			// Uses the default restart_chromium (omitted from the
			// request), exercising the server's new behaviour: skip the
			// chromium restart and instead re-assert windowState=maximized
			// via Browser.setWindowBounds. The strict outer-matches-screen
			// predicate proves mutter reflows the maximized window on RANDR
			// without any chromium restart.
			name:  "headful_start_maximized",
			image: headfulImage,
			extraEnv: map[string]string{
				"ENABLE_WEBRTC":       "true",
				"NEKO_ADMIN_PASSWORD": "admin",
				// container.go:51 always appends --no-sandbox; the rest
				// mirrors images/chromium-headful/run-docker.sh:17.
				"CHROMIUM_FLAGS": "--user-data-dir=/home/kernel/user-data --disable-dev-shm-usage --disable-gpu --start-maximized --disable-software-rasterizer --remote-allow-origins=*",
			},
			predicate:            makeHeadfulMaximizedPredicate(20), // mutter borders/titlebar
			assertXRoot:          true,
			up:                   [2]int{2560, 1440},
			down:                 [2]int{1280, 720},
			requireBaselineState: "maximized",
		},
		{
			// --kiosk runs the window in fullscreen state. mutter reflows
			// the window to fill the new screen on every RANDR — verified
			// here on both up- and down-resize.
			name:                 "headful_kiosk",
			image:                headfulImage,
			extraEnv:             map[string]string{"ENABLE_WEBRTC": "true", "NEKO_ADMIN_PASSWORD": "admin", "CHROMIUM_FLAGS": "--kiosk --start-maximized"},
			predicate:            makeHeadfulMaximizedPredicate(0),
			assertXRoot:          true,
			up:                   [2]int{2560, 1440},
			down:                 [2]int{1280, 720},
			requireBaselineState: "fullscreen",
		},
		{
			// Non-Neko Xorg path: ENABLE_WEBRTC is unset, so the server
			// falls through to setResolutionXorgViaXrandr instead of
			// nekoAuthClient.ScreenConfigurationChange. Exercises the
			// `xrandr --output DUMMY0 --mode WxH_RR.00` path. Mode targets
			// must be ones xrandr has actually attached to DUMMY0 at the
			// requested refresh rate (`xrandr -q` on this image shows
			// 1920x1080_60.00 and 1280x720_60.00 but NOT 2560x1440_60.00).
			name:  "headful_xorg_no_neko",
			image: headfulImage,
			extraEnv: map[string]string{
				"CHROMIUM_FLAGS": "--user-data-dir=/home/kernel/user-data --disable-dev-shm-usage --disable-gpu --start-maximized --disable-software-rasterizer --remote-allow-origins=*",
			},
			predicate:            makeHeadfulMaximizedPredicate(20),
			assertXRoot:          true,
			up:                   [2]int{1920, 1080},
			down:                 [2]int{1280, 720},
			requireBaselineState: "maximized",
		},
	}

	// Subtests run serially: two of the three boot the heavy headful image
	// (neko + mutter + chromium) which races on startup mode under
	// concurrent load — observed: neko's `desktop.screen=1920x1080@25`
	// initialization fails to take effect when three privileged containers
	// boot at once, leaving the root at the dummy DDX's 3840x2160 default.
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if sc.skipReason != "" {
				t.Skip(sc.skipReason)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			env := map[string]string{
				"WIDTH":  "1024",
				"HEIGHT": "768",
			}
			for k, v := range sc.extraEnv {
				env[k] = v
			}

			c := NewTestContainer(t, sc.image)
			require.NoError(t, c.Start(ctx, ContainerConfig{Env: env}), "failed to start container")
			defer c.Stop(ctx)
			require.NoError(t, c.WaitReady(ctx), "api not ready")
			require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

			// Navigate to about:blank so playwright has a page to evaluate
			// against — otherwise the daemon may not have a target wired up.
			navigateBlank(t, ctx, c)

			// Baseline — log only. We don't assert specific initial
			// dimensions because the headless renderer's window size at
			// startup depends on Chrome's --headless=new defaults rather
			// than the Xvfb root we asked for.
			rootW, rootH, err := getXRootResolution(ctx, c)
			require.NoError(t, err, "baseline xrandr")
			t.Logf("[%s] baseline x_root=%dx%d", sc.name, rootW, rootH)

			baseline, err := getRendererViewport(ctx, c)
			require.NoError(t, err, "baseline renderer viewport")
			t.Logf("[%s] baseline renderer outer=%dx%d inner=%dx%d screen=%dx%d",
				sc.name, baseline.OuterWidth, baseline.OuterHeight, baseline.InnerWidth, baseline.InnerHeight, baseline.ScreenWidth, baseline.ScreenHght)

			baseCDP, err := getChromiumWindowBoundsCDP(ctx, c)
			require.NoError(t, err, "baseline cdp window")
			t.Logf("[%s] baseline cdp=%+v", sc.name, baseCDP)

			if sc.requireBaselineState != "" {
				require.Equal(t, sc.requireBaselineState, baseCDP.WindowState,
					"baseline windowState mismatch: chromium did not come up in the expected state — production behaviour relies on this invariant")
			}

			// Resize up: must be a real delta from the baseline so the X root
			// actually changes. Headful starts at Neko's default 1920x1080;
			// headless starts at the WIDTH/HEIGHT env (1024x768).
			upW, upH := sc.upSize()
			patchDisplayExpectingOK(t, ctx, c, upW, upH, 60)
			if sc.assertXRoot {
				waitForXRootResolution(t, ctx, c, upW, upH, 30*time.Second)
			}
			waitForRendererViewport(t, ctx, c, upW, upH, sc.predicate, sc.name+":up", 60*time.Second)

			cdpAfterUp, err := getChromiumWindowBoundsCDP(ctx, c)
			require.NoError(t, err, "cdp window after up-resize")
			t.Logf("[%s] after-up cdp=%+v", sc.name, cdpAfterUp)
			require.Equal(t, baseCDP.WindowID, cdpAfterUp.WindowID,
				"windowID changed after up-resize — chromium was restarted (windowID is monotonic per-process; a new one signals a relaunch)")
			require.Equal(t, baseCDP.WindowState, cdpAfterUp.WindowState,
				"windowState changed after up-resize — the WM-tracking invariant the no-restart path relies on was broken")

			// Resize back down to a smaller size, also a real delta.
			dnW, dnH := sc.downSize()
			patchDisplayExpectingOK(t, ctx, c, dnW, dnH, 60)
			if sc.assertXRoot {
				waitForXRootResolution(t, ctx, c, dnW, dnH, 30*time.Second)
			}
			waitForRendererViewport(t, ctx, c, dnW, dnH, sc.predicate, sc.name+":down", 60*time.Second)

			cdpAfterDown, err := getChromiumWindowBoundsCDP(ctx, c)
			require.NoError(t, err, "cdp window after down-resize")
			t.Logf("[%s] after-down cdp=%+v", sc.name, cdpAfterDown)
			require.Equal(t, baseCDP.WindowID, cdpAfterDown.WindowID,
				"windowID changed after down-resize — chromium was restarted between resizes")
			require.Equal(t, baseCDP.WindowState, cdpAfterDown.WindowState,
				"windowState changed after down-resize")
		})
	}
}


// navigateBlank points the active page at about:blank via playwright so the
// renderer is alive before we query window dimensions.
func navigateBlank(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()
	client, err := c.APIClient()
	require.NoError(t, err)
	timeout := 5
	rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightRequest{
		Code:       `await page.goto('about:blank'); return true;`,
		TimeoutSec: &timeout,
	})
	require.NoError(t, err)
	require.NotNil(t, rsp.JSON200, "playwright navigate response missing")
	require.True(t, rsp.JSON200.Success, "playwright navigate to about:blank failed: %s", string(rsp.Body))
}

// patchDisplayExpectingOK issues PATCH /display and requires a 200. The
// request omits restart_chromium so the server picks its own default — the
// CDP re-assert-maximized path on the Xorg branch.
//
// refreshRate is required for headful: the dummy Xorg DDX only has modelines
// named "WxH_RR.00", and the server's xrandr fallback (`xrandr -s WxH`)
// silently no-ops when refresh rate is omitted.
func patchDisplayExpectingOK(t *testing.T, ctx context.Context, c *TestContainer, width, height, refreshRate int) {
	t.Helper()
	client, err := c.APIClient()
	require.NoError(t, err)
	rate := instanceoapi.PatchDisplayRequestRefreshRate(refreshRate)
	req := instanceoapi.PatchDisplayJSONRequestBody{
		Width:       &width,
		Height:      &height,
		RefreshRate: &rate,
	}
	rsp, err := client.PatchDisplayWithResponse(ctx, req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "PATCH /display: %s body=%s", rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200)
	require.NotNil(t, rsp.JSON200.Width)
	require.NotNil(t, rsp.JSON200.Height)
	require.Equal(t, width, *rsp.JSON200.Width)
	require.Equal(t, height, *rsp.JSON200.Height)
}

// waitForXRootResolution polls xrandr until the X root reaches the requested
// size, mirroring waitForXvfbResolution but operating on the live X root
// instead of the Xvfb process command line.
func waitForXRootResolution(t *testing.T, ctx context.Context, c *TestContainer, wantW, wantH int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		w, h, err := getXRootResolution(ctx, c)
		if err == nil && w == wantW && h == wantH {
			t.Logf("x_root_resolution: %dx%d (matches)", w, h)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("x root never reached %dx%d: lastW=%d lastH=%d err=%v", wantW, wantH, w, h, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled waiting for x root %dx%d: %v", wantW, wantH, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
