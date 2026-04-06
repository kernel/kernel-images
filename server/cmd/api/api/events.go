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

// StartCapture handles POST /events/start.
// Registered as a direct chi route (not via OpenAPI spec) because these are
// simple internal control endpoints with no request body.
// A second call while already running restarts capture (stop+start).
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

// StopCapture handles POST /events/stop. Idempotent if not running.
func (s *ApiService) StopCapture(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	s.cdpMonitor.Stop()
	w.WriteHeader(http.StatusOK)
}

// PublishEvent handles POST /events/publish.
// Accepts an Event JSON body and ingests it into the pipeline (ring buffer + log file).
// Derives Category from Type if omitted; stamps KindKernelAPI if Source.Kind is omitted.
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

	// Derive category if caller omitted it — FileWriter returns error for empty category.
	if ev.Category == "" {
		ev.Category = events.CategoryFor(ev.Type)
	}

	// Stamp provenance if caller omitted source kind.
	if ev.Source.Kind == "" {
		ev.Source.Kind = events.KindKernelAPI
	}

	s.captureSession.Publish(ev)
	w.WriteHeader(http.StatusOK)
}

// StreamEvents handles GET /events/stream.
// Delivers a live stream of Envelopes over Server-Sent Events.
// Each frame is formatted as "id: {seq}\ndata: {json}\n\n".
// Clients may reconnect with Last-Event-ID to resume from the next unseen event.
func (s *ApiService) StreamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Parse Last-Event-ID for reconnection (ignore parse errors; default to 0).
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

	// NewReader(lastSeq) positions the reader to deliver events after lastSeq.
	reader := s.captureSession.NewReader(lastSeq)
	ctx := r.Context()

	// Main event loop.
	for {
		res, err := reader.Read(ctx)
		if err != nil {
			// Context cancelled (client disconnected).
			return
		}
		if res.Envelope == nil {
			// Drop notification — skip, client will see the gap via seq discontinuity.
			continue
		}
		if err := writeSSEEnvelope(w, *res.Envelope); err != nil {
			return
		}
		flusher.Flush()
	}
}

// writeSSEEnvelope marshals env to JSON and writes a single SSE frame to w.
// Frame format: "id: {seq}\ndata: {json}\n\n"
func writeSSEEnvelope(w io.Writer, env events.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", env.Seq, data)
	return err
}
