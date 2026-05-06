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
	if body.Type == events.SessionEnded || body.Type == events.EventsDropped {
		return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "type is reserved"}}, nil
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
		// re-marshal body.Data to normalize it into a canonical RawMessage byte slice.
		data, err := json.Marshal(body.Data)
		if err != nil {
			return oapi.PublishEvent400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid data"}}, nil
		}
		ev.Data = json.RawMessage(data)
	}

	env := s.captureSession.PublishUnfiltered(ev)
	return publishEventOKResponse{env}, nil
}

// StreamEvents handles GET /events/stream.
// Opens an SSE stream of envelopes from the active capture session's ring buffer.
// Supports reconnection via the Last-Event-ID header. Emits a keepalive comment
// frame every 15 s when no event arrives, and exits cleanly on session_ended.
func (s *ApiService) StreamEvents(ctx context.Context, req oapi.StreamEventsRequestObject) (oapi.StreamEventsResponseObject, error) {
	afterSeq := uint64(0)
	if id := req.Params.LastEventID; id != nil && *id != "" {
		// Invalid/non-numeric values fall back to 0, replaying all events from the start.
		// Note: seq is per capture session and resets on each Start(). A Last-Event-ID
		// from a previous session may silently overlap with the current session's seqs.
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
				env := events.Envelope{
					CaptureSessionID: sessionID,
					Seq:              0,
					Event: events.Event{
						Ts:       time.Now().UnixMicro(),
						Type:     events.EventsDropped,
						Category: events.CategorySystem,
						Source:   events.Source{Kind: events.KindKernelAPI},
						Data:     json.RawMessage(fmt.Sprintf(`{"dropped":%d}`, result.Dropped)),
					},
				}
				// Omit the id: field so the client's Last-Event-ID is not overwritten.
				if err := writeEnvelopeFrame(pw, nil, env); err != nil {
					return
				}
				continue
			}

			env := result.Envelope
			if err := writeEnvelopeFrame(pw, &env.Seq, *env); err != nil {
				return
			}
			if env.Event.Type == events.SessionEnded {
				return
			}
		}
	}()

	headers := oapi.StreamEvents200ResponseHeaders{XSSEContentType: "application/json"}
	return oapi.StreamEvents200TexteventStreamResponse{Body: pr, Headers: headers}, nil
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

// publishEventOKResponse serializes events.Envelope directly so the response
// is identical in shape to the SSE stream frames.
type publishEventOKResponse struct{ env events.Envelope }

func (r publishEventOKResponse) VisitPublishEventResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	return json.NewEncoder(w).Encode(r.env)
}
