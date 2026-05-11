package api

import (
	"context"
	"fmt"
	"sort"

	"github.com/nrednav/cuid2"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"

	"github.com/kernel/kernel-images/server/lib/capturesession"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/logger"
)

// StartCaptureSession handles POST /events/capture_session.
// Returns 409 if a session is already active.
func (s *ApiService) StartCaptureSession(ctx context.Context, req oapi.StartCaptureSessionRequestObject) (oapi.StartCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() != "" {
		return oapi.StartCaptureSession409JSONResponse{ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{Message: "a capture session is already active"}}, nil
	}

	cfg, err := captureConfigFrom(req.Body)
	if err != nil {
		return oapi.StartCaptureSession400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}

	id := cuid2.Generate()
	s.captureSession.Start(id, cfg)

	if err := s.cdpMonitor.Start(s.lifecycleCtx); err != nil {
		// Roll back: clear the session so a retry can succeed.
		s.captureSession.Stop()
		logger.FromContext(ctx).Error("failed to start capture monitor", "err", err)
		return oapi.StartCaptureSession500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start capture"}}, nil
	}

	return oapi.StartCaptureSession201JSONResponse(s.buildSessionResponse()), nil
}

// GetCaptureSession handles GET /events/capture_session.
func (s *ApiService) GetCaptureSession(_ context.Context, _ oapi.GetCaptureSessionRequestObject) (oapi.GetCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() == "" {
		return oapi.GetCaptureSession404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "no active capture session"}}, nil
	}
	return oapi.GetCaptureSession200JSONResponse(s.buildSessionResponse()), nil
}

// UpdateCaptureSession handles PATCH /events/capture_session.
func (s *ApiService) UpdateCaptureSession(_ context.Context, req oapi.UpdateCaptureSessionRequestObject) (oapi.UpdateCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() == "" {
		return oapi.UpdateCaptureSession404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "no active capture session"}}, nil
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

// StopCaptureSession handles DELETE /events/capture_session.
// Stops the capture session and clears it so a new one can be started.
func (s *ApiService) StopCaptureSession(_ context.Context, _ oapi.StopCaptureSessionRequestObject) (oapi.StopCaptureSessionResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if s.captureSession.ID() == "" {
		return oapi.StopCaptureSession404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "no active capture session"}}, nil
	}

	s.cdpMonitor.Stop()
	// Snapshot the final state before clearing the session ID so buildSessionResponse
	// can still parse it. Force the status to Stopped because cdpMonitor.Stop may
	// tear down asynchronously, leaving IsRunning briefly true.
	resp := s.buildSessionResponse()
	resp.Status = oapi.CaptureSessionStatusStopped
	s.captureSession.Stop()

	return oapi.StopCaptureSession200JSONResponse(resp), nil
}

// buildSessionResponse constructs the CaptureSession response from current state.
func (s *ApiService) buildSessionResponse() oapi.CaptureSession {
	cfg := s.captureSession.Config()

	cats := make([]oapi.CaptureConfigCategories, len(cfg.Categories))
	for i, c := range cfg.Categories {
		cats[i] = oapi.CaptureConfigCategories(c)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i] < cats[j] })

	status := oapi.CaptureSessionStatusStopped
	if s.cdpMonitor.IsRunning() {
		status = oapi.CaptureSessionStatusRunning
	}

	return oapi.CaptureSession{
		Id:     s.captureSession.ID(),
		Status: status,
		Config: oapi.CaptureConfig{
			Categories: &cats,
		},
		Seq:       int64(s.captureSession.Seq()),
		CreatedAt: s.captureSession.CreatedAt(),
	}
}

// captureConfigFrom converts the optional StartCaptureSessionRequest body
// into a capturesession.CaptureConfig.
func captureConfigFrom(body *oapi.StartCaptureSessionRequest) (capturesession.CaptureConfig, error) {
	if body == nil {
		return capturesession.CaptureConfig{}, nil
	}
	return captureConfigFromOAPI(body.Config)
}

// captureConfigFromOAPI converts an oapi.CaptureConfig to capturesession.CaptureConfig.
func captureConfigFromOAPI(cfg *oapi.CaptureConfig) (capturesession.CaptureConfig, error) {
	if cfg == nil || cfg.Categories == nil {
		return capturesession.CaptureConfig{}, nil
	}
	out := capturesession.CaptureConfig{
		Categories: make([]events.EventCategory, 0, len(*cfg.Categories)),
	}
	for _, c := range *cfg.Categories {
		cat := events.EventCategory(c)
		if !events.ValidCategory(cat) {
			return capturesession.CaptureConfig{}, fmt.Errorf("unknown category: %q", c)
		}
		out.Categories = append(out.Categories, cat)
	}
	return out, nil
}
