package livefocus

import (
	"fmt"
	"net/http"
	"time"
)

// A ping keeps proxies from closing a quiet stream, and lets the client tell
// a live stream from a stalled one.
const pingInterval = 15 * time.Second

// ServeHTTP streams states as server-sent events: the current state on
// connect, then each change.
func (w *Watcher) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(rw)
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(http.StatusOK)

	states, unsubscribe := w.Subscribe()
	defer unsubscribe()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-w.closed:
			return
		case state := <-states:
			_, err = fmt.Fprintf(rw, "data: %s\n\n", state)
		case <-ping.C:
			_, err = fmt.Fprint(rw, "event: ping\ndata: {}\n\n")
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}
