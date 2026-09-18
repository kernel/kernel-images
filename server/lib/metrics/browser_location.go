package metrics

import "context"

// BrowserLocationSnapshot contains process-lifetime control-plane counters and
// current component convergence. It intentionally carries no lease or locale
// labels so metric cardinality remains bounded.
type BrowserLocationSnapshot struct {
	Accepted           uint64
	Applied            uint64
	Retries            uint64
	Stale              uint64
	Conflicts          uint64
	EpochRejects       uint64
	Failures           uint64
	ConvergenceMs      uint64
	Converged          bool
	TimeZoneConverged  bool
	BrowserConverged   bool
	RenderersConverged bool
	NetworkConverged   bool
}

type BrowserLocationCollector struct {
	snapshot func() BrowserLocationSnapshot
}

func NewBrowserLocationCollector(snapshot func() BrowserLocationSnapshot) *BrowserLocationCollector {
	return &BrowserLocationCollector{snapshot: snapshot}
}

func (*BrowserLocationCollector) Name() string { return "browser_location" }

func (c *BrowserLocationCollector) Collect(_ context.Context, w *Writer) error {
	s := c.snapshot()
	counters := []struct {
		name  string
		help  string
		value uint64
	}{
		{"kernel_browser_location_accepted_total", "Browser location bundles accepted.", s.Accepted},
		{"kernel_browser_location_applied_total", "Browser location bundles observed fully converged.", s.Applied},
		{"kernel_browser_location_retries_total", "Browser location same-value and convergence retries.", s.Retries},
		{"kernel_browser_location_stale_total", "Stale browser location generations rejected.", s.Stale},
		{"kernel_browser_location_conflicts_total", "Equal-generation browser location payload conflicts rejected.", s.Conflicts},
		{"kernel_browser_location_epoch_rejects_total", "Browser location bundles rejected for a non-active epoch.", s.EpochRejects},
		{"kernel_browser_location_failures_total", "Browser location reconciliation attempts that exhausted their deadline.", s.Failures},
	}
	for _, counter := range counters {
		w.Metric(counter.name, counter.help, "counter")
		w.Sample(counter.name, nil, float64(counter.value))
	}
	w.Metric("kernel_browser_location_convergence_milliseconds", "Most recent browser location convergence latency.", "gauge")
	w.Sample("kernel_browser_location_convergence_milliseconds", nil, float64(s.ConvergenceMs))
	w.Metric("kernel_browser_location_converged", "Whether the accepted browser location generation is fully converged.", "gauge")
	w.Sample("kernel_browser_location_converged", nil, boolToFloat(s.Converged))
	components := []struct {
		name  string
		value bool
	}{
		{"timezone", s.TimeZoneConverged},
		{"browser", s.BrowserConverged},
		{"renderers", s.RenderersConverged},
		{"network_contexts", s.NetworkConverged},
	}
	w.Metric("kernel_browser_location_component_converged", "Whether each browser location component has converged.", "gauge")
	for _, component := range components {
		w.Sample("kernel_browser_location_component_converged", []Label{{Name: "component", Value: component.name}}, boolToFloat(component.value))
	}
	return nil
}
