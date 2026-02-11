package api

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
)

// ambientAction identifies the type of ambient event to emit.
type ambientAction int

const (
	ambientMouseDrift ambientAction = iota
	ambientScroll
	ambientMicroDrag
	ambientClick
	ambientKeyTap
)

// ambientConfig holds the resolved configuration for the ambient mouse loop.
type ambientConfig struct {
	minIntervalMs int
	maxIntervalMs int
	weights       []struct {
		action ambientAction
		weight int
	}
	totalWeight int
}

func (s *ApiService) SetAmbientMouse(ctx context.Context, request oapi.SetAmbientMouseRequestObject) (oapi.SetAmbientMouseResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.SetAmbientMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: "request body is required"},
		}, nil
	}
	body := *request.Body

	s.inputMu.Lock()

	// Stop any running ambient loop first.
	if s.ambientCancel != nil {
		s.ambientCancel()
		s.ambientCancel = nil
	}

	if !body.Enabled {
		s.inputMu.Unlock()
		log.Info("ambient mouse disabled")
		return oapi.SetAmbientMouse200JSONResponse(oapi.AmbientMouseResponse{Enabled: false}), nil
	}

	// Resolve configuration with defaults.
	cfg, err := resolveAmbientConfig(body)
	if err != nil {
		s.inputMu.Unlock()
		return oapi.SetAmbientMouse400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
			Message: err.Error()},
		}, nil
	}

	ambientCtx, cancel := context.WithCancel(context.Background())
	s.ambientCancel = cancel
	s.inputMu.Unlock()

	go s.runAmbientLoop(ambientCtx, cfg)

	log.Info("ambient mouse enabled",
		"min_interval_ms", cfg.minIntervalMs,
		"max_interval_ms", cfg.maxIntervalMs,
	)
	return oapi.SetAmbientMouse200JSONResponse(oapi.AmbientMouseResponse{Enabled: true}), nil
}

// resolveAmbientConfig builds an ambientConfig from the request body, applying defaults.
func resolveAmbientConfig(body oapi.AmbientMouseRequest) (ambientConfig, error) {
	cfg := ambientConfig{
		minIntervalMs: 200,
		maxIntervalMs: 600,
	}
	if body.MinIntervalMs != nil {
		cfg.minIntervalMs = *body.MinIntervalMs
	}
	if body.MaxIntervalMs != nil {
		cfg.maxIntervalMs = *body.MaxIntervalMs
	}
	if cfg.minIntervalMs > cfg.maxIntervalMs {
		return cfg, fmt.Errorf("min_interval_ms must be <= max_interval_ms")
	}

	driftW := 55
	scrollW := 20
	microDragW := 12
	clickW := 10
	keyTapW := 3
	if body.MouseDriftWeight != nil {
		driftW = *body.MouseDriftWeight
	}
	if body.ScrollWeight != nil {
		scrollW = *body.ScrollWeight
	}
	if body.MicroDragWeight != nil {
		microDragW = *body.MicroDragWeight
	}
	if body.ClickWeight != nil {
		clickW = *body.ClickWeight
	}
	if body.KeyTapWeight != nil {
		keyTapW = *body.KeyTapWeight
	}

	cfg.weights = []struct {
		action ambientAction
		weight int
	}{
		{ambientMouseDrift, driftW},
		{ambientScroll, scrollW},
		{ambientMicroDrag, microDragW},
		{ambientClick, clickW},
		{ambientKeyTap, keyTapW},
	}
	for _, w := range cfg.weights {
		cfg.totalWeight += w.weight
	}
	if cfg.totalWeight == 0 {
		return cfg, fmt.Errorf("at least one action weight must be > 0")
	}
	return cfg, nil
}

// runAmbientLoop is the background goroutine that emits diverse input events.
// It acquires inputMu for each action, so it cooperates with explicit API calls.
func (s *ApiService) runAmbientLoop(ctx context.Context, cfg ambientConfig) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		action := pickAmbientAction(r, cfg)
		s.inputMu.Lock()
		s.execAmbientAction(ctx, r, action)
		s.inputMu.Unlock()

		// Random delay between events.
		delayMs := cfg.minIntervalMs + r.Intn(cfg.maxIntervalMs-cfg.minIntervalMs+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(delayMs) * time.Millisecond):
		}
	}
}

func pickAmbientAction(r *rand.Rand, cfg ambientConfig) ambientAction {
	n := r.Intn(cfg.totalWeight)
	for _, w := range cfg.weights {
		if n < w.weight {
			return w.action
		}
		n -= w.weight
	}
	return ambientMouseDrift
}

// execAmbientAction performs a single ambient event via xdotool. Must be called
// with inputMu held.
func (s *ApiService) execAmbientAction(ctx context.Context, r *rand.Rand, action ambientAction) {
	switch action {
	case ambientMouseDrift:
		dx := r.Intn(8) - 4
		dy := r.Intn(8) - 4
		if dx == 0 && dy == 0 {
			dx = 1
		}
		defaultXdoTool.Run(ctx, "mousemove_relative", "--", fmt.Sprintf("%d", dx), fmt.Sprintf("%d", dy))

	case ambientScroll:
		w, h := s.getDisplayGeometry(ctx)
		if w > 0 && h > 0 {
			x := w/2 + r.Intn(80) - 40
			y := h/2 + r.Intn(80) - 40
			defaultXdoTool.Run(ctx, "mousemove", strconv.Itoa(x), strconv.Itoa(y))
			btn := "4"
			if r.Intn(2) == 0 {
				btn = "5"
			}
			defaultXdoTool.Run(ctx, "click", btn)
		}

	case ambientMicroDrag:
		dx := 3 + r.Intn(6)
		dy := 3 + r.Intn(6)
		if r.Intn(2) == 0 {
			dx = -dx
		}
		if r.Intn(2) == 0 {
			dy = -dy
		}
		defaultXdoTool.Run(ctx, "mousedown", "1")
		defaultXdoTool.Run(ctx, "mousemove_relative", "--", fmt.Sprintf("%d", dx), fmt.Sprintf("%d", dy))
		// Use background context so mouseup always fires even if ctx is cancelled,
		// preventing a stuck mouse button.
		defaultXdoTool.Run(context.Background(), "mouseup", "1")

	case ambientClick:
		w, h := s.getDisplayGeometry(ctx)
		if w > 200 && h > 200 {
			pad := 100
			if w < 400 {
				pad = w / 4
			}
			x := pad + r.Intn(max(1, w-2*pad))
			y := pad + r.Intn(max(1, h-2*pad))
			defaultXdoTool.Run(ctx, "mousemove", strconv.Itoa(x), strconv.Itoa(y))
			defaultXdoTool.Run(ctx, "click", "1")
		}

	case ambientKeyTap:
		// Modifier tap; least likely to trigger page behavior.
		defaultXdoTool.Run(ctx, "key", "shift")
	}
}

// getDisplayGeometry returns the current display dimensions via xdotool.
// The result is cached for 30 seconds to avoid shelling out on every ambient event.
func (s *ApiService) getDisplayGeometry(ctx context.Context) (int, int) {
	s.displayGeomMu.Lock()
	defer s.displayGeomMu.Unlock()

	if time.Since(s.displayGeomAt) < 30*time.Second && s.displayGeomW > 0 {
		return s.displayGeomW, s.displayGeomH
	}

	out, err := defaultXdoTool.Run(ctx, "getdisplaygeometry")
	if err != nil {
		return s.displayGeomW, s.displayGeomH
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) >= 2 {
		w, _ := strconv.Atoi(parts[0])
		h, _ := strconv.Atoi(parts[1])
		if w > 0 && h > 0 {
			s.displayGeomW = w
			s.displayGeomH = h
			s.displayGeomAt = time.Now()
			return w, h
		}
	}
	return s.displayGeomW, s.displayGeomH
}
