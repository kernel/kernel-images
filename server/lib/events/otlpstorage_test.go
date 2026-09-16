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
	"sync/atomic"
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

// TestLoggingExporter_CountsExportFailures confirms the wrapper records export
// failures and successfully exported records into the shared metrics.
func TestLoggingExporter_CountsExportFailures(t *testing.T) {
	recs := []sdklog.Record{recordOfSize(10)}
	fm := &OTLPMetrics{}
	failing := &loggingExporter{Exporter: stubExporter{err: errors.New("boom")}, log: slog.Default(), metrics: fm}
	require.Error(t, failing.Export(context.Background(), recs))
	require.Error(t, failing.Export(context.Background(), recs))
	assert.Equal(t, uint64(2), fm.Failures())
	assert.Equal(t, uint64(0), fm.Exported())

	om := &OTLPMetrics{}
	ok := &loggingExporter{Exporter: stubExporter{}, log: slog.Default(), metrics: om}
	require.NoError(t, ok.Export(context.Background(), recs))
	assert.Equal(t, uint64(0), om.Failures())
	assert.Equal(t, uint64(1), om.Exported())
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
		Endpoint:      strings.TrimPrefix(srv.URL, "http://"),
		URLPath:       "/otlp-relay/v1/logs",
		Insecure:      true,
		AuthTokenFunc: func() string { return "test-jwt" },
		ServiceName:   "kernel-browser",
		InstanceName:  "browser-1",
		Metro:         "dev-iad",
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
	// The relay authenticates the VM by its instance JWT, sent as a bearer token.
	assert.Equal(t, "Bearer test-jwt", auths[0])
	// The excluded screenshot must not reach the receiver; only the network event does.
	assert.Equal(t, []string{"network_response"}, eventNames)
	// Promoted attributes and the structured body survive the SDK to protobuf translation.
	assert.Equal(t, "https://x", attrStr["url.full"])
	assert.Equal(t, "GET", attrStr["http.request.method"])
	assert.Equal(t, int64(200), statusAttr)
	assert.True(t, bodyIsMap, "structured body should arrive as a kvlist")
}

// TestOTLPExportController_NoReplayOnReenable confirms that toggling export off
// and back on does not re-export events already sent, and does not export events
// published while it was off: the rebuilt writer starts from the stream tail.
func TestOTLPExportController_NoReplayOnReenable(t *testing.T) {
	var mu sync.Mutex
	var urls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req collogspb.ExportLogsServiceRequest
		require.NoError(t, proto.Unmarshal(body, &req))
		mu.Lock()
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					for _, kv := range lr.Attributes {
						if kv.Key == "url.full" {
							urls = append(urls, kv.Value.GetStringValue())
						}
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

	publish := func(url string) {
		es.Publish(Envelope{Event: Event{Ts: 1, Type: "network_response", Category: Network,
			Data: []byte(`{"method":"GET","url":"` + url + `","status":200}`)}})
	}

	ctrl := NewOTLPExportController(es, OTLPConfig{
		Endpoint: strings.TrimPrefix(srv.URL, "http://"),
		URLPath:  "/v1/logs",
		Insecure: true,
	}, slog.Default())

	// First enable: only events published while on are exported.
	require.NoError(t, ctrl.Start(context.Background()))
	publish("https://one")
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ctrl.Stop(stopCtx))

	// Published while off: must never be exported.
	publish("https://gap")

	// Re-enable: must not replay "one" or "gap", only forward new events.
	require.NoError(t, ctrl.Start(context.Background()))
	publish("https://two")
	stopCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	require.NoError(t, ctrl.Stop(stopCtx2))

	mu.Lock()
	defer mu.Unlock()
	assert.ElementsMatch(t, []string{"https://one", "https://two"}, urls,
		"each event exported exactly once; no ring replay and no off-window events")
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

// TestOTLPStorageWriter_RefreshesAuthToken confirms AuthTokenFunc is read per
// request, not captured once: a token that changes after the exporter starts is
// reflected on later requests. This is what lets a forked VM export with the
// fresh instance JWT from its applied identity payload without a restart.
func TestOTLPStorageWriter_RefreshesAuthToken(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var token atomic.Value
	token.Store("jwt-1")

	es, err := NewEventStream(EventStreamConfig{RingCapacity: 64})
	require.NoError(t, err)

	cfg := OTLPConfig{
		Endpoint:       strings.TrimPrefix(srv.URL, "http://"),
		URLPath:        "/otlp-relay/v1/logs",
		Insecure:       true,
		AuthTokenFunc:  func() string { return token.Load().(string) },
		ServiceName:    "kernel-browser",
		ExportInterval: 20 * time.Millisecond,
	}
	wtr := NewOTLPStorageWriter(es, cfg, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, wtr.Start(ctx))

	seen := func(want string) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, a := range auths {
				if a == want {
					return true
				}
			}
			return false
		}
	}

	es.Publish(Envelope{Event: Event{Ts: 1, Type: "network_response", Category: Network,
		Data: []byte(`{"method":"GET","url":"https://x","status":200}`)}})
	require.Eventually(t, seen("Bearer jwt-1"), 3*time.Second, 10*time.Millisecond, "first export should use jwt-1")

	token.Store("jwt-2")
	es.Publish(Envelope{Event: Event{Ts: 2, Type: "network_response", Category: Network,
		Data: []byte(`{"method":"GET","url":"https://y","status":200}`)}})
	require.Eventually(t, seen("Bearer jwt-2"), 3*time.Second, 10*time.Millisecond, "later export should pick up the refreshed jwt-2")

	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, wtr.Stop(stopCtx))
}

// TestBearerRoundTripper_AuthenticatesOnlyConfiguredHost covers the host bound:
// net/http's cross-host strip never sees a header set in a RoundTripper.
func TestBearerRoundTripper_AuthenticatesOnlyConfiguredHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string
		wantAuth string
	}{
		{"configured host", "http://relay.example:4000/otlp-relay/v1/logs", "Bearer jwt"},
		{"same host uppercased", "http://RELAY.EXAMPLE:4000/otlp-relay/v1/logs", "Bearer jwt"},
		{"other host", "http://elsewhere.example/v1/logs", ""},
		{"same host other port", "http://relay.example:9999/v1/logs", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			rt := &bearerRoundTripper{
				base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					got = r.Header.Get("Authorization")
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
				}),
				token: func() string { return "jwt" },
				host:  "relay.example:4000",
			}
			req, err := http.NewRequest(http.MethodPost, tc.url, nil)
			require.NoError(t, err)
			_, err = rt.RoundTrip(req)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAuth, got)
		})
	}
}

// TestOTLPStorageWriter_DoesNotFollowRedirects confirms a redirect from the
// endpoint surfaces as a response rather than being chased.
func TestOTLPStorageWriter_DoesNotFollowRedirects(t *testing.T) {
	var redirectTargetCalls atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	var endpointCalls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		endpointCalls.Add(1)
		w.Header().Set("Location", redirectTarget.URL+"/v1/logs")
		w.WriteHeader(http.StatusFound)
	}))
	defer endpoint.Close()

	es, err := NewEventStream(EventStreamConfig{RingCapacity: 64})
	require.NoError(t, err)

	cfg := OTLPConfig{
		Endpoint:       strings.TrimPrefix(endpoint.URL, "http://"),
		URLPath:        "/otlp-relay/v1/logs",
		Insecure:       true,
		AuthTokenFunc:  func() string { return "jwt" },
		ServiceName:    "kernel-browser",
		ExportInterval: 20 * time.Millisecond,
	}
	wtr := NewOTLPStorageWriter(es, cfg, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, wtr.Start(ctx))

	es.Publish(Envelope{Event: Event{Ts: 1, Type: "network_response", Category: Network,
		Data: []byte(`{"method":"GET","url":"https://x","status":200}`)}})
	require.Eventually(t, func() bool { return endpointCalls.Load() > 0 }, 3*time.Second, 10*time.Millisecond,
		"the export should reach the configured endpoint")
	assert.Zero(t, redirectTargetCalls.Load(), "the exporter must not follow the endpoint's redirect")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
