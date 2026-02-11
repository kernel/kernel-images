package scaletozero

import (
	"context"
	"net"
	"net/http"

	"github.com/onkernel/kernel-images/server/lib/logger"
)

// Middleware returns a standard net/http middleware that disables scale-to-zero
// at the start of each request and re-enables it after the handler completes.
// Connections from loopback addresses are ignored and do not affect the
// scale-to-zero state.
func Middleware(ctrl Controller) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isLoopbackAddr(r.RemoteAddr) {
				next.ServeHTTP(w, r)
				return
			}

			if err := ctrl.Disable(r.Context()); err != nil {
				logger.FromContext(r.Context()).Error("failed to disable scale-to-zero", "error", err)
				http.Error(w, "failed to disable scale-to-zero", http.StatusInternalServerError)
				return
			}
			defer ctrl.Enable(context.WithoutCancel(r.Context()))

			next.ServeHTTP(w, r)
		})
	}
}

// isLoopbackAddr reports whether addr is a loopback address.
// addr may be an "ip:port" pair or a bare IP.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
