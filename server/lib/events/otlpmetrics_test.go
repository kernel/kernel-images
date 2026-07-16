package events

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDropCountingHandler_CountsDrops confirms the handler sums the SDK's
// drop-report count and ignores unrelated log records.
func TestDropCountingHandler_CountsDrops(t *testing.T) {
	m := &OTLPMetrics{}
	h := NewDropCountingHandler(slog.NewTextHandler(discard{}, nil), m)
	log := slog.New(h)

	log.Warn(otlpDroppedLogMsg, otlpDroppedLogAttr, uint64(5))
	log.Warn(otlpDroppedLogMsg, otlpDroppedLogAttr, 3) // boxed int, as the logr bridge may pass it
	log.Info("something else", "dropped", 99)          // unrelated message must not count

	assert.Equal(t, uint64(8), m.Dropped())
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
