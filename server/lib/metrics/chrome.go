package metrics

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

// DefaultUMAHistograms is the curated set of Chrome UMA histograms exposed
// on the metrics endpoint. Units are milliseconds unless noted. Histograms
// with no samples yet are simply absent from the output.
//
// Only histograms recorded in the browser process are eligible:
// Browser.getHistograms cannot see renderer/GPU-process histograms (e.g.
// EventLatency.*, Graphics.Smoothness.*, Blink.*) because those only merge
// into the browser's recorder when a UMA log is staged or chrome://histograms
// is opened, neither of which happens on our images.
var DefaultUMAHistograms = []string{
	"PageLoad.PaintTiming.NavigationToFirstContentfulPaint",
	"PageLoad.PaintTiming.NavigationToLargestContentfulPaint2",
	"PageLoad.DocumentTiming.NavigationToDOMContentLoadedEventFired",
	"PageLoad.DocumentTiming.NavigationToLoadEventFired",
	"PageLoad.ParseTiming.NavigationToParseStart",
	// INP: worst-case interaction latency (input to next paint), high
	// percentile per page load.
	"PageLoad.InteractiveTiming.UserInteractionLatency.HighPercentile2.MaxEventDuration",
	// FID: input delay of the first interaction per page load.
	"PageLoad.InteractiveTiming.FirstInputDelay4",
	// Interactions per page load; unitless count.
	"PageLoad.InteractiveTiming.NumInteractions",
	// CLS: unitless layout-shift score, not milliseconds.
	"PageLoad.LayoutInstability.MaxCumulativeShiftScore.SessionWindow.Gap1000ms.Max5000ms2",
	// CPU milliseconds consumed by the page across its lifetime.
	"PageLoad.Cpu.TotalUsage",
	// Browser start to first web contents paint; one sample per browser
	// start, measures how quickly a fresh VM renders content.
	"Startup.FirstWebContents.NonEmptyPaint3",
	// Peak GPU memory during scroll sequences, in MB.
	"Memory.GPU.PeakMemoryUsage2.Scroll",
}

// DevToolsUpstream reports the current browser-level DevTools WebSocket URL.
// Satisfied by devtoolsproxy.UpstreamManager.
type DevToolsUpstream interface {
	Current() string
}

// ChromeCollector reads browser-wide metrics over CDP: UMA histograms via
// Browser.getHistograms plus the browser version and open page count. All
// commands are browser-level — nothing attaches to pages or injects into
// sessions, so scrapes are invisible to automation running in the browser.
type ChromeCollector struct {
	upstream   DevToolsUpstream
	histograms []string
}

func NewChromeCollector(upstream DevToolsUpstream) *ChromeCollector {
	return &ChromeCollector{upstream: upstream, histograms: DefaultUMAHistograms}
}

func (c *ChromeCollector) Name() string { return "chrome" }

func (c *ChromeCollector) Collect(ctx context.Context, w *Writer) error {
	w.Metric("kernel_chromium_up", "Whether the Chromium DevTools endpoint is reachable and responsive.", "gauge")

	client, version, err := c.dial(ctx)
	if err != nil {
		w.Sample("kernel_chromium_up", nil, 0)
		return err
	}
	defer client.Close()
	w.Sample("kernel_chromium_up", nil, 1)

	w.Metric("kernel_chromium_info", "Chromium build info; value is always 1.", "gauge")
	w.Sample("kernel_chromium_info", []Label{{"product", version.Product}}, 1)

	if pages, err := client.CountPageTargets(ctx); err == nil {
		w.Metric("kernel_chromium_pages", "Number of open page targets (tabs).", "gauge")
		w.Sample("kernel_chromium_pages", nil, float64(pages))
	}

	w.Metric("kernel_chromium_uma",
		"Chrome UMA histogram, cumulative since browser start. Units follow the UMA definition; PageLoad timings are milliseconds.",
		"histogram")
	for _, name := range c.histograms {
		matches, err := client.GetHistograms(ctx, name)
		if err != nil {
			return err
		}
		for _, h := range matches {
			// The query is a substring filter, so it also returns
			// suffixed variants of the requested histogram.
			if h.Name == name {
				writeUMAHistogram(w, h)
			}
		}
	}
	return nil
}

func (c *ChromeCollector) dial(ctx context.Context) (*cdpclient.Client, *cdpclient.BrowserVersion, error) {
	url := c.upstream.Current()
	if url == "" {
		return nil, nil, errors.New("devtools upstream not available")
	}
	client, err := cdpclient.Dial(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	version, err := client.GetBrowserVersion(ctx)
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	return client, version, nil
}

// writeUMAHistogram emits one UMA histogram as a Prometheus histogram with a
// "histogram" label carrying the UMA name. Chrome reports sparse, disjoint
// [low, high) buckets; the upper bound maps to a cumulative le bound. The
// overflow bucket (high = MaxInt32) folds into +Inf.
func writeUMAHistogram(w *Writer, h cdpclient.Histogram) {
	labels := func(le string) []Label {
		return []Label{{"histogram", h.Name}, {"le", le}}
	}
	count := h.Count
	if count == 0 {
		for _, b := range h.Buckets {
			count += b.Count
		}
	}
	cumulative := int64(0)
	for _, b := range h.Buckets {
		if b.High >= math.MaxInt32 {
			continue
		}
		cumulative += b.Count
		w.Sample("kernel_chromium_uma_bucket", labels(strconv.FormatInt(b.High, 10)), float64(cumulative))
	}
	w.Sample("kernel_chromium_uma_bucket", labels("+Inf"), float64(count))
	w.Sample("kernel_chromium_uma_sum", []Label{{"histogram", h.Name}}, float64(h.Sum))
	w.Sample("kernel_chromium_uma_count", []Label{{"histogram", h.Name}}, float64(count))
}
