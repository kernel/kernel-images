package metrics

import "context"

// PageRecoveryCollector exposes the in-memory navigation-retry counters. Like
// the network collector it never dials Chrome, so it keeps reporting while the
// recoverer's own connection is down — which is exactly when the gauge matters.
type PageRecoveryCollector struct {
	snapshot func() (retries, recovered, exhausted uint64, up bool)
}

func NewPageRecoveryCollector(snapshot func() (retries, recovered, exhausted uint64, up bool)) *PageRecoveryCollector {
	return &PageRecoveryCollector{snapshot: snapshot}
}

func (*PageRecoveryCollector) Name() string { return "page_recovery" }

func (c *PageRecoveryCollector) Collect(_ context.Context, w *Writer) error {
	retries, recovered, exhausted, healthy := c.snapshot()
	const retriesName = "kernel_page_recovery_retries_total"
	const recoveredName = "kernel_page_recovery_recovered_total"
	const exhaustedName = "kernel_page_recovery_exhausted_total"
	const upName = "kernel_page_recovery_up"
	w.Metric(retriesName, "Top-level navigations replayed after a refused response.", "counter")
	w.Sample(retriesName, nil, float64(retries))
	w.Metric(recoveredName, "Replayed navigations that went on to answer below 400.", "counter")
	w.Sample(recoveredName, nil, float64(recovered))
	w.Metric(exhaustedName, "Refusals passed through with the retry budget spent.", "counter")
	w.Sample(exhaustedName, nil, float64(exhausted))
	w.Metric(upName, "Whether navigation-retry interception is installed.", "gauge")
	value := float64(0)
	if healthy {
		value = 1
	}
	w.Sample(upName, nil, value)
	return nil
}
