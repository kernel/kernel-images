package logger

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// AttrsKey groups the fields of a log line that are meant to be indexed by
// the log pipeline, as opposed to read. The host collector turns only these
// into searchable attributes, so keep them bounded: no URLs, paths, error
// text or other free-form values.
const AttrsKey = "attrs"

// RequestLogger logs one line per request once it completes, at INFO, or WARN
// for a 4xx and ERROR for a 5xx. A request whose handler panics past it, as
// with http.ErrAbortHandler, is logged at ERROR as aborted. It replaces chi's
// middleware.Logger, which writes free-form text.
//
// Only the matched route pattern is logged, never the URL, host or query:
// those can carry credentials (e.g. a URL passed as a query parameter).
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := chiMiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			completed := false
			defer func() {
				status := ww.Status()
				if status == 0 && completed {
					status = http.StatusOK
				}
				level := slog.LevelInfo
				switch {
				case !completed || status >= 500:
					level = slog.LevelError
				case status >= 400:
					level = slog.LevelWarn
				}
				elapsed := time.Since(start)
				args := []any{"bytes", ww.BytesWritten()}
				if id := chiMiddleware.GetReqID(r.Context()); id != "" {
					args = append(args, "request_id", id)
				}
				attrs := []any{
					"http.request.method", r.Method,
					"duration_ms", elapsed.Milliseconds(),
				}
				if status != 0 {
					attrs = append(attrs, "http.response.status_code", status)
				}
				if !completed {
					attrs = append(attrs, "error.type", "aborted")
				}
				if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
					attrs = append(attrs, "http.route", rctx.RoutePattern())
				}
				args = append(args, slog.Group(AttrsKey, attrs...))
				log.Log(r.Context(), level, "request", args...)
			}()
			next.ServeHTTP(ww, r)
			completed = true
		})
	}
}

// Recoverer recovers a panicking handler, logs the panic and its stack as one
// ERROR line, and answers 500 unless the connection was being upgraded. It
// replaces chi's middleware.Recoverer, which prints the stack as many lines of
// text. http.ErrAbortHandler is re-panicked so net/http aborts the response.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rvr := recover()
				if rvr == nil {
					return
				}
				if rvr == http.ErrAbortHandler {
					panic(rvr)
				}
				// The panic value is free-form, so only its Go type is indexed.
				log.ErrorContext(r.Context(), "panic", "panic", fmt.Sprint(rvr), "stack", string(debug.Stack()),
					slog.Group(AttrsKey, "exception.type", fmt.Sprintf("%T", rvr)))
				if r.Header.Get("Connection") != "Upgrade" {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
