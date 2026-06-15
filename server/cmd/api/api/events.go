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
	"github.com/kernel/kernel-images/server/lib/logger"
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
		cat := *body.Category
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

// resolveStartSeq picks the ring-buffer position a telemetry stream reads from,
// given the request's Last-Event-ID and replay params and the current head seq.
// Fresh connections start at the current seq so they only see new events; seqs are
// process-monotonic, so a Last-Event-ID from any prior session resumes correctly.
// Last-Event-ID wins over replay=all so SSE auto-reconnect resumes from the last
// seen event rather than re-replaying history: any non-empty Last-Event-ID takes the
// resume branch, and an unparseable or non-positive value (including 0) resolves to
// from-now. replay=all returns 0, the NewReader sentinel for the oldest retained event.
func resolveStartSeq(lastEventID *string, replay *oapi.StreamTelemetryEventsParamsReplay, current uint64) uint64 {
	switch {
	case lastEventID != nil && *lastEventID != "":
		if n, err := strconv.ParseUint(*lastEventID, 10, 64); err == nil && n > 0 {
			return n
		}
		return current
	case replay != nil && *replay == oapi.All:
		return 0
	default:
		return current
	}
}

// StreamTelemetryEvents handles GET /telemetry/stream.
// Opens an SSE stream of telemetry event envelopes from the telemetry stream ring buffer.
// Supports reconnection via the Last-Event-ID header. Emits a keepalive comment
// frame every 15 s when no event arrives.
func (s *ApiService) StreamTelemetryEvents(ctx context.Context, req oapi.StreamTelemetryEventsRequestObject) (oapi.StreamTelemetryEventsResponseObject, error) {
	reader := s.eventStream.NewReader(resolveStartSeq(req.Params.LastEventID, req.Params.Replay, s.eventStream.Seq()))

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

const (
	// defaultReadWindow bounds a read that supplies no since/until.
	defaultReadWindow = 5 * time.Minute
	// defaultPageSize / maxPageSize bound how many records one page reads.
	defaultPageSize = 100
	maxPageSize     = 1000
)

// ReadTelemetryEvents handles GET /telemetry/events.
// Reads one page of archived telemetry envelopes for this browser from durable
// S2 storage in ascending sequence order, applying the category filter. The
// X-Has-More / X-Next-Offset response headers carry the pagination cursor.
// Returns an empty list when S2 storage is not configured.
func (s *ApiService) ReadTelemetryEvents(ctx context.Context, req oapi.ReadTelemetryEventsRequestObject) (oapi.ReadTelemetryEventsResponseObject, error) {
	log := logger.FromContext(ctx)

	if !s.s2Enabled() {
		return readTelemetryEventsOKResponse{}, nil
	}

	result, err := s.telemetryReader.Read(ctx, buildReadOptions(req.Params), log)
	if err != nil {
		log.Error("failed to read telemetry events from S2", "err", err)
		return oapi.ReadTelemetryEvents500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to read telemetry events"}}, nil
	}

	// has_more / next cursor track the raw stream position, independent of the
	// category filter, so a filtered page may come back empty while more remain.
	envs := filterByCategory(result.Envelopes, req.Params.Category)

	return readTelemetryEventsOKResponse{envs: envs, nextSeqNum: result.NextSeqNum, hasMore: result.HasMore}, nil
}

// buildReadOptions maps query params to a bounded, paginated read. offset is the
// S2 sequence cursor and takes precedence over since as the start position (the
// two are mutually exclusive starts); until bounds the window end; the page is
// bounded to limit records.
func buildReadOptions(p oapi.ReadTelemetryEventsParams) events.ReadOptions {
	var opts events.ReadOptions

	// since/until/offset are spec'd minimum:0, but no request-validation
	// middleware is mounted, so clamp negatives to 0 here rather than wrap them
	// into a huge uint64.
	switch {
	case p.Offset != nil:
		seq := uint64(max(*p.Offset, 0))
		opts.SeqNum = &seq
	case p.Since != nil:
		ts := uint64(max(*p.Since, 0))
		opts.Timestamp = &ts
	default:
		ts := uint64(time.Now().Add(-defaultReadWindow).UnixMilli())
		opts.Timestamp = &ts
	}

	if p.Until != nil {
		until := uint64(max(*p.Until, 0))
		opts.Until = &until
	}

	count := uint64(pageSize(p.Limit))
	opts.Count = &count
	return opts
}

// pageSize clamps a requested limit into [1, maxPageSize], defaulting when unset.
func pageSize(limit *int) int {
	switch {
	case limit == nil:
		return defaultPageSize
	case *limit < 1:
		return 1
	case *limit > maxPageSize:
		return maxPageSize
	default:
		return *limit
	}
}

func filterByCategory(envs []events.Envelope, cats *[]oapi.TelemetryEventCategory) []events.Envelope {
	if cats == nil || len(*cats) == 0 {
		return envs
	}
	want := make(map[oapi.TelemetryEventCategory]struct{}, len(*cats))
	for _, c := range *cats {
		want[c] = struct{}{}
	}
	out := make([]events.Envelope, 0, len(envs))
	for _, e := range envs {
		if _, ok := want[e.Event.Category]; ok {
			out = append(out, e)
		}
	}
	return out
}

// readTelemetryEventsOKResponse serializes a page of events.Envelope directly,
// matching the SSE stream and publish endpoints. The pagination cursor rides in
// the X-Has-More / X-Next-Offset headers (X-Next-Offset only when there is more).
type readTelemetryEventsOKResponse struct {
	envs       []events.Envelope
	nextSeqNum uint64
	hasMore    bool
}

func (r readTelemetryEventsOKResponse) VisitReadTelemetryEventsResponse(w http.ResponseWriter) error {
	w.Header().Set("X-Has-More", strconv.FormatBool(r.hasMore))
	if r.hasMore {
		w.Header().Set("X-Next-Offset", strconv.FormatUint(r.nextSeqNum, 10))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	envs := r.envs
	if envs == nil {
		envs = []events.Envelope{}
	}
	return json.NewEncoder(w).Encode(envs)
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
