package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	nekooapi "github.com/m1k1o/neko/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/recorder"
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

	log.Info(fmt.Sprintf("resolution change requested from %dx%d@%d to %dx%d@%d", currentWidth, currentHeight, currentRefreshRate, width, height, refreshRate))

	// Parse requireIdle flag (default true)
	requireIdle := true
	if req.Body.RequireIdle != nil {
		requireIdle = *req.Body.RequireIdle
	}

	// Check if resize is safe (no active live view sessions)
	if requireIdle {
		live := s.getActiveNekoSessions(ctx)
		if live > 0 {
			log.Info("resize refused: live view or replay active", "live_sessions", live)
			return oapi.PatchDisplay409JSONResponse{
				ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{
					Message: "resize refused: live view or replay active",
				},
			}, nil
		}

		// Gracefully stop active recordings so the resize can proceed.
		// They will be restarted (with new segment files) after the resize completes.
		stopped, stopErr := s.stopActiveRecordings(ctx)
		if len(stopped) > 0 {
			defer s.restartRecordings(context.WithoutCancel(ctx), stopped)
		}
		if stopErr != nil {
			log.Error("failed to stop recordings for resize", "error", stopErr)
			return oapi.PatchDisplay500JSONResponse{
				InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
					Message: fmt.Sprintf("failed to stop recordings for resize: %s", stopErr.Error()),
				},
			}, nil
		}
	}

	// Detect display mode (xorg or xvfb)
	displayMode := s.detectDisplayMode(ctx)

	// Parse restartChromium flag (default depends on mode)
	restartChrome := (displayMode == "xvfb") // default true for xvfb, false for xorg
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
			err = s.setResolutionXorgViaXrandr(ctx, width, height, refreshRate, restartChrome)
		}
		if err == nil && restartChrome {
			if restartErr := s.restartChromiumAndWait(ctx, "resolution change"); restartErr != nil {
				log.Error("failed to restart chromium after resolution change", "error", restartErr)
			}
		}
	} else {
		log.Info("using Xvfb restart for resolution change")
		err = s.setResolutionXvfb(ctx, width, height, restartChrome)
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

// detectDisplayMode detects whether we're running Xorg (headful) or Xvfb (headless)
func (s *ApiService) detectDisplayMode(ctx context.Context) string {
	log := logger.FromContext(ctx)
	checkCmd := []string{"-lc", "supervisorctl status xvfb >/dev/null 2>&1 && echo 'xvfb' || echo 'xorg'"}
	checkReq := oapi.ProcessExecRequest{Command: "bash", Args: &checkCmd}
	checkResp, _ := s.ProcessExec(ctx, oapi.ProcessExecRequestObject{Body: &checkReq})

	if execResp, ok := checkResp.(oapi.ProcessExec200JSONResponse); ok {
		if execResp.StdoutB64 != nil {
			if output, err := base64.StdEncoding.DecodeString(*execResp.StdoutB64); err == nil {
				outputStr := strings.TrimSpace(string(output))
				if outputStr == "xvfb" {
					log.Info("detected Xvfb display (headless mode)")
					return "xvfb"
				}
			}
		}
	}
	log.Info("detected Xorg display (headful mode)")
	return "xorg"
}

// setResolutionXorgViaXrandr changes resolution for Xorg using xrandr (fallback when Neko is disabled)
func (s *ApiService) setResolutionXorgViaXrandr(ctx context.Context, width, height, refreshRate int, restartChrome bool) error {
	log := logger.FromContext(ctx)
	display := s.resolveDisplayFromEnv()

	// Build xrandr command - if refresh rate is specified, use the specific modeline
	var xrandrCmd string
	if refreshRate > 0 {
		modeName := fmt.Sprintf("%dx%d_%d.00", width, height, refreshRate)
		xrandrCmd = fmt.Sprintf("xrandr --output default --mode %s", modeName)
		log.Info("using specific modeline", "mode", modeName)
	} else {
		xrandrCmd = fmt.Sprintf("xrandr -s %dx%d", width, height)
	}

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

// setResolutionXvfb changes resolution for Xvfb by updating config and restarting services
func (s *ApiService) setResolutionXvfb(ctx context.Context, width, height int, restartChrome bool) error {
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

	if restartChrome {
		if restartErr := s.restartChromiumAndWait(ctx, "xvfb resolution change"); restartErr != nil {
			log.Error("failed to restart chromium after xvfb resolution change", "error", restartErr)
		}
	}

	log.Info("Xvfb resolution updated", "width", width, "height", height)
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

// getCurrentResolution returns the current display resolution and refresh rate by querying xrandr
func (s *ApiService) getCurrentResolution(ctx context.Context) (int, int, int, error) {
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
	id         string
	params     recorder.FFmpegRecordingParams
	outputPath string
}

// stopActiveRecordings gracefully stops every recording that is currently in
// progress and deregisters them from the manager. It returns info needed to
// restart each recording later. Recordings that were successfully stopped are
// always included in the returned slice, even when a later recording fails to
// stop (so the caller can restart whatever was stopped).
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
		outputPath := ffmpegRec.OutputPath()

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

		if err := s.recordManager.DeregisterRecorder(ctx, rec); err != nil {
			log.Error("failed to deregister recorder, skipping restart to avoid ID conflict", "id", id, "error", err)
			continue
		}

		stopped = append(stopped, stoppedRecordingInfo{
			id:         id,
			params:     params,
			outputPath: outputPath,
		})
		log.Info("recording stopped and deregistered for resize", "id", id)
	}

	return stopped, nil
}

// restartRecordings re-creates and starts recordings that were previously
// stopped for a display resize. The old (finalized) recording file is renamed
// to preserve it before the new recording begins at the same output path.
func (s *ApiService) restartRecordings(ctx context.Context, stopped []stoppedRecordingInfo) {
	log := logger.FromContext(ctx)

	for _, info := range stopped {
		// Best-effort: preserve the pre-resize segment by renaming the finalized file.
		// If this fails the old file may be overwritten, but we still restart recording.
		if _, err := os.Stat(info.outputPath); err == nil {
			preservedPath := strings.TrimSuffix(info.outputPath, ".mp4") +
				fmt.Sprintf("-before-resize-%d.mp4", time.Now().UnixMilli())
			if err := os.Rename(info.outputPath, preservedPath); err != nil {
				log.Error("failed to rename pre-resize recording, old file may be overwritten", "id", info.id, "error", err)
			} else {
				log.Info("preserved pre-resize recording segment", "id", info.id, "path", preservedPath)
			}
		}

		rec, err := s.factory(info.id, info.params)
		if err != nil {
			log.Error("failed to create recorder for restart", "id", info.id, "error", err)
			continue
		}

		if err := s.recordManager.RegisterRecorder(ctx, rec); err != nil {
			log.Error("failed to register restarted recorder", "id", info.id, "error", err)
			continue
		}

		if err := rec.Start(ctx); err != nil {
			log.Error("failed to start restarted recording", "id", info.id, "error", err)
			_ = s.recordManager.DeregisterRecorder(ctx, rec)
			continue
		}

		log.Info("recording restarted after resize", "id", info.id)
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

