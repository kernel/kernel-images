package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	nekooapi "github.com/m1k1o/neko/server/lib/oapi"
)

// PatchDisplay updates the display configuration. When require_idle
// is true (default), it refuses to resize while live view or recording/replay is active.
// This method automatically detects whether the system is running with Xorg (headful)
// or Xvfb (headless) and uses the appropriate method to change resolution.
func (s *ApiService) PatchDisplay(ctx context.Context, req oapi.PatchDisplayRequestObject) (oapi.PatchDisplayResponseObject, error) {
	log := logger.FromContext(ctx)

	if req.Body == nil {
		return oapi.PatchDisplay400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "missing request body"}}, nil
	}

	// Check if resolution change is requested
	if req.Body.Width == nil && req.Body.Height == nil {
		return oapi.PatchDisplay400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "no display parameters to update"}}, nil
	}

	// Get current resolution with refresh rate
	currentWidth, currentHeight, currentRefreshRate, err := s.getCurrentResolution(ctx)
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return oapi.PatchDisplay500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to get current display resolution"}}, nil
	}
	width, height, refreshRate, changed := resolveDisplayParams(req.Body, currentWidth, currentHeight, currentRefreshRate)

	if width <= 0 || height <= 0 {
		return oapi.PatchDisplay400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid width/height"}}, nil
	}

	// Display already matches the requested state. Skip the idle check,
	// recording stop, resize, and chromium restart — all of which would be
	// no-ops at the existing resolution.
	if !changed {
		log.Info("display already at requested resolution, skipping resize", "width", width, "height", height, "refreshRate", refreshRate)
		return oapi.PatchDisplay200JSONResponse{
			Width:       &width,
			Height:      &height,
			RefreshRate: &refreshRate,
		}, nil
	}

	log.Info(fmt.Sprintf("resolution change requested from %dx%d@%d to %dx%d@%d", currentWidth, currentHeight, currentRefreshRate, width, height, refreshRate))

	// Parse requireIdle flag (default true)
	requireIdle := true
	if req.Body.RequireIdle != nil {
		requireIdle = *req.Body.RequireIdle
	}

	// Check if resize is safe (no active sessions or recordings)
	if requireIdle {
		live := s.getActiveNekoSessions(ctx)
		isRecording := s.anyRecordingActive(ctx)
		resizableNow := (live == 0) && !isRecording

		log.Info("checking if resize is safe", "live_sessions", live, "is_recording", isRecording, "resizable", resizableNow)

		if !resizableNow {
			return oapi.PatchDisplay409JSONResponse{
				ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{
					Message: "resize refused: live view or recording/replay active",
				},
			}, nil
		}
	}

	// Gracefully stop active recordings so the resize can proceed.
	// Recordings are always restarted (via defer) regardless of whether the
	// resize succeeds — losing recording data is worse than a brief gap. If
	// the resize fails the display is still at the old resolution, so
	// restarting at the "old" resolution is correct.
	stopped, stopErr := s.stopActiveRecordings(ctx)
	if stopErr != nil {
		log.Error("failed to stop recordings for resize", "error", stopErr)
		return oapi.PatchDisplay500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
				Message: fmt.Sprintf("failed to stop recordings for resize: %s", stopErr),
			},
		}, nil
	}
	if len(stopped) > 0 {
		defer func() {
			go s.startNewRecordingSegments(context.WithoutCancel(ctx), stopped)
		}()
	}

	// Detect display mode (xorg or xvfb)
	displayMode := s.detectDisplayMode(ctx)

	// Parse restartChromium flag (default depends on mode)
	restartChrome := false // default false for both modes
	if req.Body.RestartChromium != nil {
		restartChrome = *req.Body.RestartChromium
	}

	// Route to appropriate resolution change handler
	if displayMode == "xorg" {
		if s.isNekoEnabled() {
			log.Info("using Neko API for Xorg resolution change")
			err = s.setResolutionViaNeko(ctx, width, height, refreshRate)
		} else {
			log.Info("using xrandr for Xorg resolution change (Neko disabled)")
			err = s.setResolutionXorgViaXrandr(ctx, width, height, refreshRate)
		}
		// Re-assert the maximized window state via CDP, then verify the
		// X root has reached the requested size. Mutter reflows a
		// maximized (or fullscreen) window onto the new root
		// automatically, so the CDP call's only job is to make sure the
		// window is in the state that triggers the reflow. The X root
		// poll is the authoritative post-condition: it's the value the
		// server actually set and stays panel-robust if mutter ever
		// gains a taskbar (a maximized window would then be smaller
		// than the root by the panel's reserved space).
		//
		// Both are fatal on failure — returning 200 with the X root
		// still at the old size, or the window stuck in normal state,
		// would leave the caller with no signal of the mismatch. The
		// previous approach of restarting chromium so it could re-apply
		// --start-maximized had the same effective contract (the
		// restart blocked the response) but cost ~9s per resize and
		// wiped browser-side state (Emulation.* overrides, devtools
		// sessions). The restart_chromium request field is still
		// accepted for API compatibility but no longer triggers a
		// restart on this path.
		if err == nil {
			// Wait for the X root to settle, then use it as the
			// realized size in the response. The wait returns early
			// when either (a) xrandr reports the requested size — the
			// common case — or (b) consecutive reads are stable for a
			// short window, capturing libxcvt's rounded size on
			// requests it can't honour exactly (CVT 8-pixel grid +
			// FWXGA bump for 1360×768 → 1366×768).
			//
			// The poll absorbs transient X root states — chromium
			// running in --kiosk briefly pushes the root to the dummy
			// DDX's max mode (3840×2160) while mutter settles on the
			// new screen, and a single immediate read would catch that
			// transient instead of the steady-state size.
			realizedW, realizedH := s.waitForXRootRealized(ctx, width, height, 10*time.Second)
			if realizedW > 0 && realizedH > 0 {
				if realizedW != width || realizedH != height {
					log.Info("X root differs from request after resize",
						"requested", fmt.Sprintf("%dx%d", width, height),
						"realized", fmt.Sprintf("%dx%d", realizedW, realizedH))
				}
				width, height = realizedW, realizedH
			} else {
				log.Warn("X root never read successfully after resize, returning requested dimensions")
			}
		}
		if err == nil {
			if cdpErr := s.setWindowMaximizedViaCDP(ctx); cdpErr != nil {
				log.Error("CDP maximize re-assert failed after Xorg resolution change", "error", cdpErr)
				err = fmt.Errorf("CDP maximize re-assert failed: %w", cdpErr)
			}
		}
	} else if len(stopped) > 0 {
		// Recordings were active when this request arrived (now temporarily
		// stopped). Resize Xvfb synchronously so the deferred
		// startNewRecordingSegments captures at the correct resolution.
		// Acquire xvfbResizeMu to wait for any in-flight background resize.
		log.Info("recordings were active, using synchronous Xvfb restart for resolution change")
		s.xvfbResizeMu.Lock()
		err = s.resizeXvfb(ctx, width, height)
		if err == nil {
			s.clearViewportOverride()
			s.recordHeadlessRefreshRate(refreshRate)
		}
		s.xvfbResizeMu.Unlock()
		if err == nil {
			if cdpErr := s.setViewportViaCDP(ctx, width, height); cdpErr != nil {
				log.Warn("CDP viewport resize failed after Xvfb restart (non-fatal)", "error", cdpErr)
			}
		}
		if err == nil && restartChrome {
			if restartErr := s.restartChromiumAndWait(ctx, "resolution change"); restartErr != nil {
				log.Error("failed to restart chromium after resolution change", "error", restartErr)
			}
		}
	} else {
		// Fast path: no recording active. Resize the browser viewport via CDP
		// (instant) and update Xvfb in the background for future recordings.
		log.Info("using CDP fast path for headless viewport resize")
		err = s.setViewportViaCDP(ctx, width, height)
		if err == nil {
			s.setViewportOverride(width, height, refreshRate)
			go s.backgroundResizeXvfb(context.WithoutCancel(ctx), width, height)
		}
	}

	if err != nil {
		log.Error("failed to change resolution", "error", err)
		return oapi.PatchDisplay500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
				Message: fmt.Sprintf("failed to change resolution: %s", err.Error()),
			},
		}, nil
	}

	// Return success with the new dimensions
	return oapi.PatchDisplay200JSONResponse{
		Width:       &width,
		Height:      &height,
		RefreshRate: &refreshRate,
	}, nil
}

// resolveDisplayParams merges the request body with the current display
// state, returning the final width, height, and refresh rate plus whether
// any field would actually change. Callers use the changed flag to skip
// the resize path when the request is a no-op.
func resolveDisplayParams(body *oapi.PatchDisplayJSONRequestBody, currentWidth, currentHeight, currentRefreshRate int) (width, height, refreshRate int, changed bool) {
	width, height, refreshRate = currentWidth, currentHeight, currentRefreshRate
	if body == nil {
		return
	}
	if body.Width != nil {
		width = *body.Width
	}
	if body.Height != nil {
		height = *body.Height
	}
	if body.RefreshRate != nil {
		refreshRate = int(*body.RefreshRate)
	}
	changed = width != currentWidth || height != currentHeight || refreshRate != currentRefreshRate
	return
}

// detectDisplayMode detects whether we're running Xorg (headful) or Xvfb
// (headless). The result is cached because the display server type does not
// change during the container's lifetime, and querying supervisorctl during
// a background Xvfb restart can produce false negatives.
func (s *ApiService) detectDisplayMode(ctx context.Context) string {
	s.displayModeOnce.Do(func() {
		s.displayModeVal = s.probeDisplayMode(ctx)
	})
	return s.displayModeVal
}

var xvfbSupervisorConf = "/etc/supervisor/conf.d/services/xvfb.conf"

func (s *ApiService) probeDisplayMode(ctx context.Context) string {
	log := logger.FromContext(ctx)
	if _, err := os.Stat(xvfbSupervisorConf); err == nil {
		log.Info("detected Xvfb display (headless mode)", "marker", xvfbSupervisorConf)
		return "xvfb"
	}
	log.Info("detected Xorg display (headful mode)")
	return "xorg"
}

// setResolutionXorgViaXrandr changes resolution for Xorg using xrandr (fallback when Neko is disabled)
func (s *ApiService) setResolutionXorgViaXrandr(ctx context.Context, width, height, refreshRate int) error {
	log := logger.FromContext(ctx)
	display := s.resolveDisplayFromEnv()

	// The headful Xorg dummy driver exposes DUMMY0, not "default". The
	// historical `xrandr --output default --mode ...` command exits 0 while
	// silently doing nothing on this driver. Default to DUMMY0 and let an
	// env var override it for any non-standard image layout.
	output := strings.TrimSpace(os.Getenv("KERNEL_IMAGES_XRANDR_OUTPUT"))
	if output == "" {
		output = "DUMMY0"
	}

	// Per-output resizing requires --mode <name>; --size is a legacy global
	// screen option that cannot be combined with --output. Always go through
	// a named modeline. The schema enum prevents callers from sending zero,
	// and getCurrentResolution falls back to 60 when xrandr is silent
	// (Xvfb), but normalize defensively in case either guarantee changes.
	if refreshRate <= 0 {
		refreshRate = 60
	}
	modeName := fmt.Sprintf("%dx%d_%d.00", width, height, refreshRate)
	xrandrCmd := fmt.Sprintf("xrandr --output %s --mode %s", output, modeName)
	log.Info("using specific modeline", "output", output, "mode", modeName)

	args := []string{"-lc", xrandrCmd}
	env := map[string]string{"DISPLAY": display}
	execReq := oapi.ProcessExecRequest{Command: "bash", Args: &args, Env: &env}
	resp, err := s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &execReq})
	if err != nil {
		return fmt.Errorf("failed to execute xrandr: %w", err)
	}

	switch r := resp.(type) {
	case oapi.ProcessExec200JSONResponse:
		if r.ExitCode != nil && *r.ExitCode != 0 {
			var stderr string
			if r.StderrB64 != nil {
				if b, decErr := base64.StdEncoding.DecodeString(*r.StderrB64); decErr == nil {
					stderr = strings.TrimSpace(string(b))
				}
			}
			if stderr == "" {
				stderr = "xrandr returned non-zero exit code"
			}
			return fmt.Errorf("xrandr failed: %s", stderr)
		}
		log.Info("resolution updated via xrandr", "display", display, "width", width, "height", height)
		return nil
	case oapi.ProcessExec400JSONResponse:
		return fmt.Errorf("bad request: %s", r.Message)
	case oapi.ProcessExec500JSONResponse:
		return fmt.Errorf("internal error: %s", r.Message)
	default:
		return fmt.Errorf("unexpected response from process exec")
	}
}

// resizeXvfb updates the Xvfb supervisor config and restarts the Xvfb process
// at the new resolution. It does NOT restart Chromium.
func (s *ApiService) resizeXvfb(ctx context.Context, width, height int) error {
	log := logger.FromContext(ctx)
	log.Info("updating Xvfb resolution requires restart", "width", width, "height", height)

	// Update supervisor config to include environment variables
	log.Info("updating xvfb supervisor config with new dimensions")
	removeEnvCmd := []string{"-lc", `sed -i '/^environment=/d' /etc/supervisor/conf.d/services/xvfb.conf`}
	removeEnvReq := oapi.ProcessExecRequest{Command: "bash", Args: &removeEnvCmd}
	s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &removeEnvReq})

	// Add the environment line with WIDTH and HEIGHT
	addEnvCmd := []string{"-lc", fmt.Sprintf(`sed -i '/\[program:xvfb\]/a environment=WIDTH="%d",HEIGHT="%d",DPI="96",DISPLAY=":1"' /etc/supervisor/conf.d/services/xvfb.conf`, width, height)}
	addEnvReq := oapi.ProcessExecRequest{Command: "bash", Args: &addEnvCmd}
	configResp, configErr := s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &addEnvReq})
	if configErr != nil {
		return fmt.Errorf("failed to update xvfb config: %w", configErr)
	}

	// Check if config update succeeded
	if execResp, ok := configResp.(oapi.ProcessExec200JSONResponse); ok {
		if execResp.ExitCode != nil && *execResp.ExitCode != 0 {
			log.Error("failed to update xvfb config", "exit_code", *execResp.ExitCode)
			return fmt.Errorf("failed to update xvfb config")
		}
	}

	// Reload supervisor configuration
	log.Info("reloading supervisor configuration")
	reloadCmd := []string{"-lc", "supervisorctl reread && supervisorctl update"}
	reloadReq := oapi.ProcessExecRequest{Command: "bash", Args: &reloadCmd}
	if _, err := s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &reloadReq}); err != nil {
		log.Error("failed to reload supervisor config", "error", err)
	}

	// Restart xvfb with new configuration
	log.Info("restarting xvfb with new resolution")
	restartXvfbCmd := []string{"-lc", "supervisorctl restart xvfb"}
	restartXvfbReq := oapi.ProcessExecRequest{Command: "bash", Args: &restartXvfbCmd}
	xvfbResp, xvfbErr := s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &restartXvfbReq})
	if xvfbErr != nil {
		return fmt.Errorf("failed to restart Xvfb: %w", xvfbErr)
	}

	// Check if Xvfb restart succeeded
	if execResp, ok := xvfbResp.(oapi.ProcessExec200JSONResponse); ok {
		if execResp.ExitCode != nil && *execResp.ExitCode != 0 {
			return fmt.Errorf("Xvfb restart failed")
		}
	}

	// Wait for Xvfb to be ready
	log.Info("waiting for Xvfb to be ready")
	waitCmd := []string{"-lc", "sleep 2"}
	waitReq := oapi.ProcessExecRequest{Command: "bash", Args: &waitCmd}
	s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &waitReq})

	log.Info("Xvfb resolution updated", "width", width, "height", height)
	return nil
}

// backgroundResizeXvfb serializes background Xvfb restarts. After acquiring
// the lock, it checks whether the current viewport override still matches the
// requested dimensions. If a newer resize has superseded this one, the resize
// is skipped so Xvfb always converges to the latest requested size.
func (s *ApiService) backgroundResizeXvfb(ctx context.Context, width, height int) {
	s.xvfbResizeMu.Lock()
	defer s.xvfbResizeMu.Unlock()

	log := logger.FromContext(ctx)

	s.viewportMu.RLock()
	override := s.viewportOverride
	s.viewportMu.RUnlock()
	if override == nil {
		log.Info("skipping background Xvfb resize: override cleared (synchronous path handled it)", "requested", fmt.Sprintf("%dx%d", width, height))
		return
	}
	if override[0] != width || override[1] != height {
		log.Info("skipping stale background Xvfb resize", "requested", fmt.Sprintf("%dx%d", width, height), "current", fmt.Sprintf("%dx%d", override[0], override[1]))
		return
	}

	if xvfbErr := s.resizeXvfb(ctx, width, height); xvfbErr != nil {
		log.Warn("background Xvfb resize failed (non-fatal), keeping viewport override", "error", xvfbErr)
		return
	}

	s.viewportMu.Lock()
	if s.viewportOverride != nil && s.viewportOverride[0] == width && s.viewportOverride[1] == height {
		s.viewportOverride = nil
	}
	s.viewportMu.Unlock()
}

// withCDPClient dials the current devtools upstream with a 10s timeout,
// hands the connected client to fn, and closes the connection on return.
// Lets the small per-call CDP helpers below avoid duplicating the dial +
// timeout + defer-close scaffolding.
func (s *ApiService) withCDPClient(ctx context.Context, fn func(context.Context, *cdpclient.Client) error) error {
	upstreamURL := s.upstreamMgr.Current()
	if upstreamURL == "" {
		return fmt.Errorf("devtools upstream not available")
	}
	cdpCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := cdpclient.Dial(cdpCtx, upstreamURL)
	if err != nil {
		return fmt.Errorf("failed to connect to devtools: %w", err)
	}
	defer client.Close()
	return fn(cdpCtx, client)
}

// setWindowMaximizedViaCDP re-asserts that the chromium OS window is in
// the "maximized" state via Browser.setWindowBounds. After a successful
// xrandr/Neko resize, mutter reflows a maximized (or fullscreen) window
// to fill the new root automatically — this call ensures the window is
// in the state that triggers the reflow. The CDP call is idempotent: it
// no-ops on a window already in maximized or fullscreen state.
//
// The PATCH /display handler reads the X root separately (via
// getCurrentResolutionFromXrandr) to capture the realized size for the
// response, rather than reading the chromium window's own bounds. That
// keeps the contract panel-robust: if mutter ever gained a taskbar/dock,
// a maximized window would be smaller than the root by the panel's
// reserved space, and any window-bounds-based check would diverge from
// the X root.
func (s *ApiService) setWindowMaximizedViaCDP(ctx context.Context) error {
	log := logger.FromContext(ctx)
	if err := s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		return client.SetWindowBoundsMaximized(cdpCtx)
	}); err != nil {
		return fmt.Errorf("CDP setWindowBoundsMaximized: %w", err)
	}
	log.Info("re-asserted maximized window state via CDP")
	return nil
}

// waitForXRootRealized polls the X root via xrandr after a resize and
// returns the realized dimensions. It returns early on either of two
// success conditions:
//
//  1. The reading matches the requested width/height — the common case,
//     when libxcvt honoured the request exactly.
//  2. The reading has been stable across stableReads consecutive samples
//     AT A VALUE CLOSE TO THE REQUEST — captures libxcvt's rounded size
//     on requests it can't honour exactly (CVT 8-pixel grid round +
//     FWXGA bump for 1360×768 → 1366×768) and covers the idempotent
//     re-PATCH case.
//
// The "close to request" guard on the stable-N path rejects transient
// xrandr readings far from the request — chromium running in
// --start-maximized or --kiosk briefly drives xrandr to report the dummy
// DDX's max mode (e.g. 3840×2160) while mode-switch propagates, and a
// naive stable-N would echo that transient into the response body. Real
// libxcvt rounding is <16 px; the dummy max is >1000 px off any normal
// request — acceptableDelta=32 sits comfortably between them.
//
// If neither condition fires before the timeout, returns the last
// observation. Always non-fatal — the response always echoes some size,
// never 500s.
//
// Calls getCurrentResolutionFromXrandr directly rather than the higher-
// level getCurrentResolution: the latter prefers a cached viewportOverride
// when one is set, which would silently report the CDP viewport instead
// of the X root. The override is only set on the headless Xvfb fast path
// today, so the Xorg branch never reaches that case — but the invariant
// is non-local and would silently regress if anyone ever sets the
// override on the Xorg path.
func (s *ApiService) waitForXRootRealized(ctx context.Context, wantW, wantH int, timeout time.Duration) (int, int) {
	const stableReads = 3
	const acceptableDelta = 32
	deadline := time.Now().Add(timeout)
	var lastW, lastH int
	var stableCount int
	for {
		w, h, _, _, err := s.getCurrentResolutionFromXrandr(ctx)
		if err == nil {
			if w == wantW && h == wantH {
				return w, h
			}
			if w == lastW && h == lastH {
				stableCount++
				if stableCount >= stableReads && abs(w-wantW) <= acceptableDelta && abs(h-wantH) <= acceptableDelta {
					return w, h
				}
			} else {
				stableCount = 1
				lastW, lastH = w, h
			}
		}
		if time.Now().After(deadline) {
			return lastW, lastH
		}
		select {
		case <-ctx.Done():
			return lastW, lastH
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// setViewportViaCDP resizes the browser viewport using the CDP
// Emulation.setDeviceMetricsOverride command. This is near-instant and does
// not require restarting Chromium or Xvfb.
func (s *ApiService) setViewportViaCDP(ctx context.Context, width, height int) error {
	log := logger.FromContext(ctx)
	if err := s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		return client.SetDeviceMetricsOverride(cdpCtx, width, height)
	}); err != nil {
		return fmt.Errorf("CDP setDeviceMetricsOverride: %w", err)
	}
	log.Info("viewport resized via CDP", "width", width, "height", height)
	return nil
}

// anyRecordingActive returns true if any registered recorder is currently recording.
func (s *ApiService) anyRecordingActive(ctx context.Context) bool {
	for _, r := range s.recordManager.ListActiveRecorders(ctx) {
		if r.IsRecording(ctx) {
			return true
		}
	}
	return false
}

// getActiveNekoSessions queries the Neko API for active viewer sessions.
func (s *ApiService) getActiveNekoSessions(ctx context.Context) int {
	log := logger.FromContext(ctx)

	// Query sessions using authenticated client
	sessions, err := s.nekoAuthClient.SessionsGet(ctx)
	if err != nil {
		log.Debug("failed to query Neko sessions", "error", err)
		return 0
	}

	// Count active sessions (connected and watching)
	live := 0
	for i, session := range sessions {
		log.Info("neko session details", "index", i, "session", session)
		if session.State != nil {
			connected := session.State.IsConnected != nil && *session.State.IsConnected
			watching := session.State.IsWatching != nil && *session.State.IsWatching
			if connected && watching {
				live++
			}
		}
	}

	log.Info("successfully queried Neko API", "active_sessions", live)
	return live
}

// resolveDisplayFromEnv returns the X display string, defaulting to ":1".
func (s *ApiService) resolveDisplayFromEnv() string {
	// Prefer KERNEL_IMAGES_API_DISPLAY_NUM, fallback to DISPLAY_NUM, default 1
	if v := strings.TrimSpace(os.Getenv("KERNEL_IMAGES_API_DISPLAY_NUM")); v != "" {
		return ":" + v
	}
	if v := strings.TrimSpace(os.Getenv("DISPLAY_NUM")); v != "" {
		return ":" + v
	}
	return ":1"
}

// setViewportOverride stores the last-known viewport dimensions so
// getCurrentResolution can return them even while Xvfb is restarting.
// It also records the refresh rate as the sticky headless value.
func (s *ApiService) setViewportOverride(width, height, refreshRate int) {
	s.viewportMu.Lock()
	s.viewportOverride = &[3]int{width, height, refreshRate}
	if refreshRate > 0 {
		s.lastHeadlessRefreshRate = refreshRate
	}
	s.viewportMu.Unlock()
}

// clearViewportOverride removes the viewport override (e.g. after Xvfb
// finishes restarting and xrandr is reliable again). The sticky
// lastHeadlessRefreshRate is intentionally preserved so that
// getCurrentResolution can still return the requested refresh rate after
// xrandr stops surfacing it.
func (s *ApiService) clearViewportOverride() {
	s.viewportMu.Lock()
	s.viewportOverride = nil
	s.viewportMu.Unlock()
}

// recordHeadlessRefreshRate stores the last refresh rate applied via a
// headless path so identical follow-up requests are detected as no-ops.
// Zero values are ignored — they signal "rate not specified".
func (s *ApiService) recordHeadlessRefreshRate(refreshRate int) {
	if refreshRate <= 0 {
		return
	}
	s.viewportMu.Lock()
	s.lastHeadlessRefreshRate = refreshRate
	s.viewportMu.Unlock()
}

// getCurrentResolution returns the current display resolution and refresh
// rate. If a viewport override is set (from a recent CDP resize while Xvfb
// restarts in the background), it returns the override instead of querying
// xrandr, which may fail during Xvfb restarts. When xrandr does not surface
// a refresh rate (Xvfb), the last-applied headless rate is used so repeat
// requests at the same rate are detected as no-ops.
func (s *ApiService) getCurrentResolution(ctx context.Context) (int, int, int, error) {
	s.viewportMu.RLock()
	override := s.viewportOverride
	stickyRate := s.lastHeadlessRefreshRate
	s.viewportMu.RUnlock()
	if override != nil {
		return override[0], override[1], override[2], nil
	}

	w, h, rr, rateFromXrandr, err := s.getCurrentResolutionFromXrandr(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	if !rateFromXrandr && stickyRate > 0 {
		rr = stickyRate
	}
	return w, h, rr, nil
}

// getCurrentResolutionFromXrandr queries xrandr for the current display
// resolution. The fourth return value reports whether the refresh rate was
// parsed from xrandr output (true) or is the synthesized fallback (false);
// callers can use it to prefer a previously-recorded rate when xrandr is
// silent, as Xvfb is.
func (s *ApiService) getCurrentResolutionFromXrandr(ctx context.Context) (int, int, int, bool, error) {
	log := logger.FromContext(ctx)
	display := s.resolveDisplayFromEnv()

	// Use xrandr to get current resolution
	// Note: Using bash -c (not -lc) to avoid login shell overriding DISPLAY env var
	cmd := exec.CommandContext(ctx, "bash", "-c", "xrandr | grep -E '\\*' | awk '{print $1}'")
	cmd.Env = append(os.Environ(), fmt.Sprintf("DISPLAY=%s", display))

	out, err := cmd.Output()
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return 0, 0, 0, false, fmt.Errorf("failed to execute xrandr command: %w", err)
	}

	resStr := strings.TrimSpace(string(out))
	parts := strings.Split(resStr, "x")
	if len(parts) != 2 {
		log.Error("unexpected xrandr output format", "output", resStr)
		return 0, 0, 0, false, fmt.Errorf("unexpected xrandr output format: %s", resStr)
	}

	width, err := strconv.Atoi(parts[0])
	if err != nil {
		log.Error("failed to parse width", "error", err, "value", parts[0])
		return 0, 0, 0, false, fmt.Errorf("failed to parse width '%s': %w", parts[0], err)
	}

	// Parse height and refresh rate (e.g., "1080_60.00" -> height=1080, rate=60)
	heightStr := parts[1]
	refreshRate := 60 // default when xrandr omits the _rate suffix (e.g. Xvfb)
	rateFromXrandr := false
	if idx := strings.Index(heightStr, "_"); idx != -1 {
		rateStr := heightStr[idx+1:]
		heightStr = heightStr[:idx]
		// Parse the refresh rate (e.g., "60.00" -> 60)
		if rateFloat, err := strconv.ParseFloat(rateStr, 64); err == nil {
			refreshRate = int(rateFloat)
			rateFromXrandr = true
		}
	}

	height, err := strconv.Atoi(heightStr)
	if err != nil {
		log.Error("failed to parse height", "error", err, "value", heightStr)
		return 0, 0, 0, false, fmt.Errorf("failed to parse height '%s': %w", heightStr, err)
	}

	return width, height, refreshRate, rateFromXrandr, nil
}

// stoppedRecordingInfo holds state captured from a recording that was stopped
// so it can be restarted after a display resize.
type stoppedRecordingInfo struct {
	id       string
	params   recorder.FFmpegRecordingParams
	metadata *recorder.RecordingMetadata
}

// stopActiveRecordings gracefully stops every recording that is currently in
// progress. The old recorders remain registered in the manager so their
// finalized files stay discoverable and downloadable. It returns info needed
// to start a new recording segment for each stopped recorder.
func (s *ApiService) stopActiveRecordings(ctx context.Context) ([]stoppedRecordingInfo, error) {
	log := logger.FromContext(ctx)
	var stopped []stoppedRecordingInfo

	for _, rec := range s.recordManager.ListActiveRecorders(ctx) {
		if !rec.IsRecording(ctx) {
			continue
		}

		id := rec.ID()

		ffmpegRec, ok := rec.(*recorder.FFmpegRecorder)
		if !ok {
			log.Warn("cannot capture params from non-FFmpeg recorder, skipping", "id", id)
			continue
		}

		params := ffmpegRec.Params()

		log.Info("stopping recording for resize", "id", id)
		if err := rec.Stop(ctx); err != nil {
			// Stop() returns finalization errors even when the process was
			// successfully terminated. Only treat it as a hard failure if
			// the process is still running.
			if rec.IsRecording(ctx) {
				log.Error("failed to stop recording for resize", "id", id, "error", err)
				return stopped, fmt.Errorf("failed to stop recording %s: %w", id, err)
			}
			log.Warn("recording stopped with finalization warning", "id", id, "error", err)
		}

		stopped = append(stopped, stoppedRecordingInfo{
			id:       id,
			params:   params,
			metadata: rec.Metadata(),
		})
		log.Info("recording stopped for resize, old segment preserved", "id", id)
	}

	return stopped, nil
}

// adjustParamsForRemainingBudget reduces MaxDurationInSeconds and MaxSizeInMB
// in the cloned params to reflect what the previous segment already consumed.
// This keeps cumulative duration and disk usage within the originally requested limits.
func adjustParamsForRemainingBudget(log *slog.Logger, info stoppedRecordingInfo) recorder.FFmpegRecordingParams {
	params := info.params

	if params.MaxDurationInSeconds != nil && info.metadata != nil && !info.metadata.EndTime.IsZero() {
		elapsed := int(info.metadata.EndTime.Sub(info.metadata.StartTime).Seconds())
		remaining := *params.MaxDurationInSeconds - elapsed
		if remaining < 1 {
			remaining = 1
		}
		params.MaxDurationInSeconds = &remaining
		log.Info("adjusted max duration for new segment", "id", info.id, "elapsed_s", elapsed, "remaining_s", remaining)
	}

	if params.MaxSizeInMB != nil && params.OutputDir != nil {
		segmentPath := filepath.Join(*params.OutputDir, info.id+".mp4")
		if fi, err := os.Stat(segmentPath); err == nil {
			consumedMB := int((fi.Size() + 1024*1024 - 1) / (1024 * 1024))
			remaining := *params.MaxSizeInMB - consumedMB
			if remaining < 1 {
				remaining = 1
			}
			params.MaxSizeInMB = &remaining
			log.Info("adjusted max size for new segment", "id", info.id, "consumed_mb", consumedMB, "remaining_mb", remaining)
		}
	}

	return params
}

// startNewRecordingSegments creates and starts a new recording segment for
// each previously-stopped recorder. Each new segment gets a unique suffixed
// ID so the old (stopped) recorder and its finalized file remain accessible
// in the manager.
//
// Duration and size limits are adjusted to account for what the previous
// segment already consumed, so the cumulative totals stay within the
// originally requested bounds.
func (s *ApiService) startNewRecordingSegments(ctx context.Context, stopped []stoppedRecordingInfo) {
	log := logger.FromContext(ctx)

	for _, info := range stopped {
		newID := fmt.Sprintf("%s-%d", info.id, time.Now().UnixMilli())

		params := adjustParamsForRemainingBudget(log, info)

		rec, err := s.factory(newID, params)
		if err != nil {
			log.Error("failed to create recorder for new segment", "old_id", info.id, "new_id", newID, "error", err)
			continue
		}

		if err := s.recordManager.RegisterRecorder(ctx, rec); err != nil {
			log.Error("failed to register new segment recorder", "old_id", info.id, "new_id", newID, "error", err)
			continue
		}

		if err := rec.Start(ctx); err != nil {
			log.Error("failed to start new segment recording", "old_id", info.id, "new_id", newID, "error", err)
			_ = s.recordManager.DeregisterRecorder(ctx, rec)
			continue
		}

		log.Info("new recording segment started after resize", "old_id", info.id, "new_id", newID)
	}
}

// isNekoEnabled checks if Neko service is enabled
func (s *ApiService) isNekoEnabled() bool {
	return os.Getenv("ENABLE_WEBRTC") == "true"
}

// setResolutionViaNeko delegates resolution change to Neko API. The
// realized X root dimensions can differ from the request — libxcvt rounds
// widths to the CVT 8-pixel grid and applies a FWXGA bump for
// 1360×768 → 1366×768. Neko's HTTP response echoes the request, not the
// realized size, so the PatchDisplay handler reads the X root via xrandr
// after this call returns.
func (s *ApiService) setResolutionViaNeko(ctx context.Context, width, height, refreshRate int) error {
	log := logger.FromContext(ctx)

	// Use default refresh rate if not specified
	if refreshRate <= 0 {
		refreshRate = 60
	}

	// Prepare screen configuration
	screenConfig := nekooapi.ScreenConfiguration{
		Width:  &width,
		Height: &height,
		Rate:   &refreshRate,
	}

	// Change screen configuration using authenticated client
	if err := s.nekoAuthClient.ScreenConfigurationChange(ctx, screenConfig); err != nil {
		return fmt.Errorf("failed to change screen configuration: %w", err)
	}

	log.Info("successfully changed resolution via Neko API", "width", width, "height", height, "refresh_rate", refreshRate)
	return nil
}
