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
	sev, sevText := otlpSeverity(ev.Type)
	rec.SetSeverity(sev)
	rec.SetSeverityText(sevText)

	if body, ok := jsonToLogValue(ev.Data); ok {
		rec.SetBody(body)
	}

	attrs := make([]log.KeyValue, 0, 8)
	attrs = append(attrs,
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
	if ev.Source.Metadata != nil {
		for k, v := range *ev.Source.Metadata {
			attrs = append(attrs, log.String("kernel."+k, v))
		}
	}
	rec.AddAttributes(attrs...)
	return rec
}

// otlpSeverity maps an event type to an OTLP severity. Console errors are
// errors; anything that reports a failure is a warning; everything else is
// informational.
func otlpSeverity(eventType string) (log.Severity, string) {
	switch {
	case eventType == "console_error":
		return log.SeverityError, "ERROR"
	case strings.HasSuffix(eventType, "_failed"):
		return log.SeverityWarn, "WARN"
	default:
		return log.SeverityInfo, "INFO"
	}
}

// jsonToLogValue decodes raw event data into a structured OTLP value so the
// body stays navigable in the backend rather than an opaque string. Returns
// false when there is no data (e.g. truncated oversized envelopes). Invalid
// JSON falls back to the raw bytes as a string.
func jsonToLogValue(raw json.RawMessage) (log.Value, bool) {
	if len(raw) == 0 {
		return log.Value{}, false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return log.StringValue(string(raw)), true
	}
	return anyToLogValue(v), true
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
