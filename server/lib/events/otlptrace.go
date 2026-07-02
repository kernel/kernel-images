package events

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Span model (prototype). A telemetry session is a trace; a page navigation is
// a span; each network request is a child span of the navigation it belongs to.
//
// Trace and span IDs are derived by hashing stable fields already present on the
// events (session id, loader id, request id). Because the derivation is
// deterministic, a child span can name its parent's span id without the parent
// being held anywhere — linkage needs no shared state. State is only kept to
// pair a start event with its terminal event so the span carries a real
// duration.
//
//	page_navigation ─▶ page_load               (navigation span, keyed by loader_id)
//	  network_request ─▶ network_response       (network span, keyed by request_id,
//	                     /network_loading_failed  parented via its loader_id)

// spanTTL bounds how long an unterminated span stays buffered before it is
// flushed as-is (a navigation whose page_load never fired, a request with no
// response). Without this a crashed tab would leak open spans forever.
const spanTTL = 60 * time.Second

// traceIDFor derives the 16-byte trace id for a telemetry session.
func traceIDFor(session string) []byte {
	h := sha256.Sum256([]byte("kernel-trace|" + session))
	return h[:16]
}

// spanIDFor derives the 8-byte span id for a key within a session.
func spanIDFor(session, key string) []byte {
	h := sha256.Sum256([]byte("kernel-span|" + session + "|" + key))
	return h[:8]
}

// openSpan is a span awaiting its terminal event.
type openSpan struct {
	span    *tracepb.Span
	addedAt time.Time
}

// spanBuilder correlates start/terminal event pairs into completed OTLP spans.
// It is safe for a single reader goroutine; callers serialize Ingest like the
// StorageWriter run loop does.
type spanBuilder struct {
	mu   sync.Mutex
	open map[string]*openSpan
	now  func() time.Time
}

func newSpanBuilder() *spanBuilder {
	return &spanBuilder{open: make(map[string]*openSpan), now: time.Now}
}

// Ingest folds one envelope into the span state. It returns any spans completed
// by this event (a terminal event closes one; other events may close nothing).
func (b *spanBuilder) Ingest(env Envelope) []*tracepb.Span {
	ev := env.Event
	session := sessionID(ev)
	if session == "" {
		return nil
	}

	var data map[string]any
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &data)
	}
	loaderID, _ := data["loader_id"].(string)
	requestID, _ := data["request_id"].(string)

	b.mu.Lock()
	defer b.mu.Unlock()

	switch ev.Type {
	case "page_navigation":
		if loaderID == "" {
			return nil
		}
		b.startLocked(session, loaderID, &tracepb.Span{
			TraceId:           traceIDFor(session),
			SpanId:            spanIDFor(session, loaderID),
			Name:              spanName("navigation", str(data["url"])),
			Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
			StartTimeUnixNano: nanos(ev.Ts),
			Attributes:        navAttributes(session, data),
		})
		return nil

	case "page_load":
		if loaderID == "" {
			return nil
		}
		return b.finishLocked(session, loaderID, ev.Ts, tracepb.Status_STATUS_CODE_OK, "", nil)

	case "network_request":
		if requestID == "" {
			return nil
		}
		sp := &tracepb.Span{
			TraceId:           traceIDFor(session),
			SpanId:            spanIDFor(session, requestID),
			Name:              spanName(str(data["method"]), str(data["url"])),
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: nanos(ev.Ts),
			Attributes:        networkAttributes(data),
		}
		if loaderID != "" {
			sp.ParentSpanId = spanIDFor(session, loaderID)
		}
		b.startLocked(session, requestID, sp)
		return nil

	case "network_response":
		if requestID == "" {
			return nil
		}
		return b.finishLocked(session, requestID, ev.Ts, tracepb.Status_STATUS_CODE_OK, "", statusAttributes(data))

	case "network_loading_failed":
		if requestID == "" {
			return nil
		}
		return b.finishLocked(session, requestID, ev.Ts, tracepb.Status_STATUS_CODE_ERROR, str(data["error_text"]), nil)
	}
	return nil
}

// Sweep flushes spans older than spanTTL as-is (no end time set beyond start),
// so long-lived open spans do not leak. Returns the flushed spans.
func (b *spanBuilder) Sweep() []*tracepb.Span {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var out []*tracepb.Span
	for k, os := range b.open {
		if now.Sub(os.addedAt) > spanTTL {
			os.span.EndTimeUnixNano = os.span.StartTimeUnixNano
			out = append(out, os.span)
			delete(b.open, k)
		}
	}
	return out
}

func (b *spanBuilder) startLocked(session, key string, sp *tracepb.Span) {
	b.open[session+"|"+key] = &openSpan{span: sp, addedAt: b.now()}
}

func (b *spanBuilder) finishLocked(session, key string, endTs int64, code tracepb.Status_StatusCode, msg string, extraAttrs []*commonpb.KeyValue) []*tracepb.Span {
	mk := session + "|" + key
	os, ok := b.open[mk]
	if !ok {
		return nil
	}
	delete(b.open, mk)
	os.span.EndTimeUnixNano = nanos(endTs)
	os.span.Status = &tracepb.Status{Code: code, Message: msg}
	os.span.Attributes = append(os.span.Attributes, extraAttrs...)
	return []*tracepb.Span{os.span}
}

// sessionID reads the telemetry session id from event metadata.
func sessionID(ev Event) string {
	if ev.Source.Metadata == nil {
		return ""
	}
	return (*ev.Source.Metadata)["telemetry_session_id"]
}

func navAttributes(session string, data map[string]any) []*commonpb.KeyValue {
	attrs := []*commonpb.KeyValue{kvStr("kernel.telemetry_session_id", session)}
	if u := str(data["url"]); u != "" {
		attrs = append(attrs, kvStr("url.full", u))
	}
	return attrs
}

// networkAttributes are set when the request span opens (method, url).
func networkAttributes(data map[string]any) []*commonpb.KeyValue {
	var attrs []*commonpb.KeyValue
	if v := str(data["method"]); v != "" {
		attrs = append(attrs, kvStr("http.request.method", v))
	}
	if v := str(data["url"]); v != "" {
		attrs = append(attrs, kvStr("url.full", v))
	}
	return attrs
}

// statusAttributes carry what the terminal response event adds (status code).
func statusAttributes(data map[string]any) []*commonpb.KeyValue {
	if v, ok := data["status"].(float64); ok && v > 0 {
		return []*commonpb.KeyValue{kvInt("http.response.status_code", int64(v))}
	}
	return nil
}

func spanName(prefix, detail string) string {
	if detail == "" {
		return prefix
	}
	return prefix + " " + detail
}

func nanos(unixMicro int64) uint64 {
	if unixMicro < 0 {
		return 0
	}
	return uint64(unixMicro) * 1000
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func kvStr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func kvInt(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}
