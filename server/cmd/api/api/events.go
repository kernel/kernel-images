package api

import (
	"context"

	"github.com/google/uuid"
	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
)

// StartCapture handles POST /events/start.
// Generates a new capture session ID, seeds the pipeline, then starts the
// CDP monitor. If already running, the monitor is stopped and
// restarted with a fresh session ID.
func (s *ApiService) StartCapture(ctx context.Context, req oapi.StartCaptureRequestObject) (oapi.StartCaptureResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	cfg := captureConfigFrom(req.Body)
	s.captureSession.Start(uuid.New().String(), cfg)

	if err := s.cdpMonitor.Start(context.Background()); err != nil {
		logger.FromContext(ctx).Error("failed to start CDP monitor", "err", err)
		return oapi.StartCapture500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start capture"}}, nil
	}
	return oapi.StartCapture200Response{}, nil
}

// captureConfigFrom converts the optional API request body into a CaptureConfig.
func captureConfigFrom(body *oapi.StartCaptureRequest) events.CaptureConfig {
	if body == nil {
		return events.CaptureConfig{}
	}
	var cfg events.CaptureConfig
	if body.Categories != nil {
		for _, c := range *body.Categories {
			cfg.Categories = append(cfg.Categories, events.EventCategory(c))
		}
	}
	if body.DetailLevel != nil {
		cfg.DetailLevel = events.DetailLevel(*body.DetailLevel)
	}
	return cfg
}

// StopCapture handles POST /events/stop
func (s *ApiService) StopCapture(ctx context.Context, req oapi.StopCaptureRequestObject) (oapi.StopCaptureResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	s.cdpMonitor.Stop()
	return oapi.StopCapture200Response{}, nil
}
