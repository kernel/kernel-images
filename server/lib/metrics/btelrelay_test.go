package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRelaySource struct{}

func (stubRelaySource) Exported() uint64  { return 42 }
func (stubRelaySource) Retries() uint64   { return 3 }
func (stubRelaySource) QueueDepth() int64 { return 5 }
func (stubRelaySource) Dropped() map[string]uint64 {
	return map[string]uint64{"queue_full": 7, "rejected": 2}
}
func (stubRelaySource) Failed() map[string]uint64 { return map[string]uint64{"403": 1} }

func TestRelayCollector(t *testing.T) {
	w := &Writer{}
	require.NoError(t, NewRelayCollector(stubRelaySource{}).Collect(context.Background(), w))

	out := string(w.Bytes())
	assert.Contains(t, out, "kernel_btel_relay_exported_total 42")
	assert.Contains(t, out, `kernel_btel_relay_dropped_total{reason="queue_full"} 7`)
	assert.Contains(t, out, `kernel_btel_relay_dropped_total{reason="rejected"} 2`)
	assert.Contains(t, out, "kernel_btel_relay_retries_total 3")
	assert.Contains(t, out, `kernel_btel_relay_failed_total{status="403"} 1`)
	assert.Contains(t, out, "kernel_btel_relay_queue_depth 5")
	assert.Contains(t, out, "# TYPE kernel_btel_relay_queue_depth gauge")
}
