package metrics

import (
	"context"
	"slices"
)

// RelaySource exposes the btel storage relay sink's lifetime counters.
// Implemented by *events.RelayMetrics.
type RelaySource interface {
	Exported() uint64
	Retries() uint64
	QueueDepth() int64
	Dropped() map[string]uint64
	Failed() map[string]uint64
}

// RelayCollector reports btel storage relay health: records exported, records
// dropped by reason, retried and failed batches, and the send queue depth.
type RelayCollector struct {
	src RelaySource
}

func NewRelayCollector(src RelaySource) *RelayCollector {
	return &RelayCollector{src: src}
}

func (c *RelayCollector) Name() string { return "btel_relay" }

func (c *RelayCollector) Collect(_ context.Context, w *Writer) error {
	w.Metric("kernel_btel_relay_exported_total", "Telemetry storage records the metro-api relay acknowledged.", "counter")
	w.Sample("kernel_btel_relay_exported_total", nil, float64(c.src.Exported()))

	w.Metric("kernel_btel_relay_dropped_total", "Telemetry storage records dropped, by reason.", "counter")
	writeLabeled(w, "kernel_btel_relay_dropped_total", "reason", c.src.Dropped())

	w.Metric("kernel_btel_relay_retries_total", "Telemetry storage batches retried after a retryable response.", "counter")
	w.Sample("kernel_btel_relay_retries_total", nil, float64(c.src.Retries()))

	w.Metric("kernel_btel_relay_failed_total", "Telemetry storage batches given up on, by HTTP status.", "counter")
	writeLabeled(w, "kernel_btel_relay_failed_total", "status", c.src.Failed())

	w.Metric("kernel_btel_relay_queue_depth", "Telemetry storage records waiting to be sent to the relay.", "gauge")
	w.Sample("kernel_btel_relay_queue_depth", nil, float64(c.src.QueueDepth()))
	return nil
}

func writeLabeled(w *Writer, name, label string, values map[string]uint64) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		w.Sample(name, []Label{{Name: label, Value: k}}, float64(values[k]))
	}
}
