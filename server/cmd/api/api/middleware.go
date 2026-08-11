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
	code        string
}

// RecordTelemetryCode attaches code submitted with the request to its api_call
// event, capped like every other captured string. It is a no-op when telemetry
// is off or the request is not one the middleware tracks.
func RecordTelemetryCode(ctx context.Context, code string) {
	tc, ok := ctx.Value(telemetryCtxKey{}).(*telemetryRequestCtx)
	if !ok {
		return
	}
	tc.code = events.TruncateCaptured(code, events.CapturedFieldCap)
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

// TelemetryHTTPMiddleware emits one event per documented operation, capturing
// the final status and wall-clock duration. Operations that drive the browser
// emit api_call under control; operations that manage the VM emit
// platform_api_call under platform, per the operation's x-telemetry-category in
// openapi.yaml. publish is wired to TelemetrySession.Publish; the middleware
// ignores the returns.
func TelemetryHTTPMiddleware(publish func(events.Event) (events.Envelope, bool)) func(http.Handler) http.Handler {
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
			eventType, category := apiCallEvent(tc.operationID)
			data := apiCallEventData(category, tc, chiMiddleware.GetReqID(ctx), ww.Status(), time.Since(start))
			publish(events.Event{
				Ts:       time.Now().UnixMicro(),
				Type:     eventType,
				Category: category,
				Source:   oapi.BrowserEventSource{Kind: oapi.KernelApi},
				Data:     data,
			})
		})
	}
}

// apiCallEventData marshals the payload for the resolved event type. The two
// types have separate schemas and only api_call declares code, so a platform
// call marshals the metadata-only type rather than a control payload that
// happens to have the field unset.
func apiCallEventData(category oapi.TelemetryEventCategory, tc *telemetryRequestCtx, requestID string, status int, duration time.Duration) []byte {
	durationMs := float32(duration.Microseconds()) / 1000.0
	if category != events.Control {
		data, _ := json.Marshal(oapi.BrowserPlatformApiCallEventData{
			RequestId:   requestID,
			OperationId: tc.operationID,
			Status:      status,
			DurationMs:  durationMs,
		})
		return data
	}
	eventData := oapi.BrowserApiCallEventData{
		RequestId:   requestID,
		OperationId: tc.operationID,
		Status:      status,
		DurationMs:  durationMs,
	}
	if tc.code != "" {
		eventData.Code = &tc.code
	}
	data, _ := json.Marshal(eventData)
	return data
}

// apiCallEvent resolves the event type and category for an operation. An
// operation missing from the generated map falls back to platform so an
// unclassified route cannot dilute the control stream.
func apiCallEvent(operationID string) (string, oapi.TelemetryEventCategory) {
	if cat, ok := events.CategoryForOperation(operationID); ok && cat == events.Control {
		return "api_call", events.Control
	}
	return "platform_api_call", events.Platform
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
