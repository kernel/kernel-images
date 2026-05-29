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

	var err error
	currentWidth, currentHeight, currentRefreshRate := 0, 0, 0
	needCurrent := req.Body.Width == nil || req.Body.Height == nil || req.Body.RefreshRate == nil
	if needCurrent {
		currentWidth, currentHeight, currentRefreshRate, err = s.getCurrentResolution(ctx)
		if err != nil {
			log.Error("failed to get current resolution", "error", err)
			return oapi.PatchDisplay500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to get current display resolution"}}, nil
		}
	}

	width := currentWidth
	height := currentHeight
	refreshRate := currentRefreshRate
	if req.Body.Width != nil {
		width = *req.Body.Width
	}
	if req.Body.Height != nil {
		height = *req.Body.Height
	}
	if req.Body.RefreshRate != nil {
		refreshRate = int(*req.Body.RefreshRate)
	}

	if width <= 0 || height <= 0 {
		return oapi.PatchDisplay400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid width/height"}}, nil
	}

	if needCurrent {
		log.Info(fmt.Sprintf("resolution change requested from %dx%d@%d to %dx%d@%d", currentWidth, currentHeight, currentRefreshRate, width, height, refreshRate))
	} else {
		log.Info(fmt.Sprintf("resolution change requested to %dx%d@%d", width, height, refreshRate))
	}

	// Parse requireIdle flag (default true)
	requireIdle := true
	if req.Body.RequireIdle != nil {
		requireIdle = *req.Body.RequireIdle
	}

	// Check if resize is safe (no active sessions or recordings)
	if requireIdle {
		live := 0
		if s.isNekoEnabled() {
			live = s.getActiveNekoSessions(ctx)
		}
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
			if err == nil && restartChrome {
				if windowErr := s.verifyMaximizedChromiumWindow(ctx, width, height); windowErr != nil {
					log.Error("chromium window was not ready after Neko resolution change", "error", windowErr)
					err = windowErr
				} else if cdpErr := s.verifyCurrentCDP(ctx); cdpErr != nil {
					log.Error("CDP was not ready after Neko resolution change", "error", cdpErr)
					err = cdpErr
				} else {
					log.Info("restart_chromium requested for Neko resize; skipped because the maximized Chromium window and CDP stayed ready")
				}
			}
		} else {
			log.Info("using xrandr for Xorg resolution change (Neko disabled)")
			err = s.setResolutionXorgViaXrandr(ctx, width, height, refreshRate, restartChrome)
			if err == nil && restartChrome {
				log.Info("restart_chromium requested for Xorg resize; skipped because RandR synchronously resized the running Chromium window")
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

// setResolutionXorgViaXrandr changes resolution for Xorg using xrandr (fallback when Neko is disabled).
func (s *ApiService) setResolutionXorgViaXrandr(ctx context.Context, width, height, refreshRate int, restartChrome bool) error {
	log := logger.FromContext(ctx)
	display := s.resolveDisplayFromEnv()
	if refreshRate <= 0 {
		refreshRate = 60
	}
	modeName := fmt.Sprintf("%dx%d_%d.00", width, height, refreshRate)
	want := fmt.Sprintf("%dx%d", width, height)
	enforceChromiumWindow := "false"
	if restartChrome {
		enforceChromiumWindow = "true"
	}

	// The headful Xorg dummy driver exposes DUMMY0, not "default". `xrandr
	// --output default ...` exits successfully while doing nothing, so always
	// discover the connected output and verify the final screen size. The
	// control plane can request non-standard widths such as 1365px; generate a
	// runtime modeline when xorg.conf does not already contain the exact mode.
	xrandrScript := fmt.Sprintf(`set -euo pipefail
export DISPLAY=%q
output="${KERNEL_IMAGES_XRANDR_OUTPUT:-DUMMY0}"
mode=%q
if ! xrandr --output "$output" --mode "$mode" 2>/dev/null; then
  modeline="$(gtf %d %d %d | awk -v w=%d '/Modeline/ { for (i=3; i<=NF; i++) { if (i == 4) $i = w; printf "%%s%%s", $i, (i < NF ? " " : "") } }')"
  if [ -z "$modeline" ]; then
    echo "failed to generate modeline for $mode" >&2
    exit 1
  fi
  xrandr --newmode "$mode" $modeline 2>/dev/null || true
  xrandr --addmode "$output" "$mode" 2>/dev/null || true
  xrandr --output "$output" --mode "$mode"
fi
# xrandr exits non-zero if DUMMY0 cannot enter the requested mode; the
# Chromium window check below gates the WM/browser adaptation before returning.
if [ %q = "true" ] && command -v xdotool >/dev/null 2>&1; then
  best_id="$(xdotool getactivewindow 2>/dev/null || true)"
  WIDTH=0
  HEIGHT=0
  if [ -n "$best_id" ]; then
    geom="$(xdotool getwindowgeometry --shell "$best_id" 2>/dev/null || true)"
    eval "$geom"
  fi
  if [ "${WIDTH}x${HEIGHT}" != %q ] && [ -n "$best_id" ]; then
    active_id="$best_id"
    class="$(xdotool getwindowclassname "$active_id" 2>/dev/null || true)"
    case "$class" in
      *[Cc]hrom*)
        xdotool windowmove "$active_id" 0 0 >/dev/null 2>&1 || true
        xdotool windowsize "$active_id" %d %d >/dev/null 2>&1 || true
        for _ in $(seq 1 25); do
          geom="$(xdotool getwindowgeometry --shell "$active_id" 2>/dev/null || true)"
          WIDTH=0
          HEIGHT=0
          eval "$geom"
          if [ "${WIDTH}x${HEIGHT}" = %q ]; then
            break
          fi
          sleep 0.02
        done
        best_id="$active_id"
        ;;
    esac
  fi
  if [ "${WIDTH}x${HEIGHT}" != %q ]; then
    best_id=""
    best_w=0
    best_h=0
    best_area=-1
    for wid in $(xdotool search --class chromium 2>/dev/null || true); do
      geom="$(xdotool getwindowgeometry --shell "$wid" 2>/dev/null || true)"
      WIDTH=0
      HEIGHT=0
      eval "$geom"
      area=$((WIDTH * HEIGHT))
      if [ "$area" -gt "$best_area" ]; then
        best_area=$area
        best_id=$wid
        best_w=$WIDTH
        best_h=$HEIGHT
      fi
    done
    WIDTH=$best_w
    HEIGHT=$best_h
  fi
  if [ -n "$best_id" ]; then
    if [ "${WIDTH}x${HEIGHT}" != %q ]; then
      xdotool windowmove "$best_id" 0 0 >/dev/null 2>&1 || true
      xdotool windowsize "$best_id" %d %d >/dev/null 2>&1 || true
      for _ in $(seq 1 25); do
        geom="$(xdotool getwindowgeometry --shell "$best_id" 2>/dev/null || true)"
        WIDTH=0
        HEIGHT=0
        eval "$geom"
        if [ "${WIDTH}x${HEIGHT}" = %q ]; then
          break
        fi
        sleep 0.02
      done
    fi
    if [ "${WIDTH}x${HEIGHT}" != %q ]; then
      echo "Chromium window is ${WIDTH}x${HEIGHT} after xrandr; want %s" >&2
      exit 1
    fi
  fi
fi
`, display, modeName, width, height, refreshRate, width, enforceChromiumWindow, want, width, height, want, want, want, width, height, want, want, want)

	args := []string{"-c", xrandrScript}
	execReq := oapi.ProcessExecRequest{Command: "bash", Args: &args}
	resp, err := s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &execReq})
	if err != nil {
		return fmt.Errorf("failed to execute xrandr: %w", err)
	}

	switch r := resp.(type) {
	case oapi.ProcessExec200JSONResponse:
		if r.ExitCode != nil && *r.ExitCode != 0 {
			stderr := decodeProcessOutput(r.StderrB64)
			if stderr == "" {
				stderr = decodeProcessOutput(r.StdoutB64)
			}
			if stderr == "" {
				stderr = "xrandr returned non-zero exit code"
			}
			return fmt.Errorf("xrandr failed: %s", stderr)
		}
		log.Info("resolution updated via xrandr", "display", display, "mode", modeName, "width", width, "height", height, "restart_chromium", restartChrome)
		return nil
	case oapi.ProcessExec400JSONResponse:
		return fmt.Errorf("bad request: %s", r.Message)
	case oapi.ProcessExec500JSONResponse:
		return fmt.Errorf("internal error: %s", r.Message)
	default:
		return fmt.Errorf("unexpected response from process exec")
	}
}

func decodeProcessOutput(encoded *string) string {
	if encoded == nil {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(*encoded)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
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

// setViewportViaCDP resizes the browser viewport using the CDP
// Emulation.setDeviceMetricsOverride command. This is near-instant and does
// not require restarting Chromium or Xvfb.
func (s *ApiService) setViewportViaCDP(ctx context.Context, width, height int) error {
	log := logger.FromContext(ctx)

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

	if err := client.SetDeviceMetricsOverride(cdpCtx, width, height); err != nil {
		return fmt.Errorf("CDP setDeviceMetricsOverride: %w", err)
	}

	log.Info("viewport resized via CDP", "width", width, "height", height)
	return nil
}

func (s *ApiService) verifyCurrentCDP(ctx context.Context) error {
	upstreamURL := s.upstreamMgr.Current()
	if upstreamURL == "" {
		return fmt.Errorf("devtools upstream not available")
	}
	cdpCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client, err := cdpclient.Dial(cdpCtx, upstreamURL)
	if err != nil {
		return fmt.Errorf("failed to connect to devtools: %w", err)
	}
	defer client.Close()
	if _, err := client.GetBrowserVersion(cdpCtx); err != nil {
		return fmt.Errorf("Browser.getVersion: %w", err)
	}
	return nil
}

func (s *ApiService) verifyMaximizedChromiumWindow(ctx context.Context, width, height int) error {
	display := s.resolveDisplayFromEnv()
	script := fmt.Sprintf(`set -euo pipefail
export DISPLAY=%q
command -v xprop >/dev/null 2>&1 || { echo "xprop not found" >&2; exit 1; }
want=%q
check_window() {
  wid="$1"
  geom="$(xdotool getwindowgeometry --shell "$wid" 2>/dev/null || true)"
  WIDTH=0
  HEIGHT=0
  eval "$geom"
  state="$(xprop -id "$wid" _NET_WM_STATE 2>/dev/null || true)"
  case "$state" in
    *_NET_WM_STATE_MAXIMIZED_VERT*_NET_WM_STATE_MAXIMIZED_HORZ*|*_NET_WM_STATE_MAXIMIZED_HORZ*_NET_WM_STATE_MAXIMIZED_VERT*) maximized=1 ;;
    *) maximized=0 ;;
  esac
  [ "${WIDTH}x${HEIGHT}" = "$want" ] && [ "$maximized" = 1 ]
}
active_id="$(xdotool getactivewindow 2>/dev/null || true)"
if [ -n "$active_id" ]; then
  active_class="$(xprop -id "$active_id" WM_CLASS 2>/dev/null || true)"
  case "$active_class" in
    *[Cc]hrom*)
      if check_window "$active_id"; then
        exit 0
      fi
      ;;
  esac
fi
best_id=""
best_w=0
best_h=0
best_area=-1
for wid in $(xdotool search --class chromium 2>/dev/null || true); do
  geom="$(xdotool getwindowgeometry --shell "$wid" 2>/dev/null || true)"
  WIDTH=0
  HEIGHT=0
  eval "$geom"
  area=$((WIDTH * HEIGHT))
  if [ "$area" -gt "$best_area" ]; then
    best_area=$area
    best_id=$wid
    best_w=$WIDTH
    best_h=$HEIGHT
  fi
done
if [ -z "$best_id" ]; then
  echo "no Chromium window found" >&2
  exit 1
fi
last="unknown"
for _ in $(seq 1 50); do
  if check_window "$best_id"; then
    exit 0
  fi
  last="${WIDTH}x${HEIGHT} maximized=${maximized:-0}"
  sleep 0.02
done
echo "Chromium window is $last; want $want maximized=1" >&2
exit 1
`, display, fmt.Sprintf("%dx%d", width, height))
	args := []string{"-c", script}
	execReq := oapi.ProcessExecRequest{Command: "bash", Args: &args}
	resp, err := s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &execReq})
	if err != nil {
		return fmt.Errorf("failed to verify Chromium window: %w", err)
	}
	switch r := resp.(type) {
	case oapi.ProcessExec200JSONResponse:
		if r.ExitCode != nil && *r.ExitCode != 0 {
			stderr := "Chromium window verification returned non-zero exit code"
			if r.StderrB64 != nil {
				if b, decErr := base64.StdEncoding.DecodeString(*r.StderrB64); decErr == nil && strings.TrimSpace(string(b)) != "" {
					stderr = strings.TrimSpace(string(b))
				}
			}
			return fmt.Errorf("verify Chromium window: %s", stderr)
		}
		return nil
	case oapi.ProcessExec400JSONResponse:
		return fmt.Errorf("bad request: %s", r.Message)
	case oapi.ProcessExec500JSONResponse:
		return fmt.Errorf("internal error: %s", r.Message)
	default:
		return fmt.Errorf("unexpected response from process exec")
	}
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
func (s *ApiService) setViewportOverride(width, height, refreshRate int) {
	s.viewportMu.Lock()
	s.viewportOverride = &[3]int{width, height, refreshRate}
	s.viewportMu.Unlock()
}

// clearViewportOverride removes the viewport override (e.g. after Xvfb
// finishes restarting and xrandr is reliable again).
func (s *ApiService) clearViewportOverride() {
	s.viewportMu.Lock()
	s.viewportOverride = nil
	s.viewportMu.Unlock()
}

// getCurrentResolution returns the current display resolution and refresh
// rate. If a viewport override is set (from a recent CDP resize while Xvfb
// restarts in the background), it returns the override instead of querying
// xrandr, which may fail during Xvfb restarts.
func (s *ApiService) getCurrentResolution(ctx context.Context) (int, int, int, error) {
	s.viewportMu.RLock()
	override := s.viewportOverride
	s.viewportMu.RUnlock()
	if override != nil {
		return override[0], override[1], override[2], nil
	}

	return s.getCurrentResolutionFromXrandr(ctx)
}

// getCurrentResolutionFromXrandr queries xrandr for the current display resolution.
func (s *ApiService) getCurrentResolutionFromXrandr(ctx context.Context) (int, int, int, error) {
	log := logger.FromContext(ctx)
	display := s.resolveDisplayFromEnv()

	// Use xrandr to get current resolution
	// Note: Using bash -c (not -lc) to avoid login shell overriding DISPLAY env var
	cmd := exec.CommandContext(ctx, "bash", "-c", "xrandr | grep -E '\\*' | awk '{print $1}'")
	cmd.Env = append(os.Environ(), fmt.Sprintf("DISPLAY=%s", display))

	out, err := cmd.Output()
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return 0, 0, 0, fmt.Errorf("failed to execute xrandr command: %w", err)
	}

	resStr := strings.TrimSpace(string(out))
	parts := strings.Split(resStr, "x")
	if len(parts) != 2 {
		log.Error("unexpected xrandr output format", "output", resStr)
		return 0, 0, 0, fmt.Errorf("unexpected xrandr output format: %s", resStr)
	}

	width, err := strconv.Atoi(parts[0])
	if err != nil {
		log.Error("failed to parse width", "error", err, "value", parts[0])
		return 0, 0, 0, fmt.Errorf("failed to parse width '%s': %w", parts[0], err)
	}

	// Parse height and refresh rate (e.g., "1080_60.00" -> height=1080, rate=60)
	heightStr := parts[1]
	refreshRate := 60 // default
	if idx := strings.Index(heightStr, "_"); idx != -1 {
		rateStr := heightStr[idx+1:]
		heightStr = heightStr[:idx]
		// Parse the refresh rate (e.g., "60.00" -> 60)
		if rateFloat, err := strconv.ParseFloat(rateStr, 64); err == nil {
			refreshRate = int(rateFloat)
		}
	}

	height, err := strconv.Atoi(heightStr)
	if err != nil {
		log.Error("failed to parse height", "error", err, "value", heightStr)
		return 0, 0, 0, fmt.Errorf("failed to parse height '%s': %w", heightStr, err)
	}

	return width, height, refreshRate, nil
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

// setResolutionViaNeko delegates resolution change to Neko API
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
