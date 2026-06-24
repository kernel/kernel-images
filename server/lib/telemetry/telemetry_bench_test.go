package telemetry

import (
	"encoding/json"
	"testing"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func benchmarkTelemetryEvent() events.Event {
	return events.Event{
		Ts:       123456789,
		Type:     "console.log",
		Category: events.Console,
		Source:   oapi.BrowserEventSource{Kind: oapi.Cdp},
		Data:     json.RawMessage(`{"message":"hello","level":"log"}`),
	}
}

func BenchmarkTelemetrySessionPublish(b *testing.B) {
	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: 1024})
	if err != nil {
		b.Fatal(err)
	}
	ts := NewTelemetrySession(es)
	ts.Start("bench-session", TelemetryConfig{Categories: events.UserCategories})
	ev := benchmarkTelemetryEvent()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := ts.Publish(ev); !ok {
			b.Fatal("expected event to publish")
		}
	}
}
