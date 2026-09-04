package metrics

import (
	"context"
	"sort"
)

type ResponseDrainSource func() map[string]uint64
type ResponseDrainGauge func() int64

type ResponseDrainCollector struct {
	snapshot          ResponseDrainSource
	active            ResponseDrainGauge
	failClosed        ResponseDrainGauge
	closeMonitors     ResponseDrainGauge
	monitorRejections func() uint64
}

func NewResponseDrainCollector(
	snapshot ResponseDrainSource,
	active, failClosed, closeMonitors ResponseDrainGauge,
	monitorRejections func() uint64,
) *ResponseDrainCollector {
	return &ResponseDrainCollector{
		snapshot:          snapshot,
		active:            active,
		failClosed:        failClosed,
		closeMonitors:     closeMonitors,
		monitorRejections: monitorRejections,
	}
}

func (c *ResponseDrainCollector) Name() string { return "scale-to-zero response drain" }

func (c *ResponseDrainCollector) Collect(_ context.Context, w *Writer) error {
	w.Metric("kernel_scale_to_zero_response_drain_total", "HTTP response drain events before scale-to-zero is re-enabled.", "counter")
	counts := c.snapshot()
	outcomes := make([]string, 0, len(counts))
	for outcome := range counts {
		outcomes = append(outcomes, outcome)
	}
	sort.Strings(outcomes)
	for _, outcome := range outcomes {
		w.Sample("kernel_scale_to_zero_response_drain_total", []Label{{Name: "outcome", Value: outcome}}, float64(counts[outcome]))
	}

	w.Metric("kernel_scale_to_zero_response_holds", "HTTP response scale-to-zero holds currently active.", "gauge")
	w.Sample("kernel_scale_to_zero_response_holds", nil, float64(c.active()))
	w.Metric("kernel_scale_to_zero_response_fail_closed_holds", "HTTP response holds awaiting terminal connection recovery or guest termination.", "gauge")
	w.Sample("kernel_scale_to_zero_response_fail_closed_holds", nil, float64(c.failClosed()))
	w.Metric("kernel_scale_to_zero_response_close_monitors", "Duplicated sockets currently monitored for acknowledged response close.", "gauge")
	w.Sample("kernel_scale_to_zero_response_close_monitors", nil, float64(c.closeMonitors()))
	w.Metric("kernel_scale_to_zero_response_close_monitor_rejections_total", "Response close monitor requests rejected at the concurrency limit.", "counter")
	w.Sample("kernel_scale_to_zero_response_close_monitor_rejections_total", nil, float64(c.monitorRejections()))
	return nil
}
