package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
)

// validationError represents a client-side error (400).
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

// executionError represents a server-side error (500).
type executionError struct{ msg string }

func (e *executionError) Error() string { return e.msg }

func isValidationErr(err error) bool {
	var ve *validationError
	return errors.As(err, &ve)
}

func (s *ApiService) doMoveMouse(ctx context.Context, body oapi.MoveMouseRequest) error {
	log := logger.FromContext(ctx)

	// Get current resolution for bounds validation
	screenWidth, screenHeight, _, err := s.getCurrentResolution(ctx)
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return &executionError{msg: "failed to get current display resolution"}
	}

	// Ensure non-negative coordinates and within screen bounds
	if body.X < 0 || body.Y < 0 {
		return &validationError{msg: "coordinates must be non-negative"}
	}
	if body.X >= screenWidth || body.Y >= screenHeight {
		return &validationError{msg: fmt.Sprintf("coordinates exceed screen bounds (max: %dx%d)", screenWidth-1, screenHeight-1)}
	}

	// Build xdotool arguments
	args := []string{}

	// Hold modifier keys (keydown)
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keydown", key)
		}
	}

	// Move the cursor to the desired coordinates
	args = append(args, "mousemove", strconv.Itoa(body.X), strconv.Itoa(body.Y))

	// Release modifier keys (keyup)
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keyup", key)
		}
	}

	log.Info("executing xdotool", "args", args)

	output, err := defaultXdoTool.Run(ctx, args...)
	if err != nil {
		log.Error("xdotool command failed", "err", err, "output", string(output))
		return &executionError{msg: "failed to move mouse"}
	}

	return nil
}

func (s *ApiService) MoveMouse(ctx context.Context, request oapi.MoveMouseRequestObject) (oapi.MoveMouseResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.MoveMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	if err := s.doMoveMouse(ctx, *request.Body); err != nil {
		if isValidationErr(err) {
			return oapi.MoveMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		return oapi.MoveMouse500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}
	return oapi.MoveMouse200Response{}, nil
}

func (s *ApiService) doClickMouse(ctx context.Context, body oapi.ClickMouseRequest) error {
	log := logger.FromContext(ctx)

	// Get current resolution for bounds validation
	screenWidth, screenHeight, _, err := s.getCurrentResolution(ctx)
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return &executionError{msg: "failed to get current display resolution"}
	}

	// Ensure non-negative coordinates and within screen bounds
	if body.X < 0 || body.Y < 0 {
		return &validationError{msg: "coordinates must be non-negative"}
	}
	if body.X >= screenWidth || body.Y >= screenHeight {
		return &validationError{msg: fmt.Sprintf("coordinates exceed screen bounds (max: %dx%d)", screenWidth-1, screenHeight-1)}
	}

	// Map button enum to xdotool button code. Default to left button.
	btn := "1"
	if body.Button != nil {
		buttonMap := map[oapi.ClickMouseRequestButton]string{
			oapi.ClickMouseRequestButtonLeft:    "1",
			oapi.ClickMouseRequestButtonMiddle:  "2",
			oapi.ClickMouseRequestButtonRight:   "3",
			oapi.ClickMouseRequestButtonBack:    "8",
			oapi.ClickMouseRequestButtonForward: "9",
		}
		var ok bool
		btn, ok = buttonMap[*body.Button]
		if !ok {
			return &validationError{msg: fmt.Sprintf("unsupported button: %s", *body.Button)}
		}
	}

	// Determine number of clicks (defaults to 1)
	numClicks := 1
	if body.NumClicks != nil && *body.NumClicks > 0 {
		numClicks = *body.NumClicks
	}

	// Build xdotool arguments
	args := []string{}

	// Hold modifier keys (keydown)
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keydown", key)
		}
	}

	// Move the cursor
	args = append(args, "mousemove", strconv.Itoa(body.X), strconv.Itoa(body.Y))

	// click type defaults to click
	clickType := oapi.Click
	if body.ClickType != nil {
		clickType = *body.ClickType
	}

	// Perform the click action
	switch clickType {
	case oapi.Down:
		args = append(args, "mousedown", btn)
	case oapi.Up:
		args = append(args, "mouseup", btn)
	case oapi.Click:
		args = append(args, "click")
		if numClicks > 1 {
			args = append(args, "--repeat", strconv.Itoa(numClicks))
		}
		args = append(args, btn)
	default:
		return &validationError{msg: fmt.Sprintf("unsupported click type: %s", clickType)}
	}

	// Release modifier keys (keyup)
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keyup", key)
		}
	}

	log.Info("executing xdotool", "args", args)

	output, err := defaultXdoTool.Run(ctx, args...)
	if err != nil {
		log.Error("xdotool command failed", "err", err, "output", string(output))
		return &executionError{msg: "failed to execute mouse action"}
	}

	return nil
}

func (s *ApiService) ClickMouse(ctx context.Context, request oapi.ClickMouseRequestObject) (oapi.ClickMouseResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.ClickMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	if err := s.doClickMouse(ctx, *request.Body); err != nil {
		if isValidationErr(err) {
			return oapi.ClickMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		return oapi.ClickMouse500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}
	return oapi.ClickMouse200Response{}, nil
}

func (s *ApiService) TakeScreenshot(ctx context.Context, request oapi.TakeScreenshotRequestObject) (oapi.TakeScreenshotResponseObject, error) {
	log := logger.FromContext(ctx)

	// serialize input operations to avoid race with other input/screen actions
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	var body oapi.ScreenshotRequest
	if request.Body != nil {
		body = *request.Body
	}

	// Get current resolution for bounds validation
	screenWidth, screenHeight, _, err := s.getCurrentResolution(ctx)
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return oapi.TakeScreenshot500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
			Message: "failed to get current display resolution"},
		}, nil
	}

	// Determine display to use (align with other functions)
	display := s.resolveDisplayFromEnv()

	// Validate region if provided
	if body.Region != nil {
		r := body.Region
		if r.X < 0 || r.Y < 0 || r.Width <= 0 || r.Height <= 0 {
			return oapi.TakeScreenshot400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "invalid region dimensions"},
			}, nil
		}
		if r.X+r.Width > screenWidth || r.Y+r.Height > screenHeight {
			return oapi.TakeScreenshot400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
				Message: "region exceeds screen bounds"},
			}, nil
		}
	}

	// Build ffmpeg command
	args := []string{
		"-f", "x11grab",
		"-video_size", fmt.Sprintf("%dx%d", screenWidth, screenHeight),
		"-i", display,
		"-vframes", "1",
	}

	// Add crop filter if region is specified
	if body.Region != nil {
		r := body.Region
		cropFilter := fmt.Sprintf("crop=%d:%d:%d:%d", r.Width, r.Height, r.X, r.Y)
		args = append(args, "-vf", cropFilter)
	}

	// Output as PNG to stdout
	args = append(args, "-f", "image2pipe", "-vcodec", "png", "-")

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("DISPLAY=%s", display))

	log.Debug("executing ffmpeg command", "args", args, "display", display)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Error("failed to create stdout pipe", "err", err)
		return oapi.TakeScreenshot500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
			Message: "internal error"},
		}, nil
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Error("failed to create stderr pipe", "err", err)
		return oapi.TakeScreenshot500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
			Message: "internal error"},
		}, nil
	}

	if err := cmd.Start(); err != nil {
		log.Error("failed to start ffmpeg", "err", err)
		return oapi.TakeScreenshot500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
			Message: "failed to start ffmpeg"},
		}, nil
	}

	// Start a goroutine to drain stderr for logging to avoid blocking
	go func() {
		data, _ := io.ReadAll(stderr)
		if len(data) > 0 {
			// ffmpeg writes progress/info to stderr; include in debug logs
			enc := base64.StdEncoding.EncodeToString(data)
			log.Debug("ffmpeg stderr (base64)", "data_b64", enc)
		}
	}()

	pr, pw := io.Pipe()
	go func() {
		_, copyErr := io.Copy(pw, stdout)
		waitErr := cmd.Wait()
		var closeErr error
		if copyErr != nil {
			closeErr = fmt.Errorf("streaming ffmpeg output: %w", copyErr)
			log.Error("failed streaming ffmpeg output", "err", copyErr)
		} else if waitErr != nil {
			closeErr = fmt.Errorf("ffmpeg exited with error: %w", waitErr)
			log.Error("ffmpeg exited with error", "err", waitErr)
		}
		if closeErr != nil {
			_ = pw.CloseWithError(closeErr)
			return
		}
		_ = pw.Close()
	}()

	return oapi.TakeScreenshot200ImagepngResponse{Body: pr, ContentLength: 0}, nil
}

func (s *ApiService) doTypeText(ctx context.Context, body oapi.TypeTextRequest) error {
	log := logger.FromContext(ctx)

	// Validate delay if provided
	if body.Delay != nil && *body.Delay < 0 {
		return &validationError{msg: "delay must be >= 0 milliseconds"}
	}

	// Build xdotool arguments
	args := []string{"type"}
	if body.Delay != nil {
		args = append(args, "--delay", strconv.Itoa(*body.Delay))
	}
	// Use "--" to terminate options and pass raw text
	args = append(args, "--", body.Text)

	output, err := defaultXdoTool.Run(ctx, args...)
	if err != nil {
		log.Error("xdotool command failed", "err", err, "output", string(output))
		return &executionError{msg: "failed to type text"}
	}

	return nil
}

func (s *ApiService) TypeText(ctx context.Context, request oapi.TypeTextRequestObject) (oapi.TypeTextResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.TypeText400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	if err := s.doTypeText(ctx, *request.Body); err != nil {
		if isValidationErr(err) {
			return oapi.TypeText400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		return oapi.TypeText500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}
	return oapi.TypeText200Response{}, nil
}

const (

	// Unclutter configuration for cursor hiding
	// Setting idle to 0 hides the cursor immediately
	unclutterIdleSeconds = "0"

	// A very large jitter value (9 million pixels) ensures that all mouse
	// movements are treated as "noise", keeping the cursor permanently hidden
	// when combined with idle=0
	unclutterJitterPixels = "9000000"
)

func (s *ApiService) doSetCursor(ctx context.Context, body oapi.SetCursorRequest) error {
	log := logger.FromContext(ctx)

	// Kill any existing unclutter processes first
	pkillCmd := exec.CommandContext(ctx, "pkill", "unclutter")
	pkillCmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: 0, Gid: 0},
	}

	if err := pkillCmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			log.Error("failed to kill existing unclutter processes", "err", err)
			return &executionError{msg: "failed to kill existing unclutter processes"}
		}
	}

	if body.Hidden {
		display := s.resolveDisplayFromEnv()
		unclutterCmd := exec.CommandContext(context.Background(),
			"unclutter",
			"-idle", unclutterIdleSeconds,
			"-jitter", unclutterJitterPixels,
		)
		unclutterCmd.Env = append(os.Environ(), fmt.Sprintf("DISPLAY=%s", display))
		unclutterCmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: 0, Gid: 0},
		}

		if err := unclutterCmd.Start(); err != nil {
			log.Error("failed to start unclutter", "err", err)
			return &executionError{msg: "failed to start unclutter"}
		}
	}

	return nil
}

func (s *ApiService) SetCursor(ctx context.Context, request oapi.SetCursorRequestObject) (oapi.SetCursorResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.SetCursor400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	if err := s.doSetCursor(ctx, *request.Body); err != nil {
		if isValidationErr(err) {
			return oapi.SetCursor400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		return oapi.SetCursor500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}
	return oapi.SetCursor200JSONResponse{Ok: true}, nil
}

// parseMousePosition parses xdotool getmouselocation --shell output.
// Expected format:
//
//	X=100
//	Y=200
//	SCREEN=0
//	WINDOW=12345
//
// Returns x, y coordinates and an error if parsing fails.
func parseMousePosition(output string) (x, y int, err error) {
	outStr := strings.TrimSpace(output)
	if outStr == "" {
		return 0, 0, fmt.Errorf("empty output")
	}

	var xParsed, yParsed bool

	for line := range strings.SplitSeq(outStr, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]
		switch key {
		case "X":
			if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
				x = parsed
				xParsed = true
			}
		case "Y":
			if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
				y = parsed
				yParsed = true
			}
		}
		// Early exit once both coordinates are found
		if xParsed && yParsed {
			break
		}
	}

	if !xParsed || !yParsed {
		return 0, 0, fmt.Errorf("failed to parse coordinates from output: %q", outStr)
	}

	return x, y, nil
}

func (s *ApiService) GetMousePosition(ctx context.Context, request oapi.GetMousePositionRequestObject) (oapi.GetMousePositionResponseObject, error) {
	log := logger.FromContext(ctx)

	// serialize input operations to avoid race conditions with other xdotool commands
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	// Execute xdotool getmouselocation --shell for parseable output
	output, err := defaultXdoTool.Run(ctx, "getmouselocation", "--shell")
	if err != nil {
		log.Error("xdotool getmouselocation failed", "err", err, "output", string(output))
		return oapi.GetMousePosition500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
			Message: "failed to get mouse position"},
		}, nil
	}

	x, y, err := parseMousePosition(string(output))
	if err != nil {
		log.Error("failed to parse mouse position", "err", err, "output", string(output))
		return oapi.GetMousePosition500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
			Message: "failed to parse mouse position from xdotool output"},
		}, nil
	}

	return oapi.GetMousePosition200JSONResponse{
		X: x,
		Y: y,
	}, nil
}

func (s *ApiService) doPressKey(ctx context.Context, body oapi.PressKeyRequest) error {
	log := logger.FromContext(ctx)

	if len(body.Keys) == 0 {
		return &validationError{msg: "keys must contain at least one key symbol"}
	}
	if body.Duration != nil && *body.Duration < 0 {
		return &validationError{msg: "duration must be >= 0 milliseconds"}
	}

	// If duration is provided and >0, hold all keys down, sleep, then release.
	if body.Duration != nil && *body.Duration > 0 {
		argsDown := []string{}
		if body.HoldKeys != nil {
			for _, key := range *body.HoldKeys {
				argsDown = append(argsDown, "keydown", key)
			}
		}
		for _, key := range body.Keys {
			argsDown = append(argsDown, "keydown", key)
		}

		if output, err := defaultXdoTool.Run(ctx, argsDown...); err != nil {
			log.Error("xdotool keydown failed", "err", err, "output", string(output))
			// Best-effort release any keys that may be down (primary and modifiers)
			argsUp := []string{}
			for _, key := range body.Keys {
				argsUp = append(argsUp, "keyup", key)
			}
			if body.HoldKeys != nil {
				for _, key := range *body.HoldKeys {
					argsUp = append(argsUp, "keyup", key)
				}
			}
			_, _ = defaultXdoTool.Run(ctx, argsUp...)
			return &executionError{msg: fmt.Sprintf("failed to press keys (keydown). out=%s", string(output))}
		}

		d := time.Duration(*body.Duration) * time.Millisecond
		time.Sleep(d)

		argsUp := []string{}
		for _, key := range body.Keys {
			argsUp = append(argsUp, "keyup", key)
		}
		if body.HoldKeys != nil {
			for _, key := range *body.HoldKeys {
				argsUp = append(argsUp, "keyup", key)
			}
		}

		if output, err := defaultXdoTool.Run(ctx, argsUp...); err != nil {
			log.Error("xdotool keyup failed", "err", err, "output", string(output))
			return &executionError{msg: fmt.Sprintf("failed to release keys. out=%s", string(output))}
		}

		return nil
	}

	// Tap behavior: hold modifiers (if any), tap each key, then release modifiers.
	args := []string{}
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keydown", key)
		}
	}
	for _, key := range body.Keys {
		args = append(args, "key", key)
	}
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keyup", key)
		}
	}

	output, err := defaultXdoTool.Run(ctx, args...)
	if err != nil {
		log.Error("xdotool command failed", "err", err, "output", string(output))
		return &executionError{msg: fmt.Sprintf("failed to press keys. out=%s", string(output))}
	}
	return nil
}

func (s *ApiService) PressKey(ctx context.Context, request oapi.PressKeyRequestObject) (oapi.PressKeyResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.PressKey400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	if err := s.doPressKey(ctx, *request.Body); err != nil {
		if isValidationErr(err) {
			return oapi.PressKey400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		return oapi.PressKey500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}
	return oapi.PressKey200Response{}, nil
}

func (s *ApiService) doScroll(ctx context.Context, body oapi.ScrollRequest) error {
	log := logger.FromContext(ctx)

	// Validate deltas
	if (body.DeltaX == nil || *body.DeltaX == 0) && (body.DeltaY == nil || *body.DeltaY == 0) {
		return &validationError{msg: "at least one of delta_x or delta_y must be non-zero"}
	}

	// Bounds check
	screenWidth, screenHeight, _, err := s.getCurrentResolution(ctx)
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return &executionError{msg: "failed to get current display resolution"}
	}
	if body.X < 0 || body.Y < 0 {
		return &validationError{msg: "coordinates must be non-negative"}
	}
	if body.X >= screenWidth || body.Y >= screenHeight {
		return &validationError{msg: fmt.Sprintf("coordinates exceed screen bounds (max: %dx%d)", screenWidth-1, screenHeight-1)}
	}

	args := []string{}
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keydown", key)
		}
	}
	args = append(args, "mousemove", strconv.Itoa(body.X), strconv.Itoa(body.Y))

	// Apply vertical ticks first (sequential as specified)
	if body.DeltaY != nil && *body.DeltaY != 0 {
		count := *body.DeltaY
		btn := "5" // down
		if count < 0 {
			btn = "4" // up
			count = -count
		}
		args = append(args, "click", "--repeat", strconv.Itoa(count), "--delay", "0", btn)
	}
	// Then horizontal ticks
	if body.DeltaX != nil && *body.DeltaX != 0 {
		count := *body.DeltaX
		btn := "7" // right
		if count < 0 {
			btn = "6" // left
			count = -count
		}
		args = append(args, "click", "--repeat", strconv.Itoa(count), "--delay", "0", btn)
	}

	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args = append(args, "keyup", key)
		}
	}

	log.Info("executing xdotool", "args", args)
	output, err := defaultXdoTool.Run(ctx, args...)
	if err != nil {
		log.Error("xdotool scroll failed", "err", err, "output", string(output))
		return &executionError{msg: fmt.Sprintf("failed to perform scroll: %s", string(output))}
	}
	return nil
}

func (s *ApiService) Scroll(ctx context.Context, request oapi.ScrollRequestObject) (oapi.ScrollResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.Scroll400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	if err := s.doScroll(ctx, *request.Body); err != nil {
		if isValidationErr(err) {
			return oapi.Scroll400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		return oapi.Scroll500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}
	return oapi.Scroll200Response{}, nil
}

func (s *ApiService) doDragMouse(ctx context.Context, body oapi.DragMouseRequest) error {
	log := logger.FromContext(ctx)

	if len(body.Path) < 2 {
		return &validationError{msg: "path must contain at least two points"}
	}

	// Bounds check for all points
	screenWidth, screenHeight, _, err := s.getCurrentResolution(ctx)
	if err != nil {
		log.Error("failed to get current resolution", "error", err)
		return &executionError{msg: "failed to get current display resolution"}
	}
	for i, pt := range body.Path {
		if len(pt) != 2 {
			return &validationError{msg: fmt.Sprintf("path[%d] must be [x,y]", i)}
		}
		x := pt[0]
		y := pt[1]
		if x < 0 || y < 0 {
			return &validationError{msg: "coordinates must be non-negative"}
		}
		if x >= screenWidth || y >= screenHeight {
			return &validationError{msg: fmt.Sprintf("coordinates exceed screen bounds (max: %dx%d)", screenWidth-1, screenHeight-1)}
		}
	}

	// Button mapping; default to left if unspecified
	btn := "1"
	if body.Button != nil {
		switch *body.Button {
		case oapi.DragMouseRequestButtonLeft:
			btn = "1"
		case oapi.DragMouseRequestButtonMiddle:
			btn = "2"
		case oapi.DragMouseRequestButtonRight:
			btn = "3"
		default:
			return &validationError{msg: fmt.Sprintf("unsupported button: %s", *body.Button)}
		}
	}

	// Phase 1: keydown modifiers, move to start, mousedown
	args1 := []string{}
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args1 = append(args1, "keydown", key)
		}
	}
	start := body.Path[0]
	args1 = append(args1, "mousemove", strconv.Itoa(start[0]), strconv.Itoa(start[1]))
	args1 = append(args1, "mousedown", btn)
	log.Info("executing xdotool (drag start)", "args", args1)
	if output, err := defaultXdoTool.Run(ctx, args1...); err != nil {
		log.Error("xdotool drag start failed", "err", err, "output", string(output))
		// Best-effort release modifiers
		if body.HoldKeys != nil {
			argsCleanup := []string{}
			for _, key := range *body.HoldKeys {
				argsCleanup = append(argsCleanup, "keyup", key)
			}
			_, _ = defaultXdoTool.Run(ctx, argsCleanup...)
		}
		return &executionError{msg: fmt.Sprintf("failed to start drag: %s", string(output))}
	}

	// Optional delay between mousedown and movement
	if body.Delay != nil && *body.Delay > 0 {
		time.Sleep(time.Duration(*body.Delay) * time.Millisecond)
	}

	// Phase 2: move along path (excluding first point) using fixed-count relative steps
	// Insert a small delay between each relative move to smooth the drag
	args2 := []string{}
	// Determine per-segment steps and per-step delay from request (with defaults)
	stepsPerSegment := 10
	if body.StepsPerSegment != nil && *body.StepsPerSegment >= 1 {
		stepsPerSegment = *body.StepsPerSegment
	}
	stepDelayMs := 50
	if body.StepDelayMs != nil && *body.StepDelayMs >= 0 {
		stepDelayMs = *body.StepDelayMs
	}
	stepDelaySeconds := fmt.Sprintf("%.3f", float64(stepDelayMs)/1000.0)

	// Precompute total number of relative steps so we can avoid a trailing sleep
	totalSteps := 0
	prev := start
	for _, pt := range body.Path[1:] {
		x0, y0 := prev[0], prev[1]
		x1, y1 := pt[0], pt[1]
		totalSteps += len(generateRelativeSteps(x1-x0, y1-y0, stepsPerSegment))
		prev = pt
	}

	prev = start
	stepIndex := 0
	for _, pt := range body.Path[1:] {
		x0, y0 := prev[0], prev[1]
		x1, y1 := pt[0], pt[1]
		for _, step := range generateRelativeSteps(x1-x0, y1-y0, stepsPerSegment) {
			xStr := strconv.Itoa(step[0])
			yStr := strconv.Itoa(step[1])
			if step[0] < 0 || step[1] < 0 {
				args2 = append(args2, "mousemove_relative", "--", xStr, yStr)
			} else {
				args2 = append(args2, "mousemove_relative", xStr, yStr)
			}
			// add a tiny delay between moves, but not after the last step
			if stepIndex < totalSteps-1 && stepDelayMs > 0 {
				args2 = append(args2, "sleep", stepDelaySeconds)
			}
			stepIndex++
		}
		prev = pt
	}
	if len(args2) > 0 {
		log.Info("executing xdotool (drag move)", "args", args2)
		if output, err := defaultXdoTool.Run(ctx, args2...); err != nil {
			log.Error("xdotool drag move failed", "err", err, "output", string(output))
			// Try to release button and modifiers
			argsCleanup := []string{"mouseup", btn}
			if body.HoldKeys != nil {
				for _, key := range *body.HoldKeys {
					argsCleanup = append(argsCleanup, "keyup", key)
				}
			}
			_, _ = defaultXdoTool.Run(ctx, argsCleanup...)
			return &executionError{msg: fmt.Sprintf("failed during drag movement: %s", string(output))}
		}
	}

	// Phase 3: mouseup and release modifiers
	args3 := []string{"mouseup", btn}
	if body.HoldKeys != nil {
		for _, key := range *body.HoldKeys {
			args3 = append(args3, "keyup", key)
		}
	}
	log.Info("executing xdotool (drag end)", "args", args3)
	if output, err := defaultXdoTool.Run(ctx, args3...); err != nil {
		log.Error("xdotool drag end failed", "err", err, "output", string(output))
		return &executionError{msg: fmt.Sprintf("failed to finish drag: %s", string(output))}
	}

	return nil
}

func (s *ApiService) DragMouse(ctx context.Context, request oapi.DragMouseRequestObject) (oapi.DragMouseResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.DragMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"}}, nil
	}
	if err := s.doDragMouse(ctx, *request.Body); err != nil {
		if isValidationErr(err) {
			return oapi.DragMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		return oapi.DragMouse500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}
	return oapi.DragMouse200Response{}, nil
}

const maxSleepDurationMs = 30_000

func (s *ApiService) doSleep(ctx context.Context, body oapi.SleepAction) error {
	if body.DurationMs < 0 {
		return &validationError{msg: "duration_ms must be >= 0"}
	}
	if body.DurationMs > maxSleepDurationMs {
		return &validationError{msg: fmt.Sprintf("duration_ms must be <= %d", maxSleepDurationMs)}
	}

	timer := time.NewTimer(time.Duration(body.DurationMs) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return &executionError{msg: fmt.Sprintf("sleep interrupted: %s", ctx.Err())}
	}
}

func (s *ApiService) BatchComputerAction(ctx context.Context, request oapi.BatchComputerActionRequestObject) (oapi.BatchComputerActionResponseObject, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	if request.Body == nil {
		return oapi.BatchComputerAction400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}

	actions := request.Body.Actions
	if len(actions) == 0 {
		return oapi.BatchComputerAction400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "actions must contain at least one action"},
		}, nil
	}

	for i, action := range actions {
		var err error
		switch action.Type {
		case oapi.ClickMouse:
			if action.ClickMouse == nil {
				err = &validationError{msg: "click_mouse field is required when type is click_mouse"}
			} else {
				err = s.doClickMouse(ctx, *action.ClickMouse)
			}
		case oapi.MoveMouse:
			if action.MoveMouse == nil {
				err = &validationError{msg: "move_mouse field is required when type is move_mouse"}
			} else {
				err = s.doMoveMouse(ctx, *action.MoveMouse)
			}
		case oapi.TypeText:
			if action.TypeText == nil {
				err = &validationError{msg: "type_text field is required when type is type_text"}
			} else {
				err = s.doTypeText(ctx, *action.TypeText)
			}
		case oapi.PressKey:
			if action.PressKey == nil {
				err = &validationError{msg: "press_key field is required when type is press_key"}
			} else {
				err = s.doPressKey(ctx, *action.PressKey)
			}
		case oapi.Scroll:
			if action.Scroll == nil {
				err = &validationError{msg: "scroll field is required when type is scroll"}
			} else {
				err = s.doScroll(ctx, *action.Scroll)
			}
		case oapi.DragMouse:
			if action.DragMouse == nil {
				err = &validationError{msg: "drag_mouse field is required when type is drag_mouse"}
			} else {
				err = s.doDragMouse(ctx, *action.DragMouse)
			}
		case oapi.SetCursor:
			if action.SetCursor == nil {
				err = &validationError{msg: "set_cursor field is required when type is set_cursor"}
			} else {
				err = s.doSetCursor(ctx, *action.SetCursor)
			}
		case oapi.Sleep:
			if action.Sleep == nil {
				err = &validationError{msg: "sleep field is required when type is sleep"}
			} else {
				err = s.doSleep(ctx, *action.Sleep)
			}
		default:
			err = &validationError{msg: fmt.Sprintf("unsupported action type: %s", action.Type)}
		}

		if err != nil {
			msg := fmt.Sprintf("actions[%d] (%s): %s", i, action.Type, err.Error())
			if isValidationErr(err) {
				return oapi.BatchComputerAction400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: msg}}, nil
			}
			return oapi.BatchComputerAction500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: msg}}, nil
		}
	}

	return oapi.BatchComputerAction200Response{}, nil
}

// generateRelativeSteps produces a sequence of relative steps that approximate a
// straight line from (0,0) to (dx,dy) using at most the provided number of
// steps. Each returned element is a pair {stepX, stepY}. The steps are
// distributed so that the cumulative sum equals exactly (dx, dy). If dx and dy
// are both zero, no steps are returned. If the requested step count is less
// than the distance, the per-step movement will be greater than one pixel.
func generateRelativeSteps(dx, dy, steps int) [][2]int {
	if steps <= 0 {
		return nil
	}
	if dx == 0 && dy == 0 {
		return nil
	}

	out := make([][2]int, 0, steps)

	// Use cumulative rounding to distribute integers across the requested
	// number of steps while preserving the exact totals.
	prevCX := 0
	prevCY := 0
	for i := 1; i <= steps; i++ {
		// Target cumulative positions after i steps
		cx := int(math.Round(float64(i*dx) / float64(steps)))
		cy := int(math.Round(float64(i*dy) / float64(steps)))
		sx := cx - prevCX
		sy := cy - prevCY
		prevCX = cx
		prevCY = cy
		out = append(out, [2]int{sx, sy})
	}

	return out
}
