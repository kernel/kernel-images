package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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

func (rp *recordingPublisher) publish(ev events.Event) (events.Envelope, bool) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.events = append(rp.events, ev)
	return events.Envelope{Event: ev}, true
}

func (rp *recordingPublisher) snapshot() []events.Event {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	out := make([]events.Event, len(rp.events))
	copy(out, rp.events)
	return out
}

// Mirrors the oapi-codegen strict dispatcher, running body inside the handler so
// a test can act on the request context the way a real handler does.
func fakeStrictHandlerFunc(operationID string, status int, body func(ctx context.Context)) http.Handler {
	inner := oapi.StrictHandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		body(ctx)
		return nil, nil
	})
	inner = TelemetryStrictMiddleware()(inner, operationID)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = inner(r.Context(), w, r, nil)
		w.WriteHeader(status)
	})
}

// noBody is the handler body for tests that only exercise the middleware.
func noBody(context.Context) {}

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

func TestWebMCPRequestSizeMiddlewareRejectsOversizedInvokeBody(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", maxWebMCPRequestBytes+1))
	request := httptest.NewRequest(http.MethodPost, "/webmcp/invoke", body)
	recorder := httptest.NewRecorder()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		var tooLarge *http.MaxBytesError
		require.ErrorAs(t, err, &tooLarge)
		w.WriteHeader(http.StatusBadRequest)
	})

	WebMCPRequestSizeMiddleware(next).ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestWebMCPRequestSizeMiddlewareDoesNotLimitOtherRoutes(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", maxWebMCPRequestBytes+1))
	request := httptest.NewRequest(http.MethodPost, "/playwright/execute", body)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Len(t, payload, maxWebMCPRequestBytes+1)
	})

	WebMCPRequestSizeMiddleware(next).ServeHTTP(httptest.NewRecorder(), request)
}

func TestStrictErrorHandlersReturnJSON(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, error)
		status  int
	}{
		{name: "request", handler: StrictRequestErrorHandler, status: http.StatusBadRequest},
		{name: "response", handler: StrictResponseErrorHandler, status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(recorder, httptest.NewRequest(http.MethodPost, "/webmcp/invoke", nil), errors.New("invalid request"))
			require.Equal(t, test.status, recorder.Code)
			require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
			require.JSONEq(t, `{"message":"invalid request"}`, recorder.Body.String())
		})
	}
}

func TestStrictErrorHandlersPreservePlaintextOutsideWebMCP(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, error)
		status  int
	}{
		{name: "request", handler: StrictRequestErrorHandler, status: http.StatusBadRequest},
		{name: "response", handler: StrictResponseErrorHandler, status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(recorder, httptest.NewRequest(http.MethodPost, "/playwright/execute", nil), errors.New("invalid request"))
			require.Equal(t, test.status, recorder.Code)
			require.Equal(t, "text/plain; charset=utf-8", recorder.Header().Get("Content-Type"))
			require.Equal(t, "invalid request\n", recorder.Body.String())
		})
	}
}

func TestTelemetryMiddleware_EmitsApiCallEventOnDocumentedRoute(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	chain := chiHandler(t, rp.publish, "ClickMouse", http.StatusOK, noBody)

	req := httptest.NewRequest(http.MethodPost, "/computer/click_mouse", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	captured := rp.snapshot()
	require.Len(t, captured, 1)
	ev := captured[0]
	assert.Equal(t, "api_call", ev.Type)
	assert.Equal(t, events.Control, ev.Category)
	assert.Equal(t, oapi.KernelApi, ev.Source.Kind)

	var data struct {
		RequestID   string  `json:"request_id"`
		OperationID string  `json:"operation_id"`
		Status      int     `json:"status"`
		DurationMs  float64 `json:"duration_ms"`
		Code        *string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.NotEmpty(t, data.RequestID, "request_id should be set by chi RequestID middleware")
	assert.Equal(t, "ClickMouse", data.OperationID)
	assert.Equal(t, http.StatusOK, data.Status)
	assert.GreaterOrEqual(t, data.DurationMs, 0.0)
	assert.Nil(t, data.Code, "code is only recorded by handlers that submit code")
}

func TestTelemetryMiddleware_EmitsPlatformApiCallForVMOperations(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	// Records code from a platform handler, which no handler does today, to pin
	// that the payload type and not the call sites is what keeps it out.
	chain := chiHandler(t, rp.publish, "ProcessExec", http.StatusOK, func(ctx context.Context) {
		RecordTelemetryCode(ctx, "await page.goto('https://example.com')")
	})

	req := httptest.NewRequest(http.MethodPost, "/process/exec", nil)
	chain.ServeHTTP(httptest.NewRecorder(), req)

	captured := rp.snapshot()
	require.Len(t, captured, 1)
	assert.Equal(t, "platform_api_call", captured[0].Type)
	assert.Equal(t, events.Platform, captured[0].Category)
	// BrowserPlatformApiCallEventData declares no code field, so the key must be
	// absent for the payload to match the published schema.
	assert.NotContains(t, string(captured[0].Data), `"code"`)
}

// An operation the generated map does not know about must not land in control,
// which is the stream callers read to see what the agent did.
func TestTelemetryMiddleware_UnknownOperationIsPlatform(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	chain := chiHandler(t, rp.publish, "SomeRouteAddedLater", http.StatusOK, noBody)

	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/whatever", nil))

	captured := rp.snapshot()
	require.Len(t, captured, 1)
	assert.Equal(t, events.Platform, captured[0].Category)
}

func TestTelemetryMiddleware_RecordsSubmittedCode(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	code := "await page.goto('https://example.com')"
	chain := chiHandler(t, rp.publish, "ExecutePlaywrightCode", http.StatusOK, func(ctx context.Context) {
		RecordTelemetryCode(ctx, code)
	})

	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/playwright/execute", nil))

	captured := rp.snapshot()
	require.Len(t, captured, 1)
	assert.Equal(t, events.Control, captured[0].Category)
	assert.False(t, captured[0].Truncated)
	var data struct {
		Code *string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(captured[0].Data, &data))
	require.NotNil(t, data.Code)
	assert.Equal(t, code, *data.Code)
}

func TestTelemetryMiddleware_ClipsOversizedCode(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	// Ends on a multi-byte rune straddling the cap so the clip has to back off.
	oversized := strings.Repeat("x", events.CapturedFieldCap-1) + strings.Repeat("é", 10)
	chain := chiHandler(t, rp.publish, "ExecutePlaywrightCode", http.StatusOK, func(ctx context.Context) {
		RecordTelemetryCode(ctx, oversized)
	})

	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/playwright/execute", nil))

	captured := rp.snapshot()
	require.Len(t, captured, 1)
	var data struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(captured[0].Data, &data))
	assert.True(t, strings.HasSuffix(data.Code, events.TruncatedSuffix), "clipped code is not marked: %q", data.Code[max(0, len(data.Code)-40):])
	assert.LessOrEqual(t, len(data.Code), events.CapturedFieldCap)
	assert.True(t, utf8.ValidString(data.Code))
}

// RecordTelemetryCode is called from handlers that also serve requests the
// middleware is not tracking, so it must tolerate a bare context.
func TestRecordTelemetryCode_NoopWithoutRequestScratch(t *testing.T) {
	RecordTelemetryCode(context.Background(), "await page.goto('https://example.com')")
}

func TestTelemetryMiddleware_CapturesNonOKStatus(t *testing.T) {
	withTelemetryMiddlewareEnabled(t)
	rp := &recordingPublisher{}
	chain := chiHandler(t, rp.publish, "ProcessExec", http.StatusInternalServerError, noBody)

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
	chain := chiHandler(t, rp.publish, "ProcessExec", http.StatusOK, noBody)

	req := httptest.NewRequest(http.MethodPost, "/process/exec", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	assert.Empty(t, rp.snapshot(), "disabled middleware must not emit")
}

// Builds the same middleware stack as main.go: RequestID -> HTTP middleware ->
// strict dispatch -> inner handler.
func chiHandler(t *testing.T, publish func(events.Event) (events.Envelope, bool), operationID string, status int, body func(ctx context.Context)) http.Handler {
	t.Helper()
	inner := fakeStrictHandlerFunc(operationID, status, body)
	telemetry := TelemetryHTTPMiddleware(publish)(inner)
	return chiMiddleware.RequestID(telemetry)
}
