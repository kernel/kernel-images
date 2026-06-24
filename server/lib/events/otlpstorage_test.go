package events

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOTLPStorageWriter_ExportsEvents drives the full sink against a local HTTP
// server standing in for the relay/collector, verifying records reach the
// configured path with the configured headers.
func TestOTLPStorageWriter_ExportsEvents(t *testing.T) {
	var mu sync.Mutex
	var paths, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	es, err := NewEventStream(EventStreamConfig{RingCapacity: 64})
	require.NoError(t, err)

	cfg := OTLPConfig{
		Endpoint:        strings.TrimPrefix(srv.URL, "http://"),
		URLPath:         "/otlp-relay/v1/logs",
		Insecure:        true,
		Headers:         map[string]string{"Authorization": "Bearer test-jwt"},
		ServiceName:     "kernel-browser",
		InstanceName:    "browser-1",
		Metro:           "dev-iad",
		MaxBatchRecords: 10,
	}
	wtr := NewOTLPStorageWriter(es, cfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, wtr.Start(ctx))

	es.Publish(Envelope{Event: Event{Ts: 1, Type: "page_navigation", Category: Page, Data: []byte(`{"url":"https://x"}`)}})
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
}
