package api

import (
	"context"
	"fmt"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func (s *ApiService) waylandViewport(ctx context.Context) (int, int, error) {
	var size cdpclient.ViewportSize
	err := s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		var err error
		size, err = client.GetViewportSize(cdpCtx)
		return err
	})
	if err != nil {
		return 0, 0, fmt.Errorf("get Wayland viewport: %w", err)
	}
	if size.Width <= 0 || size.Height <= 0 {
		return 0, 0, fmt.Errorf("invalid Wayland viewport %dx%d", size.Width, size.Height)
	}
	return size.Width, size.Height, nil
}

func validateWaylandPoint(x, y, width, height int) error {
	if x < 0 || y < 0 {
		return &validationError{msg: "coordinates must be non-negative"}
	}
	if x >= width || y >= height {
		return &validationError{msg: fmt.Sprintf("coordinates exceed screen bounds (max: %dx%d)", width-1, height-1)}
	}
	return nil
}

func (s *ApiService) dispatchWaylandMouse(ctx context.Context, eventType string, x, y int, button string, clickCount int) error {
	return s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		return client.DispatchMouseEvent(cdpCtx, eventType, float64(x), float64(y), button, clickCount)
	})
}

func (s *ApiService) doMoveMouseWayland(ctx context.Context, body oapi.MoveMouseRequest) error {
	width, height, err := s.waylandViewport(ctx)
	if err != nil {
		return &executionError{msg: err.Error()}
	}
	if err := validateWaylandPoint(body.X, body.Y, width, height); err != nil {
		return err
	}
	if err := s.dispatchWaylandMouse(ctx, "mouseMoved", body.X, body.Y, "", 0); err != nil {
		return &executionError{msg: fmt.Sprintf("failed to move Wayland pointer: %v", err)}
	}
	return nil
}

func (s *ApiService) doClickMouseWayland(ctx context.Context, body oapi.ClickMouseRequest) error {
	width, height, err := s.waylandViewport(ctx)
	if err != nil {
		return &executionError{msg: err.Error()}
	}
	if err := validateWaylandPoint(body.X, body.Y, width, height); err != nil {
		return err
	}
	button := "left"
	if body.Button != nil {
		button = map[oapi.ClickMouseRequestButton]string{
			oapi.ClickMouseRequestButtonLeft:    "left",
			oapi.ClickMouseRequestButtonMiddle:  "middle",
			oapi.ClickMouseRequestButtonRight:   "right",
			oapi.ClickMouseRequestButtonBack:    "back",
			oapi.ClickMouseRequestButtonForward: "forward",
		}[*body.Button]
		if button == "" {
			return &validationError{msg: fmt.Sprintf("unsupported button: %s", *body.Button)}
		}
	}
	clickCount := 1
	if body.NumClicks != nil && *body.NumClicks > 0 {
		clickCount = *body.NumClicks
	}
	if body.ClickType != nil && *body.ClickType != oapi.Click {
		return &validationError{msg: "Wayland input currently supports click only"}
	}
	if err := s.dispatchWaylandMouse(ctx, "mousePressed", body.X, body.Y, button, clickCount); err != nil {
		return &executionError{msg: fmt.Sprintf("failed to press Wayland pointer: %v", err)}
	}
	if err := s.dispatchWaylandMouse(ctx, "mouseReleased", body.X, body.Y, button, clickCount); err != nil {
		return &executionError{msg: fmt.Sprintf("failed to release Wayland pointer: %v", err)}
	}
	return nil
}

func (s *ApiService) doTypeTextWayland(ctx context.Context, body oapi.TypeTextRequest) error {
	return s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		if err := client.InsertText(cdpCtx, body.Text); err != nil {
			return &executionError{msg: fmt.Sprintf("failed to type text through Wayland: %v", err)}
		}
		return nil
	})
}

func (s *ApiService) doPressKeyWayland(ctx context.Context, body oapi.PressKeyRequest) error {
	if len(body.Keys) == 0 {
		return &validationError{msg: "keys must contain at least one key symbol"}
	}
	for _, key := range body.Keys {
		code := key
		if len([]rune(key)) == 1 {
			code = "Key" + key
		}
		if err := s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
			if err := client.DispatchKeyEvent(cdpCtx, "keyDown", key, code, ""); err != nil {
				return err
			}
			return client.DispatchKeyEvent(cdpCtx, "keyUp", key, code, "")
		}); err != nil {
			return &executionError{msg: fmt.Sprintf("failed to press Wayland key %q: %v", key, err)}
		}
	}
	return nil
}

func (s *ApiService) doScrollWayland(ctx context.Context, body oapi.ScrollRequest) error {
	width, height, err := s.waylandViewport(ctx)
	if err != nil {
		return &executionError{msg: err.Error()}
	}
	if err := validateWaylandPoint(body.X, body.Y, width, height); err != nil {
		return err
	}
	deltaX, deltaY := 0, 0
	if body.DeltaX != nil {
		deltaX = *body.DeltaX
	}
	if body.DeltaY != nil {
		deltaY = *body.DeltaY
	}
	if deltaX == 0 && deltaY == 0 {
		return &validationError{msg: "at least one of delta_x or delta_y must be non-zero"}
	}
	return s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		return client.DispatchMouseWheel(cdpCtx, float64(body.X), float64(body.Y), deltaX, deltaY)
	})
}

func (s *ApiService) doDragMouseWayland(ctx context.Context, body oapi.DragMouseRequest) error {
	if len(body.Path) < 2 {
		return &validationError{msg: "path must contain at least two points"}
	}
	width, height, err := s.waylandViewport(ctx)
	if err != nil {
		return &executionError{msg: err.Error()}
	}
	for _, point := range body.Path {
		if len(point) != 2 {
			return &validationError{msg: "path points must be [x,y]"}
		}
		if err := validateWaylandPoint(point[0], point[1], width, height); err != nil {
			return err
		}
	}
	button := "left"
	if body.Button != nil {
		button = map[oapi.DragMouseRequestButton]string{
			oapi.DragMouseRequestButtonLeft:   "left",
			oapi.DragMouseRequestButtonMiddle: "middle",
			oapi.DragMouseRequestButtonRight:  "right",
		}[*body.Button]
		if button == "" {
			return &validationError{msg: fmt.Sprintf("unsupported button: %s", *body.Button)}
		}
	}
	start := body.Path[0]
	if err := s.dispatchWaylandMouse(ctx, "mousePressed", start[0], start[1], button, 1); err != nil {
		return &executionError{msg: fmt.Sprintf("failed to press Wayland pointer: %v", err)}
	}
	for _, point := range body.Path[1:] {
		if err := s.dispatchWaylandMouse(ctx, "mouseMoved", point[0], point[1], "", 0); err != nil {
			_ = s.dispatchWaylandMouse(context.Background(), "mouseReleased", point[0], point[1], button, 1)
			return &executionError{msg: fmt.Sprintf("failed during Wayland drag: %v", err)}
		}
	}
	if err := s.dispatchWaylandMouse(ctx, "mouseReleased", body.Path[len(body.Path)-1][0], body.Path[len(body.Path)-1][1], button, 1); err != nil {
		return &executionError{msg: fmt.Sprintf("failed to release Wayland pointer: %v", err)}
	}
	return nil
}
