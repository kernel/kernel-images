package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/onkernel/kernel-images/server/lib/logger"
)

// StartCapture handles POST /events/start.
// Generates a new capture session ID, seeds the pipeline, then starts the
// CDP monitor. Idempotent: if already running, the monitor is stopped and
// restarted with a fresh session ID.
func (s *ApiService) StartCapture(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	s.pipeline.Start(uuid.New().String())

	if err := s.monitor.Start(r.Context()); err != nil {
		logger.FromContext(r.Context()).Error("failed to start CDP monitor", "err", err)
		http.Error(w, "failed to start capture", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// StopCapture handles POST /events/stop. Idempotent if not running.
func (s *ApiService) StopCapture(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	s.monitor.Stop()
	w.WriteHeader(http.StatusOK)
}
