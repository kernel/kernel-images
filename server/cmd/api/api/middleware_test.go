package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// recordingPublisher captures published events for assertion.
type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (rp *recordingPublisher) publish(ev events.Event) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.events = append(rp.events, ev)
}

func (rp *recordingPublisher) snapshot() []events.Event {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	out := make([]events.Event, len(rp.events))
	copy(out, rp.events)
	return out
}

// Mirrors the oapi-codegen strict dispatcher: middleware chain -> inner
// handler -> response write.
func fakeStrictHandler(operationID string, status int, mws []oapi.StrictMiddlewareFunc) http.Handler {
	inner := oapi.StrictHandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		return nil, nil
	})
	for _, mw := range mws {
		inner = mw(inner, operationID)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = inner(r.Context(), w, r, nil)
		w.WriteHeader(status)
	})
}

// Flips the package-level toggle on for the test, restoring prior state
// via t.Cleanup.
func withTelemetryMiddlewareEnabled(t *testing.T) {
	t.Helper()
	prev := TelemetryMiddlewareEnabled()
	EnableTelemetryMiddleware()
	t.Cleanup(func() {
		if prev {
			EnableTelemetryMiddleware()
		} else {
			DisableTelemetryMiddleware()
		}
	})
}

func TestTelemetryMiddleware_EmitsApiCallEventOnDocumentedRoute(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	chain := chiHandler(t, rp.publish, "ProcessExec", http.StatusOK)

	req := httptest.NewRequest(http.MethodPost, "/process/exec", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	captured := rp.snapshot()
	require.Len(t, captured, 1)
	ev := captured[0]
	assert.Equal(t, "api_call", ev.Type)
	assert.Equal(t, events.Api, ev.Category)
	assert.Equal(t, oapi.KernelApi, ev.Source.Kind)

	var data struct {
		RequestID   string  `json:"request_id"`
		OperationID string  `json:"operation_id"`
		Status      int     `json:"status"`
		DurationMs  float64 `json:"duration_ms"`
	}
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.NotEmpty(t, data.RequestID, "request_id should be set by chi RequestID middleware")
	assert.Equal(t, "ProcessExec", data.OperationID)
	assert.Equal(t, http.StatusOK, data.Status)
	assert.GreaterOrEqual(t, data.DurationMs, 0.0)
}

func TestTelemetryMiddleware_CapturesNonOKStatus(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	chain := chiHandler(t, rp.publish, "ProcessExec", http.StatusInternalServerError)

	req := httptest.NewRequest(http.MethodPost, "/process/exec", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	captured := rp.snapshot()
	require.Len(t, captured, 1)
	var data struct {
		Status int `json:"status"`
	}
	require.NoError(t, json.Unmarshal(captured[0].Data, &data))
	assert.Equal(t, http.StatusInternalServerError, data.Status)
}

func TestTelemetryMiddleware_SkipsUndocumentedRoutes(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	mw := TelemetryHTTPMiddleware(rp.publish)
	plain := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	chiMiddleware.RequestID(plain).ServeHTTP(httptest.NewRecorder(), req)

	assert.Empty(t, rp.snapshot(), "no event should be emitted when operationId is unset")
}

func TestTelemetryMiddleware_ShortCircuitsWhenDisabled(t *testing.T) {
	DisableTelemetryMiddleware()
	rp := &recordingPublisher{}
	chain := chiHandler(t, rp.publish, "ProcessExec", http.StatusOK)

	req := httptest.NewRequest(http.MethodPost, "/process/exec", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	assert.Empty(t, rp.snapshot(), "disabled middleware must not emit")
}

// Builds the same middleware stack as main.go: RequestID -> HTTP middleware ->
// strict dispatch -> inner handler.
func chiHandler(t *testing.T, publish func(events.Event), operationID string, status int) http.Handler {
	t.Helper()
	inner := fakeStrictHandler(operationID, status, []oapi.StrictMiddlewareFunc{TelemetryStrictMiddleware()})
	telemetry := TelemetryHTTPMiddleware(publish)(inner)
	return chiMiddleware.RequestID(telemetry)
}
