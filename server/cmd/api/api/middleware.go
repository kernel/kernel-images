package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// Per-request scratch shared between the chi-level HTTP middleware and the
// strict-server middleware so the latter can stamp the matched operationId.
type telemetryCtxKey struct{}

type telemetryRequestCtx struct {
	operationID string
}

// Process-wide toggle for the api_call middleware. Flipped by
// Enable/DisableTelemetryMiddleware; both middleware layers short-circuit
// to passthroughs when false.
var telemetryMiddlewareEnabled atomic.Bool

// EnableTelemetryMiddleware turns on api_call event emission.
func EnableTelemetryMiddleware() { telemetryMiddlewareEnabled.Store(true) }

// DisableTelemetryMiddleware turns api_call event emission off.
func DisableTelemetryMiddleware() { telemetryMiddlewareEnabled.Store(false) }

// TelemetryMiddlewareEnabled reports the current state.
func TelemetryMiddlewareEnabled() bool { return telemetryMiddlewareEnabled.Load() }

// TelemetryHTTPMiddleware emits a BrowserApiCallEvent per documented operation,
// capturing the final status and wall-clock duration.
func TelemetryHTTPMiddleware(publish func(events.Event)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !telemetryMiddlewareEnabled.Load() {
				next.ServeHTTP(w, r)
				return
			}
			tc := &telemetryRequestCtx{}
			ctx := context.WithValue(r.Context(), telemetryCtxKey{}, tc)
			ww := chiMiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r.WithContext(ctx))

			if tc.operationID == "" {
				return
			}
			data, _ := json.Marshal(oapi.BrowserApiCallEventData{
				RequestId:   chiMiddleware.GetReqID(ctx),
				OperationId: tc.operationID,
				Status:      ww.Status(),
				DurationMs:  float32(time.Since(start).Microseconds()) / 1000.0,
			})
			publish(events.Event{
				Ts:       time.Now().UnixMicro(),
				Type:     "api_call",
				Category: events.Api,
				Source:   oapi.BrowserEventSource{Kind: oapi.KernelApi},
				Data:     data,
			})
		})
	}
}

// TelemetryStrictMiddleware records the matched OpenAPI operationId onto the
// per-request scratch so TelemetryHTTPMiddleware can include it in the event.
func TelemetryStrictMiddleware() oapi.StrictMiddlewareFunc {
	return func(next oapi.StrictHandlerFunc, operationID string) oapi.StrictHandlerFunc {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			if !telemetryMiddlewareEnabled.Load() {
				return next(ctx, w, r, request)
			}
			if tc, ok := ctx.Value(telemetryCtxKey{}).(*telemetryRequestCtx); ok {
				tc.operationID = operationID
			}
			return next(ctx, w, r, request)
		}
	}
}
