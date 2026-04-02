package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// PublishEvent handles POST /events/publish.
// Injects a caller-supplied event into the event bus. Returns 400 if the event
// fails validation.
func (s *ApiService) PublishEvent(_ context.Context, req oapi.PublishEventRequestObject) (oapi.PublishEventResponseObject, error) {
	body := req.Body
	if body == nil || body.Type == "" {
		return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "type is required"}}, nil
	}
	ev := events.Event{Type: body.Type}

	ev.Ts = time.Now().UnixMicro()
	if body.Category != nil {
		cat := events.EventCategory(*body.Category)
		if !events.ValidCategory(cat) {
			return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid category"}}, nil
		}
		ev.Category = cat
	} else {
		ev.Category = events.CategorySystem
	}

	if body.Source != nil {
		if body.Source.Kind != nil {
			if *body.Source.Kind == oapi.KernelApi {
				return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "source.kind kernel_api is reserved for server-generated events"}}, nil
			}
			ev.Source.Kind = events.SourceKind(*body.Source.Kind)
		}
		if body.Source.Event != nil {
			ev.Source.Event = *body.Source.Event
		}
		if body.Source.Metadata != nil {
			ev.Source.Metadata = *body.Source.Metadata
		}
	}

	if body.Data != nil {
		data, err := json.Marshal(body.Data)
		if err != nil {
			return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid data"}}, nil
		}
		ev.Data = json.RawMessage(data)
	}

	env := s.captureSession.Publish(ev)
	return publishEventOKResponse{env}, nil
}

// StreamEvents handles GET /events/stream.
// Opens an SSE stream of envelopes from the event bus ring buffer.
// Supports reconnection via the Last-Event-ID header. Emits a keepalive comment
// frame every 15 s when no event arrives.
func (s *ApiService) StreamEvents(ctx context.Context, req oapi.StreamEventsRequestObject) (oapi.StreamEventsResponseObject, error) {
	afterSeq := s.captureSession.Seq()
	if id := req.Params.LastEventID; id != nil && *id != "" {
		if n, err := strconv.ParseUint(*id, 10, 64); err == nil && n > 0 {
			afterSeq = n
		}
	}

	reader := s.captureSession.NewReader(afterSeq)

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			result, err := reader.Read(readCtx)
			cancel()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					select {
					case <-ctx.Done():
						return
					default:
						if _, err := pw.Write([]byte(":\n\n")); err != nil {
							return
						}
						continue
					}
				}
				return
			}

			if result.Dropped > 0 {
				continue
			}

			env := result.Envelope
			if err := writeEnvelopeFrame(pw, &env.Seq, *env); err != nil {
				return
			}
		}
	}()

	headers := oapi.StreamEvents200ResponseHeaders{XSSEContentType: "application/json"}
	return oapi.StreamEvents200TexteventStreamResponse{Body: pr, Headers: headers}, nil
}

// publishEventOKResponse serializes events.Envelope directly so the response
// is identical in shape to the SSE stream frames.
type publishEventOKResponse struct{ env events.Envelope }

func (r publishEventOKResponse) VisitPublishEventResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	return json.NewEncoder(w).Encode(r.env)
}

// writeEnvelopeFrame writes a single SSE frame. If seq is non-nil it is
// emitted as the id: field, updating the client's Last-Event-ID.
func writeEnvelopeFrame(w io.Writer, seq *uint64, env events.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if seq != nil {
		fmt.Fprintf(&buf, "id: %d\n", *seq)
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	_, err = w.Write(buf.Bytes())
	return err
}

// StartCapture handles POST /events/start.
// Generates a new capture session ID, seeds the pipeline, then starts the
// CDP monitor. If already running, the monitor is stopped and
// restarted with a fresh session ID.
func (s *ApiService) StartCapture(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	s.captureSession.Start(uuid.New().String(), events.CaptureConfig{})

	if err := s.cdpMonitor.Start(context.Background()); err != nil {
		logger.FromContext(r.Context()).Error("failed to start CDP monitor", "err", err)
		http.Error(w, "failed to start capture", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// StopCapture handles POST /events/stop.
func (s *ApiService) StopCapture(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	s.cdpMonitor.Stop()
	w.WriteHeader(http.StatusOK)
}
