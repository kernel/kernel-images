package api

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"

	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/onkernel/kernel-images/server/lib/logger"
)

// CreateCaptureSession handles POST /events/capture_sessions.
// Returns 409 if a session is already active.
func (s *ApiService) CreateCaptureSession(ctx context.Context, req oapi.CreateCaptureSessionRequestObject) (oapi.CreateCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() != "" {
		return oapi.CreateCaptureSession409JSONResponse{ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{Message: "a capture session is already active"}}, nil
	}

	cfg, err := captureConfigFrom(req.Body)
	if err != nil {
		return oapi.CreateCaptureSession400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}

	id := uuid.New().String()
	s.captureSession.Start(id, cfg)

	if err := s.cdpMonitor.Start(s.lifecycleCtx); err != nil {
		// Roll back: clear the session so a retry can succeed.
		s.captureSession.Stop()
		logger.FromContext(ctx).Error("failed to start capture monitor", "err", err)
		return oapi.CreateCaptureSession500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start capture"}}, nil
	}

	return oapi.CreateCaptureSession201JSONResponse(s.buildSessionResponse()), nil
}

// GetCaptureSession handles GET /events/capture_sessions/{capture_session_id}.
func (s *ApiService) GetCaptureSession(_ context.Context, req oapi.GetCaptureSessionRequestObject) (oapi.GetCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() != req.CaptureSessionId.String() {
		return oapi.GetCaptureSession404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "capture session not found"}}, nil
	}
	return oapi.GetCaptureSession200JSONResponse(s.buildSessionResponse()), nil
}

// UpdateCaptureSession handles PATCH /events/capture_sessions/{capture_session_id}.
func (s *ApiService) UpdateCaptureSession(_ context.Context, req oapi.UpdateCaptureSessionRequestObject) (oapi.UpdateCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() != req.CaptureSessionId.String() {
		return oapi.UpdateCaptureSession404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "capture session not found"}}, nil
	}

	if req.Body != nil && req.Body.Config != nil {
		cfg, err := captureConfigFromOAPI(req.Body.Config)
		if err != nil {
			return oapi.UpdateCaptureSession400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		s.captureSession.UpdateConfig(cfg)
	}

	return oapi.UpdateCaptureSession200JSONResponse(s.buildSessionResponse()), nil
}

// DeleteCaptureSession handles DELETE /events/capture_sessions/{capture_session_id}.
// Stops the capture session and clears it so a new one can be created.
func (s *ApiService) DeleteCaptureSession(_ context.Context, req oapi.DeleteCaptureSessionRequestObject) (oapi.DeleteCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() != req.CaptureSessionId.String() {
		return oapi.DeleteCaptureSession404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "capture session not found"}}, nil
	}

	s.cdpMonitor.Stop()
	// Snapshot the final state before clearing the session ID.
	resp := s.buildSessionResponse()
	resp.Status = oapi.CaptureSessionStatusStopped
	s.captureSession.Stop()

	return oapi.DeleteCaptureSession200JSONResponse(resp), nil
}

// buildSessionResponse constructs the CaptureSession response from current state.
func (s *ApiService) buildSessionResponse() oapi.CaptureSession {
	cfg := s.captureSession.Config()

	cats := make([]oapi.CaptureConfigCategories, len(cfg.Categories))
	for i, c := range cfg.Categories {
		cats[i] = oapi.CaptureConfigCategories(c)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i] < cats[j] })

	var dl *oapi.CaptureConfigDetailLevel
	if cfg.DetailLevel != "" {
		v := oapi.CaptureConfigDetailLevel(cfg.DetailLevel)
		dl = &v
	}

	status := oapi.CaptureSessionStatusStopped
	if s.cdpMonitor.IsRunning() {
		status = oapi.CaptureSessionStatusRunning
	}

	// callers hold monitorMu, guaranteeing ID is a valid UUID.
	id, _ := uuid.Parse(s.captureSession.ID())

	return oapi.CaptureSession{
		Id:     id,
		Status: status,
		Config: oapi.CaptureConfig{
			Categories:  &cats,
			DetailLevel: dl,
		},
		Seq:       int64(s.captureSession.Seq()),
		CreatedAt: s.captureSession.CreatedAt(),
	}
}

// captureConfigFrom converts the optional CreateCaptureSessionRequest body
// into an events.CaptureConfig.
func captureConfigFrom(body *oapi.CreateCaptureSessionRequest) (events.CaptureConfig, error) {
	if body == nil || body.Config == nil {
		return events.CaptureConfig{}, nil
	}
	return captureConfigFromOAPI(body.Config)
}

// captureConfigFromOAPI converts an oapi.CaptureConfig to events.CaptureConfig.
func captureConfigFromOAPI(cfg *oapi.CaptureConfig) (events.CaptureConfig, error) {
	var out events.CaptureConfig
	if cfg.Categories != nil {
		for _, c := range *cfg.Categories {
			cat := events.EventCategory(c)
			if !events.ValidCategory(cat) {
				return events.CaptureConfig{}, fmt.Errorf("unknown category: %q", c)
			}
			out.Categories = append(out.Categories, cat)
		}
	}
	if cfg.DetailLevel != nil {
		if !cfg.DetailLevel.Valid() {
			return events.CaptureConfig{}, fmt.Errorf("unknown detail level: %q", *cfg.DetailLevel)
		}
		out.DetailLevel = events.DetailLevel(*cfg.DetailLevel)
	}
	return out, nil
}
