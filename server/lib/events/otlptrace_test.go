package events

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func traceEnv(session, evType string, tsMicro int64, data map[string]any) Envelope {
	meta := map[string]string{"telemetry_session_id": session}
	raw, _ := json.Marshal(data)
	return Envelope{Event: Event{
		Ts:     tsMicro,
		Type:   evType,
		Source: oapi.BrowserEventSource{Kind: "cdp", Metadata: &meta},
		Data:   json.RawMessage(raw),
	}}
}

func spanAttr(sp *tracepb.Span, key string) *commonpb.AnyValue {
	for _, kv := range sp.Attributes {
		if kv.Key == key {
			return kv.Value
		}
	}
	return nil
}

func TestSpanBuilder_NavigationSpan(t *testing.T) {
	b := newSpanBuilder()
	session, loader := "cs_1", "L1"

	assert.Nil(t, b.Ingest(traceEnv(session, "page_navigation", 1_000_000, map[string]any{
		"loader_id": loader, "url": "https://example.com",
	})), "start event emits nothing")

	spans := b.Ingest(traceEnv(session, "page_load", 3_000_000, map[string]any{"loader_id": loader}))
	require.Len(t, spans, 1)
	sp := spans[0]

	assert.Equal(t, traceIDFor(session), sp.TraceId)
	assert.Equal(t, spanIDFor(session, loader), sp.SpanId)
	assert.Empty(t, sp.ParentSpanId, "navigation is a root span")
	assert.Equal(t, "navigation https://example.com", sp.Name)
	assert.Equal(t, uint64(1_000_000)*1000, sp.StartTimeUnixNano)
	assert.Equal(t, uint64(3_000_000)*1000, sp.EndTimeUnixNano)
	assert.Equal(t, tracepb.Status_STATUS_CODE_OK, sp.Status.Code)
	assert.Equal(t, "https://example.com", spanAttr(sp, "url.full").GetStringValue())
}

func TestSpanBuilder_NetworkChildSpan(t *testing.T) {
	b := newSpanBuilder()
	session, loader, req := "cs_1", "L1", "R7"

	// Open the navigation so the child has a parent to reference.
	b.Ingest(traceEnv(session, "page_navigation", 1_000_000, map[string]any{"loader_id": loader, "url": "https://example.com"}))

	assert.Nil(t, b.Ingest(traceEnv(session, "network_request", 1_500_000, map[string]any{
		"request_id": req, "loader_id": loader, "method": "GET", "url": "https://example.com/api",
	})))

	spans := b.Ingest(traceEnv(session, "network_response", 1_800_000, map[string]any{
		"request_id": req, "method": "GET", "url": "https://example.com/api", "status": float64(200),
	}))
	require.Len(t, spans, 1)
	sp := spans[0]

	assert.Equal(t, traceIDFor(session), sp.TraceId)
	assert.Equal(t, spanIDFor(session, req), sp.SpanId)
	assert.Equal(t, spanIDFor(session, loader), sp.ParentSpanId, "network span parents to its navigation via loader_id")
	assert.Equal(t, tracepb.Span_SPAN_KIND_CLIENT, sp.Kind)
	assert.Equal(t, "GET https://example.com/api", sp.Name)
	assert.Equal(t, int64(200), spanAttr(sp, "http.response.status_code").GetIntValue())
	assert.Equal(t, "GET", spanAttr(sp, "http.request.method").GetStringValue())
}

func TestSpanBuilder_FailedRequestIsError(t *testing.T) {
	b := newSpanBuilder()
	session, req := "cs_1", "R9"

	b.Ingest(traceEnv(session, "network_request", 1_000_000, map[string]any{"request_id": req, "method": "GET", "url": "https://x.test"}))
	spans := b.Ingest(traceEnv(session, "network_loading_failed", 1_200_000, map[string]any{
		"request_id": req, "error_text": "net::ERR_TIMED_OUT",
	}))
	require.Len(t, spans, 1)
	assert.Equal(t, tracepb.Status_STATUS_CODE_ERROR, spans[0].Status.Code)
	assert.Equal(t, "net::ERR_TIMED_OUT", spans[0].Status.Message)
}

func TestSpanBuilder_ParentLinkageIsDeterministic(t *testing.T) {
	// A child arriving before its navigation was ever seen still parents
	// correctly, because the parent span id is derived, not looked up.
	b := newSpanBuilder()
	session, loader, req := "cs_1", "L1", "R1"

	b.Ingest(traceEnv(session, "network_request", 1_000_000, map[string]any{"request_id": req, "loader_id": loader, "method": "GET", "url": "https://x"}))
	spans := b.Ingest(traceEnv(session, "network_response", 1_100_000, map[string]any{"request_id": req, "status": float64(200)}))
	require.Len(t, spans, 1)

	navID := spanIDFor(session, loader)
	assert.True(t, bytes.Equal(spans[0].ParentSpanId, navID))
}

func TestSpanBuilder_IgnoresPointAndUnknownEvents(t *testing.T) {
	b := newSpanBuilder()
	for _, ty := range []string{"console_log", "page_lcp", "interaction_click", "network_idle", "something_else"} {
		assert.Nil(t, b.Ingest(traceEnv("cs_1", ty, 1_000_000, map[string]any{"foo": "bar"})), ty)
	}
}

func TestSpanBuilder_MissingSessionIsDropped(t *testing.T) {
	b := newSpanBuilder()
	env := Envelope{Event: Event{Ts: 1_000_000, Type: "page_navigation", Source: oapi.BrowserEventSource{Kind: "cdp"}, Data: json.RawMessage(`{"loader_id":"L1"}`)}}
	assert.Nil(t, b.Ingest(env))
}

func TestSpanBuilder_SweepEvictsStaleOpenSpans(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newSpanBuilder()
	b.now = func() time.Time { return now }

	b.Ingest(traceEnv("cs_1", "network_request", 1_000_000, map[string]any{"request_id": "R1", "method": "GET", "url": "https://x"}))
	assert.Nil(t, b.Sweep(), "not yet stale")

	now = now.Add(spanTTL + time.Second)
	flushed := b.Sweep()
	require.Len(t, flushed, 1)
	assert.Equal(t, flushed[0].StartTimeUnixNano, flushed[0].EndTimeUnixNano, "flushed unterminated span has zero duration")
	assert.Empty(t, b.Sweep(), "swept spans are removed")
}
