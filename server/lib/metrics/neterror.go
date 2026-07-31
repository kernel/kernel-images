package metrics

import (
	"context"

	"github.com/kernel/kernel-images/server/lib/neterror"
)

// NetErrorSource reports network failure counts observed on relayed CDP
// traffic. Satisfied by neterror.Tracker.
type NetErrorSource interface {
	Snapshot() (map[neterror.Key]int64, int64)
}

// NetErrorCollector exposes Chromium net::ERR_* failures as counters. Unlike
// the Chrome collector it touches neither Chrome nor the network on scrape:
// the counts are accumulated by the CDP proxy as traffic flows and only read
// here.
type NetErrorCollector struct {
	source NetErrorSource
}

func NewNetErrorCollector(source NetErrorSource) *NetErrorCollector {
	return &NetErrorCollector{source: source}
}

func (c *NetErrorCollector) Name() string { return "neterror" }

func (c *NetErrorCollector) Collect(_ context.Context, w *Writer) error {
	counts, overflow := c.source.Snapshot()

	// A VM that has seen no failures emits no samples here, so this family is
	// simply absent from queries until something fails. Use the unlabelled
	// _dropped_total below to tell "no failures" apart from "not scraped".
	w.Metric("kernel_chromium_net_errors_total",
		"Chromium network request failures by error text and resource type, cumulative since VM start. Client-cancelled requests (net::ERR_ABORTED) are excluded; deliberate route.abort() blocking is not separable and lands in the net::ERR_FAILED series.",
		"counter")
	for _, k := range neterror.SortedKeys(counts) {
		w.Sample("kernel_chromium_net_errors_total", []Label{
			{"error", k.Error},
			{"resource_type", k.ResourceType},
		}, float64(counts[k]))
	}

	// Always sampled, so this doubles as the proof that the tap is running on
	// a VM reporting no failures.
	w.Metric("kernel_chromium_net_errors_dropped_total",
		"Network failures not counted because the distinct error/resource-type cap was reached.",
		"counter")
	w.Sample("kernel_chromium_net_errors_dropped_total", nil, float64(overflow))
	return nil
}
