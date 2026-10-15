package metrics

import (
	"context"
	"maps"
	"slices"
	"strconv"
)

// NetworkCollector exposes in-memory monitor counters without dialing Chrome.
// It remains available when the independent Chrome/UMA collector fails.
type NetworkCollector struct {
	snapshot func() (resets, completed uint64, up bool, failures map[string][2]uint64)
}

func NewNetworkCollector(snapshot func() (resets, completed uint64, up bool, failures map[string][2]uint64)) *NetworkCollector {
	return &NetworkCollector{snapshot: snapshot}
}

func (*NetworkCollector) Name() string { return "chromium_network" }

func (c *NetworkCollector) Collect(_ context.Context, w *Writer) error {
	resetCount, completedCount, healthy, failureCounts := c.snapshot()
	const resets = "kernel_chromium_connection_resets_total"
	const completed = "kernel_chromium_network_requests_completed_total"
	const up = "kernel_chromium_network_monitor_up"
	w.Metric(resets, "Observed request terminal outcomes with exact net::ERR_CONNECTION_RESET.", "counter")
	w.Sample(resets, nil, float64(resetCount))
	w.Metric(completed, "Observed Network.loadingFinished or Network.loadingFailed outcomes.", "counter")
	w.Sample(completed, nil, float64(completedCount))
	const failures = "kernel_chromium_network_failures_total"
	w.Metric(failures, "Observed Network.loadingFailed outcomes by bounded error code and CDP canceled flag.", "counter")
	// The monitor preseeds the finite taxonomy, including zero-valued buckets.
	for _, code := range slices.Sorted(maps.Keys(failureCounts)) {
		for canceled, count := range failureCounts[code] {
			w.Sample(failures, []Label{{"canceled", strconv.FormatBool(canceled == 1)}, {"error_code", code}}, float64(count))
		}
	}
	w.Metric(up, "Whether CDP network capture is initialized for known targets.", "gauge")
	value := float64(0)
	if healthy {
		value = 1
	}
	w.Sample(up, nil, value)
	return nil
}
