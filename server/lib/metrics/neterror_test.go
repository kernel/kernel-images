package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kernel/kernel-images/server/lib/neterror"
)

type stubNetErrorSource struct {
	counts   map[neterror.Key]int64
	overflow int64
}

func (s stubNetErrorSource) Snapshot() (map[neterror.Key]int64, int64) {
	return s.counts, s.overflow
}

func TestNetErrorCollector(t *testing.T) {
	c := NewNetErrorCollector(stubNetErrorSource{
		counts: map[neterror.Key]int64{
			{Error: "net::ERR_TIMED_OUT", ResourceType: "XHR"}:                 3,
			{Error: "net::ERR_HTTP2_PROTOCOL_ERROR", ResourceType: "Document"}: 12,
		},
		overflow: 2,
	})

	w := &Writer{}
	require.NoError(t, c.Collect(context.Background(), w))

	out := string(w.Bytes())
	assert.Contains(t, out, "# TYPE kernel_chromium_net_errors_total counter\n")
	// Samples are ordered by error then resource type so an unchanged tracker
	// scrapes byte-identically.
	assert.Contains(t, out, `kernel_chromium_net_errors_total{error="net::ERR_HTTP2_PROTOCOL_ERROR",resource_type="Document"} 12
kernel_chromium_net_errors_total{error="net::ERR_TIMED_OUT",resource_type="XHR"} 3
`)
	assert.Contains(t, out, "kernel_chromium_net_errors_dropped_total 2\n")
}

// With no failures the labelled family has no samples and so is absent from
// queries; the unlabelled dropped counter is what proves the VM reported.
func TestNetErrorCollectorWithNoFailures(t *testing.T) {
	c := NewNetErrorCollector(stubNetErrorSource{})

	w := &Writer{}
	require.NoError(t, c.Collect(context.Background(), w))

	out := string(w.Bytes())
	assert.NotContains(t, out, "kernel_chromium_net_errors_total{")
	assert.Contains(t, out, "kernel_chromium_net_errors_dropped_total 0\n")
}

var _ NetErrorSource = (*neterror.Tracker)(nil)
