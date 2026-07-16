//go:build livetest

// Live end-to-end test of the OTLP export path against a real dockerized
// OpenTelemetry collector. Run with: go test -tags livetest -run TestLiveOTLPExport ./lib/events/
package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveOTLPExport(t *testing.T) {
	const container = "kernel-otlp-livetest"
	_ = exec.Command("docker", "rm", "-f", container).Run()

	run := exec.Command("docker", "run", "-d", "--name", container, "-p", "4318:4318",
		"otel/opentelemetry-collector-contrib",
		"--set=receivers.otlp.protocols.http.endpoint=0.0.0.0:4318",
		"--set=exporters.debug.verbosity=detailed",
		"--set=service.pipelines.logs.receivers=[otlp]",
		"--set=service.pipelines.logs.exporters=[debug]",
	)
	out, err := run.CombinedOutput()
	require.NoError(t, err, "docker run: %s", out)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	time.Sleep(4 * time.Second) // let the collector bind 4318

	es, err := NewEventStream(EventStreamConfig{RingCapacity: 1024})
	require.NoError(t, err)
	m := &OTLPMetrics{}
	ctrl := NewOTLPExportController(es, OTLPConfig{
		Endpoint:       "127.0.0.1:4318",
		Insecure:       true,
		ServiceName:    "kernel-browser",
		InstanceName:   "live-inst",
		Metro:          "live-metro",
		MaxQueueSize:   256,
		ExportInterval: 500 * time.Millisecond,
		ExportTimeout:  5 * time.Second,
		Metrics:        m,
	}, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	require.NoError(t, ctrl.Start(context.Background()))
	require.True(t, ctrl.Running())

	publish := func(typ string, cat oapi.TelemetryEventCategory, data any) {
		b, _ := json.Marshal(data)
		es.Publish(Envelope{Event: Event{Ts: time.Now().UnixMicro(), Type: typ, Category: cat, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}, Data: b}})
	}
	publish("page_crashed", Page, oapi.BrowserPageCrashedEventData{TargetId: "t1", TargetType: "page", Url: "https://crash.example.com"})
	publish("system_oom_kill", System, map[string]any{"process_name": "chromium", "pid": 42})
	publish("network_response", Network, map[string]any{"url": "https://ok.example.com", "status": 200, "method": "GET"})
	publish("monitor_screenshot", Screenshot, map[string]any{"data": "BASE64"}) // excluded category

	time.Sleep(2 * time.Second)
	require.NoError(t, ctrl.Stop(context.Background())) // drains + flushes
	assert.False(t, ctrl.Running())

	logs, err := exec.Command("docker", "logs", container).CombinedOutput()
	require.NoError(t, err)
	got := string(logs)
	t.Logf("collector received:\n%s", lastLines(got, 60))

	// #2: crash/OOM land as ERROR, normal events as INFO.
	assert.Contains(t, got, "page_crashed", "renderer crash event should reach the collector")
	assert.Contains(t, got, "system_oom_kill")
	assert.Contains(t, got, "network_response")
	assert.Contains(t, got, "SeverityText: ERROR", "crash/OOM must be ERROR severity")
	// excluded category must never be exported
	assert.NotContains(t, got, "monitor_screenshot", "screenshot category must be excluded")
	// promoted attribute from the converter
	assert.Contains(t, got, "http.response.status_code")

	// #6: exported counter advanced, nothing failed or dropped.
	assert.GreaterOrEqual(t, m.Exported(), uint64(3), "3 non-excluded events should export")
	assert.Equal(t, uint64(0), m.Failures())
	assert.Equal(t, uint64(0), m.Dropped())
	t.Logf("metrics: exported=%d failures=%d dropped=%d", m.Exported(), m.Failures(), m.Dropped())
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestLiveOTLPDropMetric forces a real batch-queue overflow (tiny queue + a
// hung endpoint that blocks exports) and asserts the drop-counting log handler
// increments OTLPMetrics.Dropped via the OTel SDK's global logger.
func TestLiveOTLPDropMetric(t *testing.T) {
	// A TCP server that accepts and then hangs, so each export blocks until the
	// export timeout and the queue backs up.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // hold it open, never respond
		}
	}()

	m := &OTLPMetrics{}
	// Mirror main.go: the SDK reports drops just below Info, so the base handler
	// must admit that level or slog short-circuits before the counter runs.
	base := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo - 1})
	otel.SetLogger(logr.FromSlogHandler(NewDropCountingHandler(base, m)))
	t.Cleanup(func() { otel.SetLogger(logr.Discard()) })

	es, err := NewEventStream(EventStreamConfig{RingCapacity: 4096})
	require.NoError(t, err)
	ctrl := NewOTLPExportController(es, OTLPConfig{
		Endpoint:       ln.Addr().String(),
		Insecure:       true,
		MaxQueueSize:   4, // tiny buffer: overflow under a flood while export blocks
		ExportInterval: 50 * time.Millisecond,
		ExportTimeout:  2 * time.Second,
		Metrics:        m,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, ctrl.Start(context.Background()))

	for i := 0; i < 2000; i++ {
		es.Publish(Envelope{Event: Event{Ts: time.Now().UnixMicro(), Type: "network_response", Category: Network, Source: oapi.BrowserEventSource{Kind: oapi.Cdp}, Data: []byte(`{"status":200}`)}})
	}
	time.Sleep(2 * time.Second)
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = ctrl.Stop(stopCtx)

	t.Logf("metrics: exported=%d failures=%d dropped=%d", m.Exported(), m.Failures(), m.Dropped())
	assert.Greater(t, m.Dropped(), uint64(0), "queue overflow should be counted as drops")
}
