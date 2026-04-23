package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// PublishEvent handles POST /events/publish.
// Injects a caller-supplied event into the active capture session. Returns 400
// if no session is active or the event fails validation.
func (s *ApiService) PublishEvent(_ context.Context, req oapi.PublishEventRequestObject) (oapi.PublishEventResponseObject, error) {
	if !s.captureSession.Active() {
		return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "no active capture session"}}, nil
	}

	body := req.Body
	if body == nil || body.Type == "" {
		return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "type is required"}}, nil
	}

	ev := events.Event{Type: body.Type}

	if body.Ts != nil {
		ev.Ts = *body.Ts
	}
	if body.Category != nil {
		ev.Category = events.EventCategory(*body.Category)
	}

	// Enforce source.kind = KindKernelAPI so callers can't spoof the origin.
	ev.Source.Kind = events.KindKernelAPI
	if body.Source != nil {
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

	s.captureSession.Publish(ev)
	return oapi.PublishEvent200Response{}, nil
}

// StreamEvents handles GET /events/stream.
// Opens an SSE stream of envelopes from the active capture session's ring buffer.
// Supports reconnection via the Last-Event-ID header. Emits a keepalive comment
// frame every 15 s when no event arrives, and exits cleanly on session_ended.
func (s *ApiService) StreamEvents(ctx context.Context, req oapi.StreamEventsRequestObject) (oapi.StreamEventsResponseObject, error) {
	if !s.captureSession.Active() {
		return oapi.StreamEvents400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "no active capture session"}}, nil
	}

	afterSeq := uint64(0)
	if id := req.Params.LastEventID; id != nil && *id != "" {
		if n, err := strconv.ParseUint(*id, 10, 64); err == nil {
			afterSeq = n
		}
	}

	sessionID := s.captureSession.ID()
	reader := s.captureSession.NewReader(afterSeq)

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			result, err := reader.Read(readCtx)
			cancel()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					// No event in 15 s and client still connected, send keepalive.
					if _, err := pw.Write([]byte(":\n\n")); err != nil {
						return
					}
					continue
				}
				return
			}

			if result.Dropped > 0 {
				env := events.Envelope{
					CaptureSessionID: sessionID,
					Seq:              0,
					Event: events.Event{
						Ts:       time.Now().UnixMicro(),
						Type:     "events_dropped",
						Category: events.CategorySystem,
						Source:   events.Source{Kind: events.KindKernelAPI},
						Data:     json.RawMessage(fmt.Sprintf(`{"dropped":%d}`, result.Dropped)),
					},
				}
				if err := writeEnvelopeFrame(pw, 0, env); err != nil {
					return
				}
				continue
			}

			env := result.Envelope
			if err := writeEnvelopeFrame(pw, env.Seq, *env); err != nil {
				return
			}
			if env.Event.Type == "session_ended" {
				return
			}
		}
	}()

	headers := oapi.StreamEvents200ResponseHeaders{XSSEContentType: "application/json"}
	return oapi.StreamEvents200TexteventStreamResponse{Body: pr, Headers: headers}, nil
}

// writeEnvelopeFrame writes a single SSE frame with the given seq as the id.
func writeEnvelopeFrame(w io.Writer, seq uint64, env events.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "id: %d\ndata: ", seq)
	buf.Write(data)
	buf.WriteString("\n\n")
	_, err = w.Write(buf.Bytes())
	return err
}
