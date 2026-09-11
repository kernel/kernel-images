package metrics

import "context"

// NetworkCollector exposes in-memory monitor counters without dialing Chrome.
// It remains available when the independent Chrome/UMA collector fails.
type NetworkCollector struct {
	snapshot func() (resets, completed uint64, up bool)
}

func NewNetworkCollector(snapshot func() (resets, completed uint64, up bool)) *NetworkCollector {
	return &NetworkCollector{snapshot: snapshot}
}

func (*NetworkCollector) Name() string { return "chromium_network" }

func (c *NetworkCollector) Collect(_ context.Context, w *Writer) error {
	resetCount, completedCount, healthy := c.snapshot()
	const resets = "kernel_chromium_connection_resets_total"
	const completed = "kernel_chromium_network_requests_completed_total"
	const up = "kernel_chromium_network_monitor_up"
	w.Metric(resets, "Observed request terminal outcomes with exact net::ERR_CONNECTION_RESET.", "counter")
	w.Sample(resets, nil, float64(resetCount))
	w.Metric(completed, "Observed Network.loadingFinished or Network.loadingFailed outcomes.", "counter")
	w.Sample(completed, nil, float64(completedCount))
	w.Metric(up, "Whether CDP network capture is initialized for known targets.", "gauge")
	value := float64(0)
	if healthy {
		value = 1
	}
	w.Sample(up, nil, value)
	return nil
}
