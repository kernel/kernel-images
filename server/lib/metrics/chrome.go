package metrics

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

// UMAHistogram is one curated Chrome histogram plus the fixed bucket
// boundaries it is re-bucketed onto for exposition.
type UMAHistogram struct {
	Name   string
	Bounds []int64
}

// Fixed bucket boundary grids. Chrome's native buckets are exponential
// with per-histogram layouts and only non-empty buckets are reported, so
// exposing them directly would give every VM a different le set — which
// breaks cross-VM histogram aggregation and inflates cardinality.
// Re-bucketing onto small fixed grids makes every VM emit identical le
// sets at ~10 bounds per histogram.
var (
	boundsPageLoadMs    = []int64{100, 250, 500, 1000, 2000, 4000, 8000, 15000, 30000, 60000}
	boundsInteractionMs = []int64{10, 25, 50, 100, 200, 300, 500, 1000, 2500, 10000}
	boundsCPUMs         = []int64{50, 100, 250, 500, 1000, 2500, 5000, 15000, 60000}
	boundsShiftScore    = []int64{1, 5, 10, 25, 50, 100, 250, 1000}
	boundsCount         = []int64{1, 2, 5, 10, 25, 50, 100, 250}
	boundsMB            = []int64{16, 32, 64, 128, 256, 512, 1024, 2048}
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
var DefaultUMAHistograms = []UMAHistogram{
	{Name: "PageLoad.PaintTiming.NavigationToFirstContentfulPaint", Bounds: boundsPageLoadMs},
	{Name: "PageLoad.PaintTiming.NavigationToLargestContentfulPaint2", Bounds: boundsPageLoadMs},
	{Name: "PageLoad.DocumentTiming.NavigationToDOMContentLoadedEventFired", Bounds: boundsPageLoadMs},
	{Name: "PageLoad.DocumentTiming.NavigationToLoadEventFired", Bounds: boundsPageLoadMs},
	{Name: "PageLoad.ParseTiming.NavigationToParseStart", Bounds: boundsPageLoadMs},
	// INP: worst-case interaction latency (input to next paint), high
	// percentile per page load.
	{Name: "PageLoad.InteractiveTiming.UserInteractionLatency.HighPercentile2.MaxEventDuration", Bounds: boundsInteractionMs},
	// FID: input delay of the first interaction per page load.
	{Name: "PageLoad.InteractiveTiming.FirstInputDelay4", Bounds: boundsInteractionMs},
	// Interactions per page load; unitless count.
	{Name: "PageLoad.InteractiveTiming.NumInteractions", Bounds: boundsCount},
	// CLS: unitless layout-shift score, not milliseconds.
	{Name: "PageLoad.LayoutInstability.MaxCumulativeShiftScore.SessionWindow.Gap1000ms.Max5000ms2", Bounds: boundsShiftScore},
	// CPU milliseconds consumed by the page across its lifetime.
	{Name: "PageLoad.Cpu.TotalUsage", Bounds: boundsCPUMs},
	// Browser start to first web contents paint; one sample per browser
	// start, measures how quickly a fresh VM renders content.
	{Name: "Startup.FirstWebContents.NonEmptyPaint3", Bounds: boundsPageLoadMs},
	// Peak GPU memory during scroll sequences, in MB.
	{Name: "Memory.GPU.PeakMemoryUsage2.Scroll", Bounds: boundsMB},
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
	histograms []UMAHistogram
}

func NewChromeCollector(upstream DevToolsUpstream) *ChromeCollector {
	return &ChromeCollector{upstream: upstream, histograms: DefaultUMAHistograms}
}

func (c *ChromeCollector) Name() string { return "chrome" }

func (c *ChromeCollector) Collect(ctx context.Context, w *Writer) error {
	w.Metric("kernel_chromium_up", "Whether the Chromium DevTools endpoint is reachable and responsive.", "gauge")

	client, version, err := c.dial(ctx)
	if err != nil {
		// An unreachable browser is a routine, load-bearing signal (the
		// browser may simply be restarting), not a collector failure:
		// return nil so the up sample survives the handler's
		// discard-on-error policy.
		w.Sample("kernel_chromium_up", nil, 0)
		return nil
	}
	defer client.Close()
	w.Sample("kernel_chromium_up", nil, 1)

	w.Metric("kernel_chromium_info", "Chromium build info; value is always 1.", "gauge")
	w.Sample("kernel_chromium_info", []Label{{"product", version.Product}}, 1)

	if pages, err := client.CountPageTargets(ctx); err == nil {
		w.Metric("kernel_chromium_pages", "Number of open page targets (tabs).", "gauge")
		w.Sample("kernel_chromium_pages", nil, float64(pages))
	}

	fetched, err := c.fetchHistograms(ctx, client)
	if err != nil {
		return err
	}
	w.Metric("kernel_chromium_uma",
		"Chrome UMA histogram, cumulative since browser start, re-bucketed onto fixed bounds. Units follow the UMA definition: PageLoad timings are milliseconds, NumInteractions is a count, CLS is a unitless score, Memory.GPU.* is MB.",
		"histogram")
	for _, uma := range c.histograms {
		if h, ok := fetched[uma.Name]; ok {
			writeUMAHistogram(w, h, uma.Bounds)
		}
	}
	return nil
}

// fetchHistograms retrieves the curated histograms in as few CDP round
// trips as possible: one substring query covers the whole PageLoad family,
// plus one query per remaining name. The query is a substring filter, so
// results also contain suffixed variants — only exact names are kept.
func (c *ChromeCollector) fetchHistograms(ctx context.Context, client *cdpclient.Client) (map[string]cdpclient.Histogram, error) {
	const pageLoadPrefix = "PageLoad."

	queries := make([]string, 0, len(c.histograms))
	hasPageLoad := false
	for _, uma := range c.histograms {
		if strings.HasPrefix(uma.Name, pageLoadPrefix) {
			hasPageLoad = true
			continue
		}
		queries = append(queries, uma.Name)
	}
	if hasPageLoad {
		queries = append(queries, pageLoadPrefix)
	}

	wanted := make(map[string]struct{}, len(c.histograms))
	for _, uma := range c.histograms {
		wanted[uma.Name] = struct{}{}
	}

	fetched := make(map[string]cdpclient.Histogram, len(c.histograms))
	for _, query := range queries {
		matches, err := client.GetHistograms(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, h := range matches {
			if _, ok := wanted[h.Name]; ok {
				fetched[h.Name] = h
			}
		}
	}
	return fetched, nil
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

// writeUMAHistogram emits one UMA histogram as a Prometheus histogram with
// a "histogram" label carrying the UMA name. Chrome's sparse [low, high)
// buckets are re-bucketed onto the fixed bounds: each chrome bucket counts
// toward the smallest fixed bound >= its upper edge (rounding a straddling
// bucket up, biased at most one fixed bucket wide). All fixed bounds are
// emitted, including empty ones, so every VM produces an identical le set.
func writeUMAHistogram(w *Writer, h cdpclient.Histogram, bounds []int64) {
	cumulative := make([]int64, len(bounds))
	bucketSum := int64(0)
	for _, b := range h.Buckets {
		bucketSum += b.Count
		if b.High >= math.MaxInt32 {
			// Overflow bucket: counted in +Inf only.
			continue
		}
		for i, bound := range bounds {
			if b.High <= bound {
				cumulative[i] += b.Count
				break
			}
		}
	}
	for i := 1; i < len(cumulative); i++ {
		cumulative[i] += cumulative[i-1]
	}
	// Chrome's count and its bucket sum are read from live memory and can
	// disagree; taking the max keeps +Inf >= the last bucket.
	count := max(h.Count, bucketSum)

	labels := func(le string) []Label {
		return []Label{{"histogram", h.Name}, {"le", le}}
	}
	for i, bound := range bounds {
		w.Sample("kernel_chromium_uma_bucket", labels(strconv.FormatInt(bound, 10)), float64(cumulative[i]))
	}
	w.Sample("kernel_chromium_uma_bucket", labels("+Inf"), float64(count))
	w.Sample("kernel_chromium_uma_sum", []Label{{"histogram", h.Name}}, float64(h.Sum))
	w.Sample("kernel_chromium_uma_count", []Label{{"histogram", h.Name}}, float64(count))
}
