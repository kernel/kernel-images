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

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// PublishTelemetryEvent handles POST /telemetry/events.
// Routes a caller-supplied event through the active telemetry session so it
// picks up category filtering and the telemetry_session_id metadata stamp.
// Returns 200 with the assigned envelope when the event is admitted, 204
// when filtered (no active session or the category is disabled), or 400 on
// validation failure.
func (s *ApiService) PublishTelemetryEvent(_ context.Context, req oapi.PublishTelemetryEventRequestObject) (oapi.PublishTelemetryEventResponseObject, error) {
	body := req.Body
	if body == nil || body.Type == "" {
		return oapi.PublishTelemetryEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "type is required"}}, nil
	}
	ev := events.Event{Type: body.Type}
	ev.Ts = time.Now().UnixMicro()

	// Category is server-authoritative. A known event type is assigned its
	// canonical category and any caller-supplied value is ignored; an unknown
	// custom type must carry a valid category from the caller.
	if cat, ok := events.CategoryForType(body.Type); ok {
		ev.Category = cat
	} else if body.Category != nil {
		cat := oapi.TelemetryEventCategory(*body.Category)
		if !cat.Valid() {
			return oapi.PublishTelemetryEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid category"}}, nil
		}
		ev.Category = cat
	} else {
		return oapi.PublishTelemetryEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "category is required for unknown event type"}}, nil
	}

	if body.Source != nil {
		if body.Source.Kind == oapi.KernelApi {
			return oapi.PublishTelemetryEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "source.kind kernel_api is reserved for server-generated events"}}, nil
		}
		ev.Source.Kind = oapi.BrowserEventSourceKind(body.Source.Kind)
		ev.Source.Event = body.Source.Event
		ev.Source.Metadata = body.Source.Metadata
	}

	if body.Data != nil {
		// re-marshal body.Data to normalize it into a canonical RawMessage byte slice.
		data, err := json.Marshal(body.Data)
		if err != nil {
			return oapi.PublishTelemetryEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid data"}}, nil
		}
		ev.Data = json.RawMessage(data)
	}

	env, ok := s.telemetrySession.Publish(ev)
	if !ok {
		return oapi.PublishTelemetryEvent204Response{}, nil
	}
	return publishTelemetryEventOKResponse{env}, nil
}

// StreamTelemetryEvents handles GET /telemetry/stream.
// Opens an SSE stream of telemetry event envelopes from the telemetry stream ring buffer.
// Supports reconnection via the Last-Event-ID header. Emits a keepalive comment
// frame every 15 s when no event arrives.
func (s *ApiService) StreamTelemetryEvents(ctx context.Context, req oapi.StreamTelemetryEventsRequestObject) (oapi.StreamTelemetryEventsResponseObject, error) {
	// Default to the current seq so fresh connections only see new events.
	// Seqs are process-monotonic; a Last-Event-ID from any prior session resumes correctly.
	afterSeq := s.eventStream.Seq()
	if id := req.Params.LastEventID; id != nil && *id != "" {
		if n, err := strconv.ParseUint(*id, 10, 64); err == nil && n > 0 {
			afterSeq = n
		}
	}

	reader := s.eventStream.NewReader(afterSeq)

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
						// No event in 15 s and client still connected, send keepalive.
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

	headers := oapi.StreamTelemetryEvents200ResponseHeaders{XSSEContentType: "application/json"}
	return oapi.StreamTelemetryEvents200TexteventStreamResponse{Body: pr, Headers: headers}, nil
}

// publishTelemetryEventOKResponse serializes events.Envelope directly so the response
// is identical in shape to the SSE stream frames.
type publishTelemetryEventOKResponse struct{ env events.Envelope }

func (r publishTelemetryEventOKResponse) VisitPublishTelemetryEventResponse(w http.ResponseWriter) error {
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
