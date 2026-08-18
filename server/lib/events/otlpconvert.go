package events

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"go.opentelemetry.io/otel/log"
)

// otlpScopeName is the InstrumentationScope name stamped on exported records.
const otlpScopeName = "github.com/kernel/kernel-images/server/lib/events"

// otlpExcludedCategories are telemetry categories that are not forwarded to the
// OTLP sink. Screenshots are base64 frames and Monitor is collector-health
// metadata; both bloat a customer's log backend with no analytical value.
var otlpExcludedCategories = map[oapi.TelemetryEventCategory]struct{}{
	Screenshot: {},
	Monitor:    {},
}

// otlpCategoryExported reports whether events in cat are forwarded to OTLP.
func otlpCategoryExported(cat oapi.TelemetryEventCategory) bool {
	_, excluded := otlpExcludedCategories[cat]
	return !excluded
}

// toLogRecord maps a telemetry envelope to an OTLP log record. It is pure (no
// I/O, no wall-clock reads) so it can be unit tested in isolation.
func toLogRecord(env Envelope) log.Record {
	ev := env.Event

	var rec log.Record
	rec.SetTimestamp(time.UnixMicro(ev.Ts))
	rec.SetEventName(ev.Type)

	// Decode the payload once: it becomes the structured body and the source of
	// promoted attributes, and drives per-resource severity for proxy_error.
	var data any
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			rec.SetBody(log.StringValue(string(ev.Data)))
			data = nil
		} else {
			rec.SetBody(anyToLogValue(data))
		}
	}

	sev, sevText := otlpSeverity(ev.Type, data)
	rec.SetSeverity(sev)
	rec.SetSeverityText(sevText)

	attrs := make([]log.KeyValue, 0)
	attrs = append(attrs,
		// EventName is dropped by some backends (Datadog, Loki), so the event
		// type is mirrored into an attribute to stay queryable everywhere.
		log.String("kernel.event.type", ev.Type),
		log.String("kernel.event.category", string(ev.Category)),
		log.Int64("kernel.event.seq", int64(env.Seq)),
		log.String("kernel.source.kind", string(ev.Source.Kind)),
	)
	if ev.Source.Event != nil {
		attrs = append(attrs, log.String("kernel.source.event", *ev.Source.Event))
	}
	if ev.Truncated {
		attrs = append(attrs, log.Bool("kernel.truncated", true))
	}
	// Producer metadata keys (telemetry_session_id, cdp ids) pass through under
	// the kernel. prefix as-is: they are producer snake_case, unlike the dotted
	// converter-owned keys above.
	if ev.Source.Metadata != nil {
		for k, v := range *ev.Source.Metadata {
			attrs = append(attrs, log.String("kernel."+k, v))
		}
	}
	if m, ok := data.(map[string]any); ok {
		attrs = append(attrs, promotedAttributes(ev.Category, m)...)
	}
	rec.AddAttributes(attrs...)
	return rec
}

// Producer event-data keys promoted to queryable attributes. They mirror the
// network/console event-data fields in openapi.yaml; keep them in sync if that
// schema changes.
const (
	dataKeyMethod = "method"
	dataKeyURL    = "url"
	dataKeyStatus = "status"
	dataKeyCode   = "code"
	dataKeyLevel  = "level"
)

// promotedAttributes lifts high-value payload fields into typed, queryable
// attributes (OTel semantic conventions where they exist) so they stay
// filterable in backends that do not flatten a structured body (Datadog, Loki).
// The full payload remains in the body for fidelity.
func promotedAttributes(cat oapi.TelemetryEventCategory, data map[string]any) []log.KeyValue {
	var out []log.KeyValue
	switch cat {
	case Network:
		if v, ok := data[dataKeyMethod].(string); ok {
			out = append(out, log.String("http.request.method", v))
		}
		if v, ok := data[dataKeyURL].(string); ok {
			out = append(out, log.String("url.full", v))
		}
		if v, ok := data[dataKeyStatus].(float64); ok {
			out = append(out, log.Int64("http.response.status_code", int64(v)))
		}
		// Only proxy_error events carry code; other network events have no such
		// field, so the promotion is effectively gated to that event type.
		if v, ok := data[dataKeyCode].(string); ok {
			out = append(out, log.String("kernel.proxy_error_code", v))
		}
	case Console:
		if v, ok := data[dataKeyLevel].(string); ok {
			out = append(out, log.String("kernel.console.level", v))
		}
	}
	return out
}

// otlpSeverity maps an event type to an OTLP severity. It is a deliberately
// coarse, best-effort mapping keyed off the producer's event-type string
// conventions (the literal "console_error" and the "_failed" suffix), not the
// generated event-type constants: those live in the producer package
// (cdpmonitor), which this package cannot import without a cycle. If those type
// names change, update this mapping in lockstep.
func otlpSeverity(eventType string, data any) (log.Severity, string) {
	switch {
	case eventType == "console_error":
		return log.SeverityError, "ERROR"
	// Process-death, renderer-crash, OOM, and proxy loss of the top-level
	// document are failures a consumer alerts on, so they map to ERROR.
	case eventType == "service_crashed", eventType == "system_oom_kill", eventType == "page_crashed":
		return log.SeverityError, "ERROR"
	case eventType == "proxy_error":
		// ERROR only for the top-level document; the ~vast majority of branded
		// proxy errors are subresources (blocked scripts/images/fetches), which
		// map to WARN so consumers do not alert on every blocked subresource.
		if m, ok := data.(map[string]any); ok {
			if rt, ok := m["resource_type"].(string); ok && rt == "Document" {
				return log.SeverityError, "ERROR"
			}
		}
		return log.SeverityWarn, "WARN"
	case strings.HasSuffix(eventType, "_failed"):
		return log.SeverityWarn, "WARN"
	default:
		return log.SeverityInfo, "INFO"
	}
}

func anyToLogValue(v any) log.Value {
	switch t := v.(type) {
	case nil:
		return log.Value{}
	case bool:
		return log.BoolValue(t)
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < math.MaxInt64 {
			return log.Int64Value(int64(t))
		}
		return log.Float64Value(t)
	case string:
		return log.StringValue(t)
	case []any:
		vals := make([]log.Value, len(t))
		for i, e := range t {
			vals[i] = anyToLogValue(e)
		}
		return log.SliceValue(vals...)
	case map[string]any:
		kvs := make([]log.KeyValue, 0, len(t))
		for k, e := range t {
			kvs = append(kvs, log.KeyValue{Key: k, Value: anyToLogValue(e)})
		}
		return log.MapValue(kvs...)
	default:
		return log.StringValue(fmt.Sprintf("%v", t))
	}
}
