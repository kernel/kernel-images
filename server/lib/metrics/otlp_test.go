package metrics

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubOTLPSource struct{ dropped, failures, exported uint64 }

func (s stubOTLPSource) Dropped() uint64  { return s.dropped }
func (s stubOTLPSource) Failures() uint64 { return s.failures }
func (s stubOTLPSource) Exported() uint64 { return s.exported }

func TestOTLPCollector(t *testing.T) {
	w := &Writer{}
	err := NewOTLPCollector(stubOTLPSource{dropped: 7, failures: 2, exported: 42}).Collect(context.Background(), w)
	require.NoError(t, err)

	out := string(w.Bytes())
	assert.Contains(t, out, "kernel_otlp_records_dropped_total 7")
	assert.Contains(t, out, "kernel_otlp_export_failures_total 2")
	assert.Contains(t, out, "kernel_otlp_records_exported_total 42")
	assert.Contains(t, out, "# TYPE kernel_otlp_records_dropped_total counter")
	assert.Equal(t, 1, strings.Count(out, "# TYPE kernel_otlp_records_dropped_total"))
}
