package events

import (
	"encoding/json"
	"testing"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
)

func strptr(s string) *string { return &s }

func attrsOf(rec log.Record) map[string]log.Value {
	out := make(map[string]log.Value)
	rec.WalkAttributes(func(kv log.KeyValue) bool {
		out[kv.Key] = kv.Value
		return true
	})
	return out
}

func TestToLogRecord_CoreFields(t *testing.T) {
	meta := map[string]string{
		"telemetry_session_id": "cs_abc",
		"cdp_session_id":       "s1",
		"target_id":            "t1",
	}
	env := Envelope{
		Seq: 412,
		Event: Event{
			Ts:       1_718_801_234_567_890,
			Type:     "network_response",
			Category: Network,
			Source: oapi.BrowserEventSource{
				Kind:     oapi.BrowserEventSourceKind("cdp"),
				Event:    strptr("Network.loadingFinished"),
				Metadata: &meta,
			},
			Data: json.RawMessage(`{"status":200,"url":"https://api.foo.com/v1/x","ok":true}`),
		},
	}

	rec := toLogRecord(env)

	assert.Equal(t, "network_response", rec.EventName())
	assert.Equal(t, time.UnixMicro(1_718_801_234_567_890).UTC(), rec.Timestamp().UTC())
	assert.Equal(t, log.SeverityInfo, rec.Severity())

	attrs := attrsOf(rec)
	assert.Equal(t, "network_response", attrs["kernel.event.type"].AsString(), "event type mirrored as attribute for backends that drop EventName")
	assert.Equal(t, "network", attrs["kernel.event.category"].AsString())
	assert.Equal(t, int64(412), attrs["kernel.event.seq"].AsInt64())
	assert.Equal(t, "cdp", attrs["kernel.source.kind"].AsString())
	assert.Equal(t, "Network.loadingFinished", attrs["kernel.source.event"].AsString())
	assert.Equal(t, "cs_abc", attrs["kernel.telemetry_session_id"].AsString())
	assert.Equal(t, "s1", attrs["kernel.cdp_session_id"].AsString())
	assert.Equal(t, "t1", attrs["kernel.target_id"].AsString())
	// High-value network fields promoted to queryable attributes.
	assert.Equal(t, int64(200), attrs["http.response.status_code"].AsInt64())
	assert.Equal(t, "https://api.foo.com/v1/x", attrs["url.full"].AsString())
}

func TestToLogRecord_PromotedAttributes(t *testing.T) {
	t.Run("network", func(t *testing.T) {
		// network_response carries method/url/status; network_request has no status.
		env := Envelope{Seq: 1, Event: Event{Type: "network_response", Category: Network,
			Data: json.RawMessage(`{"method":"GET","url":"https://x/y","status":404}`)}}
		rec := toLogRecord(env)
		attrs := attrsOf(rec)
		assert.Equal(t, "GET", attrs["http.request.method"].AsString())
		assert.Equal(t, "https://x/y", attrs["url.full"].AsString())
		assert.Equal(t, int64(404), attrs["http.response.status_code"].AsInt64())
	})
	t.Run("console", func(t *testing.T) {
		env := Envelope{Seq: 2, Event: Event{Type: "console_error", Category: Console,
			Data: json.RawMessage(`{"level":"error","text":"boom"}`)}}
		rec := toLogRecord(env)
		assert.Equal(t, "error", attrsOf(rec)["kernel.console.level"].AsString())
	})
	t.Run("non-object data does not panic", func(t *testing.T) {
		env := Envelope{Seq: 3, Event: Event{Type: "x", Category: Network,
			Data: json.RawMessage(`["a","b"]`)}}
		rec := toLogRecord(env)
		assert.Equal(t, "x", attrsOf(rec)["kernel.event.type"].AsString())
	})
}

func TestToLogRecord_StructuredBody(t *testing.T) {
	env := Envelope{Seq: 1, Event: Event{
		Ts:   1,
		Type: "network_response",
		Data: json.RawMessage(`{"status":200,"ok":true,"ratio":1.5,"tags":["a","b"]}`),
	}}

	rec := toLogRecord(env)
	body := rec.Body()
	require.Equal(t, log.KindMap, body.Kind())

	got := make(map[string]log.Value)
	for _, kv := range body.AsMap() {
		got[kv.Key] = kv.Value
	}
	assert.Equal(t, int64(200), got["status"].AsInt64(), "integral json number stays int64")
	assert.Equal(t, true, got["ok"].AsBool())
	assert.Equal(t, 1.5, got["ratio"].AsFloat64())
	require.Equal(t, log.KindSlice, got["tags"].Kind())
	assert.Len(t, got["tags"].AsSlice(), 2)
}

func TestToLogRecord_Severity(t *testing.T) {
	cases := map[string]log.Severity{
		"console_error":          log.SeverityError,
		"service_crashed":        log.SeverityError,
		"system_oom_kill":        log.SeverityError,
		"network_loading_failed": log.SeverityWarn,
		"monitor_init_failed":    log.SeverityWarn,
		"network_response":       log.SeverityInfo,
		"page_navigation":        log.SeverityInfo,
	}
	for typ, want := range cases {
		rec := toLogRecord(Envelope{Event: Event{Type: typ}})
		assert.Equalf(t, want, rec.Severity(), "severity for %q", typ)
	}
}

func TestToLogRecord_Truncated(t *testing.T) {
	// Absent data (never set) and the production truncation output (payload
	// replaced with JSON null at publish, not stripped to nil) both yield an
	// empty body, but via different branches; cover both.
	t.Run("data absent", func(t *testing.T) {
		env := Envelope{Seq: 9, Event: Event{Ts: 1, Type: "network_response", Category: Network, Truncated: true}}
		rec := toLogRecord(env)
		assert.Equal(t, log.KindEmpty, rec.Body().Kind(), "no body when data is absent")
		assert.Equal(t, true, attrsOf(rec)["kernel.truncated"].AsBool())
	})
	t.Run("data replaced with json null", func(t *testing.T) {
		env := Envelope{Seq: 9, Event: Event{Ts: 1, Type: "network_response", Category: Network, Truncated: true, Data: json.RawMessage(`null`)}}
		rec := toLogRecord(env)
		assert.Equal(t, log.KindEmpty, rec.Body().Kind(), "json null payload yields an empty body")
		assert.Equal(t, true, attrsOf(rec)["kernel.truncated"].AsBool())
	})
}

func TestToLogRecord_NumberCoercion(t *testing.T) {
	env := Envelope{Seq: 1, Event: Event{Ts: 1, Type: "x", Category: Network,
		Data: json.RawMessage(`{"small":200,"neg":-5,"frac":1.5,"big":1e19,"nullish":null}`)}}
	rec := toLogRecord(env)
	got := make(map[string]log.Value)
	for _, kv := range rec.Body().AsMap() {
		got[kv.Key] = kv.Value
	}
	assert.Equal(t, log.KindInt64, got["small"].Kind())
	assert.Equal(t, int64(-5), got["neg"].AsInt64())
	assert.Equal(t, log.KindFloat64, got["frac"].Kind())
	// 1e19 exceeds MaxInt64, so it stays a float rather than overflowing int64.
	assert.Equal(t, log.KindFloat64, got["big"].Kind())
	// A JSON null inside the payload maps to an empty value, not "<nil>".
	assert.Equal(t, log.KindEmpty, got["nullish"].Kind())
}

func TestToLogRecord_InvalidJSONFallsBackToString(t *testing.T) {
	env := Envelope{Seq: 1, Event: Event{Ts: 1, Type: "x", Data: json.RawMessage(`not json`)}}
	rec := toLogRecord(env)
	body := rec.Body()
	require.Equal(t, log.KindString, body.Kind())
	assert.Equal(t, "not json", body.AsString())
}

func TestOTLPCategoryExported(t *testing.T) {
	assert.True(t, otlpCategoryExported(Network))
	assert.True(t, otlpCategoryExported(Console))
	assert.False(t, otlpCategoryExported(Screenshot))
	assert.False(t, otlpCategoryExported(Monitor))
}
