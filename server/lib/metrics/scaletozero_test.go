package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseDrainCollector(t *testing.T) {
	collector := NewResponseDrainCollector(func() map[string]uint64 {
		return map[string]uint64{
			"timeout": 2,
			"drained": 7,
		}
	}, func() int64 { return 3 }, func() int64 { return 1 })
	writer := &Writer{}

	require.NoError(t, collector.Collect(context.Background(), writer))

	assert.Equal(t, `# HELP kernel_scale_to_zero_response_drain_total HTTP response drain events before scale-to-zero is re-enabled.
# TYPE kernel_scale_to_zero_response_drain_total counter
kernel_scale_to_zero_response_drain_total{outcome="drained"} 7
kernel_scale_to_zero_response_drain_total{outcome="timeout"} 2
# HELP kernel_scale_to_zero_response_holds HTTP response scale-to-zero holds currently active.
# TYPE kernel_scale_to_zero_response_holds gauge
kernel_scale_to_zero_response_holds 3
# HELP kernel_scale_to_zero_response_fail_closed_holds HTTP response holds awaiting terminal connection recovery.
# TYPE kernel_scale_to_zero_response_fail_closed_holds gauge
kernel_scale_to_zero_response_fail_closed_holds 1
`, string(writer.Bytes()))
}
