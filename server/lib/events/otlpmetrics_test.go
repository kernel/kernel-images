package events

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
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

// TestOTLPMetrics_CountsRealDrops forces a real batch-queue overflow through
// the OTel SDK and asserts the drop count reaches OTLPMetrics via the global
// logger. This exercises the full SDK -> global logger -> counting handler
// chain every run, so an SDK bump that renames the drop-report message fails
// here instead of silently zeroing the metric. No external services: a local
// listener accepts and hangs so each export blocks and the tiny queue overflows.
func TestOTLPMetrics_CountsRealDrops(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // hold open, never respond
		}
	}()

	m := &OTLPMetrics{}
	// The SDK reports drops just below Info, so the base handler must admit that
	// level or slog short-circuits before the counter runs (mirrors main.go).
	base := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo - 1})
	otel.SetLogger(logr.FromSlogHandler(NewDropCountingHandler(base, m)))
	t.Cleanup(func() { otel.SetLogger(logr.Discard()) })

	es, err := NewEventStream(EventStreamConfig{RingCapacity: 4096})
	require.NoError(t, err)
	ctrl := NewOTLPExportController(es, OTLPConfig{
		Endpoint:       ln.Addr().String(),
		Insecure:       true,
		MaxQueueSize:   4, // tiny buffer overflows while the first export blocks
		ExportInterval: 50 * time.Millisecond,
		ExportTimeout:  time.Second,
		Metrics:        m,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, ctrl.Start(context.Background()))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ctrl.Stop(stopCtx)
	})

	for i := 0; i < 2000; i++ {
		es.Publish(Envelope{Event: Event{Ts: time.Now().UnixMicro(), Type: "network_response", Category: Network, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}, Data: []byte(`{"status":200}`)}})
	}
	require.Eventually(t, func() bool { return m.Dropped() > 0 }, 4*time.Second, 100*time.Millisecond,
		"queue overflow should be counted as drops via the SDK global logger")
}
