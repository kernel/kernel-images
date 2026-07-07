package events

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// stubExporter is a controllable sdklog.Exporter for testing the wrapper.
type stubExporter struct{ err error }

func (s stubExporter) Export(context.Context, []sdklog.Record) error { return s.err }
func (s stubExporter) Shutdown(context.Context) error                { return nil }
func (s stubExporter) ForceFlush(context.Context) error              { return nil }

// TestLoggingExporter_CountsExportFailures confirms the wrapper surfaces export
// failures (the reason it exists) and leaves the counter untouched on success.
func TestLoggingExporter_CountsExportFailures(t *testing.T) {
	failing := &loggingExporter{Exporter: stubExporter{err: errors.New("boom")}, log: slog.Default()}
	require.Error(t, failing.Export(context.Background(), nil))
	require.Error(t, failing.Export(context.Background(), nil))
	assert.Equal(t, uint64(2), failing.failures.Load())

	ok := &loggingExporter{Exporter: stubExporter{}, log: slog.Default()}
	require.NoError(t, ok.Export(context.Background(), nil))
	assert.Equal(t, uint64(0), ok.failures.Load())
}

// TestOTLPStorageWriter_ExportsEvents drives the full sink against a local HTTP
// server standing in for the relay/collector. It decodes the exported OTLP
// payload to confirm the request lands with the configured path/headers and
// that an excluded category (screenshot) never reaches the receiver.
func TestOTLPStorageWriter_ExportsEvents(t *testing.T) {
	var mu sync.Mutex
	var paths, auths, eventNames []string
	attrStr := map[string]string{}
	var statusAttr int64
	var bodyIsMap bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req collogspb.ExportLogsServiceRequest
		require.NoError(t, proto.Unmarshal(body, &req))
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					eventNames = append(eventNames, lr.EventName)
					for _, kv := range lr.Attributes {
						if kv.Key == "http.response.status_code" {
							statusAttr = kv.Value.GetIntValue()
						} else {
							attrStr[kv.Key] = kv.Value.GetStringValue()
						}
					}
					if lr.Body.GetKvlistValue() != nil {
						bodyIsMap = true
					}
				}
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	es, err := NewEventStream(EventStreamConfig{RingCapacity: 64})
	require.NoError(t, err)

	cfg := OTLPConfig{
		Endpoint:     strings.TrimPrefix(srv.URL, "http://"),
		URLPath:      "/otlp-relay/v1/logs",
		Insecure:     true,
		Headers:      map[string]string{"Authorization": "Bearer test-jwt"},
		ServiceName:  "kernel-browser",
		InstanceName: "browser-1",
		Metro:        "dev-iad",
	}
	wtr := NewOTLPStorageWriter(es, cfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, wtr.Start(ctx))

	es.Publish(Envelope{Event: Event{Ts: 1, Type: "network_response", Category: Network,
		Data: []byte(`{"method":"GET","url":"https://x","status":200}`)}})
	es.Publish(Envelope{Event: Event{Ts: 2, Type: "screenshot", Category: Screenshot, Data: []byte(`{"png":"..."}`)}})

	cancel() // stop the Run loop; Stop then drains the ring and flushes
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, wtr.Stop(stopCtx))

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, paths, "expected at least one export request")
	assert.Equal(t, "/otlp-relay/v1/logs", paths[0])
	assert.Equal(t, "Bearer test-jwt", auths[0])
	// The excluded screenshot must not reach the receiver; only the network event does.
	assert.Equal(t, []string{"network_response"}, eventNames)
	// Promoted attributes and the structured body survive the SDK to protobuf translation.
	assert.Equal(t, "https://x", attrStr["url.full"])
	assert.Equal(t, "GET", attrStr["http.request.method"])
	assert.Equal(t, int64(200), statusAttr)
	assert.True(t, bodyIsMap, "structured body should arrive as a kvlist")
}
