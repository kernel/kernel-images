package events

import (
	"encoding/json"
	"testing"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func benchmarkEvent() Event {
	return Event{
		Ts:       123456789,
		Type:     "console.log",
		Category: Console,
		Source:   oapi.BrowserEventSource{Kind: oapi.Cdp},
		Data:     json.RawMessage(`{"message":"hello","level":"log"}`),
	}
}

func BenchmarkEventStreamPublish(b *testing.B) {
	es, err := NewEventStream(EventStreamConfig{RingCapacity: 1024})
	if err != nil {
		b.Fatal(err)
	}
	ev := benchmarkEvent()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		es.Publish(Envelope{Event: ev})
	}
}

func BenchmarkEventStreamPublishRead(b *testing.B) {
	es, err := NewEventStream(EventStreamConfig{RingCapacity: 1024})
	if err != nil {
		b.Fatal(err)
	}
	reader := es.NewReader(0)
	ev := benchmarkEvent()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		es.Publish(Envelope{Event: ev})
		res, ok := reader.TryRead()
		if !ok || res.Envelope == nil {
			b.Fatal("expected envelope")
		}
	}
}
