package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

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

// setViewportViaCDP resizes the browser viewport using the CDP
// Browser.setContentsSize command. This is near-instant and does not require
// restarting Chromium or Xvfb. When Chromium refuses to resize the window (a
// fullscreen window, or a CDP error), it falls back to
// Emulation.setDeviceMetricsOverride, which Chromium 154+ clears as soon as
// this client detaches, so on those versions the viewport stays as it was.
func (s *ApiService) setViewportViaCDP(ctx context.Context, width, height int) error {
	log := logger.FromContext(ctx)
	if err := s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		err := client.SetContentsSize(cdpCtx, width, height)
		if !contentsSizeRefused(err) {
			return err
		}
		log.Warn("CDP setContentsSize refused, falling back to setDeviceMetricsOverride", "error", err)
		return client.SetDeviceMetricsOverride(cdpCtx, width, height)
	}); err != nil {
		return fmt.Errorf("CDP viewport resize: %w", err)
	}
	log.Info("viewport resized via CDP", "width", width, "height", height)
	return nil
}

// contentsSizeRefused reports whether SetContentsSize failed because Chromium
// declined to resize the window, as opposed to the page target or the
// connection being unavailable, where the fallback would fail the same way.
func contentsSizeRefused(err error) bool {
	var cdpErr *cdpclient.Error
	return errors.Is(err, cdpclient.ErrWindowFullscreen) || errors.As(err, &cdpErr)
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

// restoreHeadlessViewport sizes a freshly started headless Chromium to the
// resolution GET /display reports: the pending viewport override while Xvfb
// is still resizing, otherwise the Xvfb size. Headless Chromium launches at
// its own default window size (maximized on its 800x600 screen in
// production), so without this a restart (profile load, policy or flag
// change) silently shrinks the page while GET /display still reports the
// requested size.
func (s *ApiService) restoreHeadlessViewport(ctx context.Context) error {
	if s.detectDisplayMode(ctx) != "xvfb" {
		return nil
	}
	width, height, _, err := s.getCurrentResolution(ctx)
	if err != nil {
		return err
	}
	return s.setViewportViaCDP(ctx, width, height)
}
