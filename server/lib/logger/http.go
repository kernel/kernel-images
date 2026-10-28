package logger

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// AttrsKey groups the fields of a log line that are meant to be indexed by
// the log pipeline, as opposed to read. The host collector turns only these
// into searchable attributes, so keep them bounded: no URLs, paths, error
// text or other free-form values.
const AttrsKey = "attrs"

// maxPanicMessage bounds the indexed copy of a panic's value. The full value
// and stack are logged alongside it.
const maxPanicMessage = 512

// RequestLogger logs one line per request once it completes, at INFO, or WARN
// for a 4xx and ERROR for a 5xx. It replaces chi's middleware.Logger, which
// writes free-form text.
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := chiMiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			defer func() {
				status := ww.Status()
				if status == 0 {
					status = http.StatusOK
				}
				level := slog.LevelInfo
				switch {
				case status >= 500:
					level = slog.LevelError
				case status >= 400:
					level = slog.LevelWarn
				}
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				elapsed := time.Since(start)
				args := []any{
					"method", r.Method,
					"url", scheme + "://" + r.Host + r.RequestURI,
					"proto", r.Proto,
					"remote_addr", r.RemoteAddr,
					"status", status,
					"bytes", ww.BytesWritten(),
					"duration", elapsed.String(),
				}
				if id := chiMiddleware.GetReqID(r.Context()); id != "" {
					args = append(args, "request_id", id)
				}
				attrs := []any{
					"http.request.method", r.Method,
					"http.response.status_code", status,
					"duration_ms", elapsed.Milliseconds(),
				}
				if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
					attrs = append(attrs, "http.route", rctx.RoutePattern())
				}
				args = append(args, slog.Group(AttrsKey, attrs...))
				log.Log(r.Context(), level, "request", args...)
			}()
			next.ServeHTTP(ww, r)
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
				msg := fmt.Sprint(rvr)
				indexed := msg
				if len(indexed) > maxPanicMessage {
					indexed = strings.ToValidUTF8(indexed[:maxPanicMessage], "")
				}
				log.ErrorContext(r.Context(), "panic", "panic", msg, "stack", string(debug.Stack()),
					slog.Group(AttrsKey, "exception.type", "panic", "exception.message", indexed))
				if r.Header.Get("Connection") != "Upgrade" {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
