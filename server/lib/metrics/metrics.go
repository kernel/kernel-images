// Package metrics exposes VM- and browser-level metrics in the Prometheus
// text exposition format, intended to be scraped by an external collector
// and aggregated across VMs. Metrics carry no per-session or per-user
// labels; instance identity is expected to be attached (and later
// aggregated away) by the scraper.
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Collector renders one group of metrics for a single scrape.
type Collector interface {
	// Name identifies the collector in error logs.
	Name() string
	// Collect appends samples to w. Returning an error does not fail the
	// scrape; the other collectors' output is still served.
	Collect(ctx context.Context, w *Writer) error
}

// Label is a single Prometheus label pair.
type Label struct {
	Name  string
	Value string
}

// Writer accumulates Prometheus text-format output.
type Writer struct {
	buf bytes.Buffer
}

// Metric writes the HELP and TYPE header lines for a metric family.
func (w *Writer) Metric(name, help, typ string) {
	fmt.Fprintf(&w.buf, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// Sample writes one sample line. name may carry a family suffix such as
// _bucket, _sum, or _count for histogram families.
func (w *Writer) Sample(name string, labels []Label, value float64) {
	w.buf.WriteString(name)
	if len(labels) > 0 {
		w.buf.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				w.buf.WriteByte(',')
			}
			// %q emits the exposition-format escapes: \\, \", and \n.
			fmt.Fprintf(&w.buf, "%s=%q", l.Name, l.Value)
		}
		w.buf.WriteByte('}')
	}
	w.buf.WriteByte(' ')
	w.buf.WriteString(formatValue(value))
	w.buf.WriteByte('\n')
}

func (w *Writer) Bytes() []byte {
	return w.buf.Bytes()
}

func formatValue(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

const scrapeTimeout = 10 * time.Second

// Handler serves GET /metrics. Scrapes are serialized: concurrent requests
// wait rather than probing Chrome and nvidia-smi in parallel.
func Handler(log *slog.Logger, collectors ...Collector) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		ctx, cancel := context.WithTimeout(r.Context(), scrapeTimeout)
		defer cancel()

		w := &Writer{}
		for _, c := range collectors {
			if err := c.Collect(ctx, w); err != nil {
				log.Error("metrics collector failed", "collector", c.Name(), "err", err)
			}
		}
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		rw.Write(w.Bytes())
	})
}
