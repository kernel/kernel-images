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
	return s.dispatchWaylandMouseEvents(ctx, []cdpclient.MouseEvent{{
		Type:       eventType,
		X:          float64(x),
		Y:          float64(y),
		Button:     button,
		ClickCount: clickCount,
	}})
}

func (s *ApiService) dispatchWaylandMouseEvents(ctx context.Context, events []cdpclient.MouseEvent) error {
	return s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		return client.DispatchMouseEvents(cdpCtx, events)
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
	if err := s.dispatchWaylandMouseEvents(ctx, []cdpclient.MouseEvent{
		{Type: "mousePressed", X: float64(body.X), Y: float64(body.Y), Button: button, ClickCount: clickCount},
		{Type: "mouseReleased", X: float64(body.X), Y: float64(body.Y), Button: button, ClickCount: clickCount},
	}); err != nil {
		return &executionError{msg: fmt.Sprintf("failed to click through Wayland: %v", err)}
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
	events := make([]cdpclient.KeyEvent, 0, len(body.Keys)*2)
	for _, key := range body.Keys {
		code := key
		if len([]rune(key)) == 1 {
			code = "Key" + key
		}
		events = append(events,
			cdpclient.KeyEvent{Type: "keyDown", Key: key, Code: code},
			cdpclient.KeyEvent{Type: "keyUp", Key: key, Code: code},
		)
	}
	if err := s.withCDPClient(ctx, func(cdpCtx context.Context, client *cdpclient.Client) error {
		return client.DispatchKeyEvents(cdpCtx, events)
	}); err != nil {
		return &executionError{msg: fmt.Sprintf("failed to press Wayland key: %v", err)}
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
	events := make([]cdpclient.MouseEvent, 0, len(body.Path)+1)
	start := body.Path[0]
	events = append(events, cdpclient.MouseEvent{Type: "mousePressed", X: float64(start[0]), Y: float64(start[1]), Button: button, ClickCount: 1})
	for _, point := range body.Path[1:] {
		events = append(events, cdpclient.MouseEvent{Type: "mouseMoved", X: float64(point[0]), Y: float64(point[1])})
	}
	end := body.Path[len(body.Path)-1]
	events = append(events, cdpclient.MouseEvent{Type: "mouseReleased", X: float64(end[0]), Y: float64(end[1]), Button: button, ClickCount: 1})
	if err := s.dispatchWaylandMouseEvents(ctx, events); err != nil {
		return &executionError{msg: fmt.Sprintf("failed during Wayland drag: %v", err)}
	}
	return nil
}
