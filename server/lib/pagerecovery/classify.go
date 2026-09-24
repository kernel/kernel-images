package pagerecovery

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// proxyErrorHeader brands a 502 produced by Kernel's own egress rather than by
// the site. cdpmonitor already reports those as typed proxy_error events, so
// retrying one would spend a navigation's latency budget hiding a Kernel-side
// signal instead of working around a site-side one.
const proxyErrorHeader = "x-kernel-proxy-error"

// retryableErrorReasons are the Fetch network error reasons that describe a
// connection that died rather than a site that answered. ERR_CONNECTION_RESET
// is the one the browser VMs see most (it has its own always-on counter in
// cdpmonitor); the rest are its neighbours in the same failure mode.
var retryableErrorReasons = map[string]struct{}{
	"ConnectionAborted": {},
	"ConnectionClosed":  {},
	"ConnectionFailed":  {},
	"ConnectionReset":   {},
	"TimedOut":          {},
}

// retryableStatuses are the responses a site sends when it is refusing to serve
// this request *now*. 403 is deliberately absent: it is as often a settled
// verdict on the session as a throttle, and replaying it looks like hammering
// while changing nothing. Recovering from a 403 needs evidence about which of
// the two it is, which is the anti-bot extension's reading, not a status code.
var retryableStatuses = map[int]struct{}{
	http.StatusRequestTimeout:      {}, // 408
	http.StatusTooManyRequests:     {}, // 429
	http.StatusBadGateway:          {}, // 502
	http.StatusServiceUnavailable:  {}, // 503
	http.StatusGatewayTimeout:      {}, // 504
	http.StatusInsufficientStorage: {}, // 507, seen from overloaded edges
}

// pausedResponse is the part of a Fetch.requestPaused notification the decision
// reads. Keeping it separate from the CDP payload keeps the rules testable
// without a browser.
type pausedResponse struct {
	method       string
	status       int
	errorReason  string
	headers      map[string]string
	isTopLevel   bool
	isNavigation bool
}

// retryAfter reports the delay the response asked for, if it named one.
// Both RFC 9110 forms are accepted; anything else is treated as absent rather
// than as zero, so a malformed header does not turn into an immediate replay.
func (r pausedResponse) retryAfter(now time.Time) (time.Duration, bool) {
	raw := strings.TrimSpace(r.headers["retry-after"])
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	if d := when.Sub(now); d > 0 {
		return d, true
	}
	return 0, true
}

// retryable reports whether replaying this request could plausibly produce a
// different answer.
//
// Only top-level navigations qualify. A subresource that 429s is the page's
// problem to retry — the page is already loaded and its own code knows what the
// resource was for — and a document load inside an iframe is not what the
// caller is waiting on.
//
// Only GET qualifies. The replay is a 307, which preserves method and body, and
// a POST that a gateway refused may still have been recorded upstream; a
// duplicate order is a worse outcome than a visible 429.
func (r pausedResponse) retryable() bool {
	if !r.isTopLevel || !r.isNavigation {
		return false
	}
	if r.method != http.MethodGet {
		return false
	}
	if r.errorReason != "" {
		_, ok := retryableErrorReasons[r.errorReason]
		return ok
	}
	if _, branded := r.headers[proxyErrorHeader]; branded {
		return false
	}
	_, ok := retryableStatuses[r.status]
	return ok
}
