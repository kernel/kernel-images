package cdpmonitor

import (
	"fmt"
	"testing"
)

func BenchmarkMetricsOnlyTerminalDispatch(b *testing.B) {
	m := New(newTestUpstream(""), newEventCollector().publishFn(), 0, discardLogger, nil)
	_ = m.SetTelemetry(false)
	messages := make([]cdpMessage, 2*terminalHistorySize)
	for i := range messages {
		messages[i] = cdpMessage{SessionID: "session", Method: "Network.loadingFailed", Params: []byte(fmt.Sprintf(`{"requestId":"%d","errorText":"net::ERR_CONNECTION_RESET"}`, i))}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.dispatchEvent(messages[i%len(messages)])
	}
}
