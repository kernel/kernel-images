package metrics

import "context"

// OTLPSource exposes the OTLP export sink's lifetime counters. Implemented by
// *events.OTLPMetrics.
type OTLPSource interface {
	Dropped() uint64
	Failures() uint64
	Exported() uint64
}

// OTLPCollector reports OTLP export health: records dropped under backpressure,
// failed export requests, and records successfully exported. All are
// process-lifetime counters with no labels; the scraper attaches instance
// identity.
type OTLPCollector struct {
	src OTLPSource
}

func NewOTLPCollector(src OTLPSource) *OTLPCollector {
	return &OTLPCollector{src: src}
}

func (c *OTLPCollector) Name() string { return "otlp" }

func (c *OTLPCollector) Collect(_ context.Context, w *Writer) error {
	w.Metric("kernel_otlp_records_dropped_total", "OTLP telemetry records dropped due to export queue overflow.", "counter")
	w.Sample("kernel_otlp_records_dropped_total", nil, float64(c.src.Dropped()))

	w.Metric("kernel_otlp_export_failures_total", "OTLP export requests that returned an error.", "counter")
	w.Sample("kernel_otlp_export_failures_total", nil, float64(c.src.Failures()))

	w.Metric("kernel_otlp_records_exported_total", "OTLP telemetry records successfully exported.", "counter")
	w.Sample("kernel_otlp_records_exported_total", nil, float64(c.src.Exported()))
	return nil
}
