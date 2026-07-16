package events

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// OTLPMetrics holds process-lifetime counters for the OTLP export sink. It is
// owned above the export writer so the counts stay monotonic across export
// enable/disable cycles (the writer is rebuilt on each enable). Safe for
// concurrent use.
type OTLPMetrics struct {
	dropped  atomic.Uint64
	failures atomic.Uint64
	exported atomic.Uint64
}

// Dropped returns records dropped from the batch queue under backpressure.
func (m *OTLPMetrics) Dropped() uint64 { return m.dropped.Load() }

// Failures returns export requests that returned an error.
func (m *OTLPMetrics) Failures() uint64 { return m.failures.Load() }

// Exported returns records that were successfully exported.
func (m *OTLPMetrics) Exported() uint64 { return m.exported.Load() }

// The OTel batch log processor reports queue-overflow drops only through its
// global logger as `Warn("dropped log records", "dropped", n)` and resets its
// internal counter each report, so this message is the sole place the count is
// observable. dropCountingHandler reads n off that record into an OTLPMetrics.
const (
	otlpDroppedLogMsg  = "dropped log records"
	otlpDroppedLogAttr = "dropped"
)

type dropCountingHandler struct {
	slog.Handler
	metrics *OTLPMetrics
}

// NewDropCountingHandler wraps base so OTel batch-queue drop reports are summed
// into metrics before being logged as usual.
func NewDropCountingHandler(base slog.Handler, metrics *OTLPMetrics) slog.Handler {
	return &dropCountingHandler{Handler: base, metrics: metrics}
}

func (h *dropCountingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == otlpDroppedLogMsg {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key != otlpDroppedLogAttr {
				return true
			}
			if n := attrUint64(a.Value); n > 0 {
				h.metrics.dropped.Add(n)
			}
			return false
		})
	}
	return h.Handler.Handle(ctx, r)
}

// attrUint64 reads a non-negative integer from a slog value regardless of how
// the logr bridge typed it (uint64, int64, or a boxed int).
func attrUint64(v slog.Value) uint64 {
	switch v.Kind() {
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindInt64:
		if n := v.Int64(); n > 0 {
			return uint64(n)
		}
	default:
		switch x := v.Any().(type) {
		case uint64:
			return x
		case int64:
			if x > 0 {
				return uint64(x)
			}
		case int:
			if x > 0 {
				return uint64(x)
			}
		}
	}
	return 0
}
