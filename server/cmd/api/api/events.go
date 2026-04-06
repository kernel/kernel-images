package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/onkernel/kernel-images/server/lib/events"
	"github.com/onkernel/kernel-images/server/lib/logger"
)

// StartCapture handles POST /events/start. Restarts if already running.
func (s *ApiService) StartCapture(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	captureSessionID := uuid.New().String()
	s.captureSession.Start(captureSessionID)

	if err := s.cdpMonitor.Start(context.Background()); err != nil {
		logger.FromContext(r.Context()).Error("failed to start CDP monitor", "err", err)
		http.Error(w, "failed to start capture", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// StopCapture handles POST /events/stop. No-op if not running.
func (s *ApiService) StopCapture(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	s.cdpMonitor.Stop()
	w.WriteHeader(http.StatusOK)
}

// PublishEvent handles POST /events/publish.
// Defaults Category (via CategoryFor) and Source.Kind (to KindKernelAPI) when omitted.
func (s *ApiService) PublishEvent(w http.ResponseWriter, r *http.Request) {
	var ev events.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if ev.Type == "" {
		http.Error(w, "type is required", http.StatusBadRequest)
		return
	}

	if ev.Category == "" {
		ev.Category = events.CategoryFor(ev.Type)
	}

	if ev.Source.Kind == "" {
		ev.Source.Kind = events.KindKernelAPI
	}

	s.captureSession.Publish(ev)
	w.WriteHeader(http.StatusOK)
}

// StreamEvents handles GET /events/stream (SSE).
// Supports Last-Event-ID for reconnection.
func (s *ApiService) StreamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	var lastSeq uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			lastSeq = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	reader := s.captureSession.NewReader(lastSeq)
	ctx := r.Context()

	for {
		res, err := reader.Read(ctx)
		if err != nil {
			return
		}
		if res.Envelope == nil {
			continue
		}
		if err := writeSSEEnvelope(w, *res.Envelope); err != nil {
			return
		}
		flusher.Flush()
	}
}

// writeSSEEnvelope writes a single SSE frame: "id: {seq}\ndata: {json}\n\n".
func writeSSEEnvelope(w io.Writer, env events.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", env.Seq, data)
	return err
}
