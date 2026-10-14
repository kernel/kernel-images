package api

import (
	"context"
	"strconv"

	"github.com/kernel/kernel-images/server/lib/ambientmouse"
	"github.com/kernel/kernel-images/server/lib/devtoolsproxy"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// AmbientMouse is the guard the CDP proxy holds client input behind.
func (s *ApiService) AmbientMouse() devtoolsproxy.InputGuard {
	return s.ambientMouse
}

func (s *ApiService) GetAmbientMouse(ctx context.Context, request oapi.GetAmbientMouseRequestObject) (oapi.GetAmbientMouseResponseObject, error) {
	return oapi.GetAmbientMouse200JSONResponse{Enabled: s.ambientMouse.Enabled()}, nil
}

func (s *ApiService) SetAmbientMouse(ctx context.Context, request oapi.SetAmbientMouseRequestObject) (oapi.SetAmbientMouseResponseObject, error) {
	if request.Body == nil {
		return oapi.SetAmbientMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	// Headless Chromium takes no input from the X display, so the page would
	// never see the moves.
	if request.Body.Enabled && s.detectDisplayMode(ctx) == "xvfb" {
		return oapi.SetAmbientMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "ambient mouse movement is not supported on headless browsers"},
		}, nil
	}
	s.ambientMouse.SetEnabled(request.Body.Enabled)
	return oapi.SetAmbientMouse200JSONResponse{Enabled: s.ambientMouse.Enabled()}, nil
}

// lockAgentInput takes inputMu for an agent input action. The action first
// waits until the current page has had its ambient pointer movement, and
// ambient movement then stays paused until the action has finished.
func (s *ApiService) lockAgentInput(ctx context.Context) func() {
	s.ambientMouse.Await(ctx)
	s.inputMu.Lock()
	s.ambientMouse.AgentPointer(ambientmouse.PointerMove)
	return func() {
		s.ambientMouse.AgentPointer(ambientmouse.PointerMove)
		s.inputMu.Unlock()
	}
}

// xdoPointer drives the X pointer through xdotool, the same way the computer
// input endpoints do.
type xdoPointer struct{ s *ApiService }

func (p xdoPointer) Location(ctx context.Context) (int, int, error) {
	return p.s.getMouseLocation(ctx)
}

func (p xdoPointer) MoveRelative(ctx context.Context, dx, dy int) error {
	_, err := defaultXdoTool.Run(ctx, "mousemove_relative", "--", strconv.Itoa(dx), strconv.Itoa(dy))
	return err
}

func (p xdoPointer) ScreenSize(ctx context.Context) (int, int, error) {
	width, height, _, err := p.s.getCurrentResolution(ctx)
	return width, height, err
}
