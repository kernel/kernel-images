package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrowserLocationCollector(t *testing.T) {
	collector := NewBrowserLocationCollector(func() BrowserLocationSnapshot {
		return BrowserLocationSnapshot{Accepted: 2, Applied: 1, Retries: 3, Converged: false, TimeZoneConverged: true}
	})
	writer := &Writer{}
	require.NoError(t, collector.Collect(context.Background(), writer))
	out := string(writer.Bytes())
	assert.Contains(t, out, "kernel_browser_location_accepted_total 2")
	assert.Contains(t, out, "kernel_browser_location_applied_total 1")
	assert.Contains(t, out, "kernel_browser_location_retries_total 3")
	assert.Contains(t, out, `kernel_browser_location_component_converged{component="timezone"} 1`)
	assert.Contains(t, out, `kernel_browser_location_component_converged{component="renderers"} 0`)
}
