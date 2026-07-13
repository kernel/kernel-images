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
	"go.opentelemetry.io/otel/log"
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
	recs := []sdklog.Record{recordOfSize(10)}
	failing := &loggingExporter{Exporter: stubExporter{err: errors.New("boom")}, log: slog.Default()}
	require.Error(t, failing.Export(context.Background(), recs))
	require.Error(t, failing.Export(context.Background(), recs))
	assert.Equal(t, uint64(2), failing.failures.Load())

	ok := &loggingExporter{Exporter: stubExporter{}, log: slog.Default()}
	require.NoError(t, ok.Export(context.Background(), recs))
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

// recordOfSize builds a log record whose body string is n bytes, so
// estimateRecordBytes reports ~n.
func recordOfSize(n int) sdklog.Record {
	var r sdklog.Record
	r.SetBody(log.StringValue(strings.Repeat("x", n)))
	return r
}

// TestChunkBySize confirms an export is split into sub-requests that each stay
// under the byte budget, so a batch of large records can't exceed the target's
// HTTP body limit.
func TestChunkBySize(t *testing.T) {
	const mb = 1_000_000

	t.Run("empty is nil", func(t *testing.T) {
		assert.Nil(t, chunkBySize(nil, maxOTLPExportBytes))
	})

	t.Run("small batch stays whole", func(t *testing.T) {
		recs := []sdklog.Record{recordOfSize(1000), recordOfSize(1000), recordOfSize(1000)}
		chunks := chunkBySize(recs, maxOTLPExportBytes)
		require.Len(t, chunks, 1)
		assert.Len(t, chunks[0], 3)
	})

	t.Run("large batch splits under budget", func(t *testing.T) {
		recs := make([]sdklog.Record, 10) // 10 x 1MB, budget 4MiB -> 4,4,2
		for i := range recs {
			recs[i] = recordOfSize(mb)
		}
		chunks := chunkBySize(recs, maxOTLPExportBytes)
		require.Len(t, chunks, 3)
		assert.Equal(t, []int{4, 4, 2}, []int{len(chunks[0]), len(chunks[1]), len(chunks[2])})
		for _, c := range chunks {
			total := 0
			for i := range c {
				total += estimateRecordBytes(&c[i])
			}
			assert.LessOrEqual(t, total, maxOTLPExportBytes)
		}
	})

	t.Run("oversized single record ships alone", func(t *testing.T) {
		recs := []sdklog.Record{recordOfSize(maxOTLPExportBytes + mb)}
		chunks := chunkBySize(recs, maxOTLPExportBytes)
		require.Len(t, chunks, 1)
		assert.Len(t, chunks[0], 1)
	})
}
