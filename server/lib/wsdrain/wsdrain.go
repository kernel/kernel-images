// Package wsdrain tracks live WebSocket connections so they can be closed with
// a single status code when the server shuts down.
package wsdrain

import (
	"sync"

	"github.com/coder/websocket"
)

// Conn is the subset of *websocket.Conn the registry needs.
type Conn interface {
	Close(code websocket.StatusCode, reason string) error
}

// Registry tracks active connections. Construct one with New. All methods are
// safe for concurrent use and tolerate a nil receiver, so callers may pass a
// nil *Registry to disable tracking.
type Registry struct {
	mu    sync.Mutex
	conns map[Conn]struct{}
}

func New() *Registry {
	return &Registry{conns: make(map[Conn]struct{})}
}

// Track registers conn and returns a function that removes it. The returned
// function is idempotent; call it (e.g. via defer) when the connection ends.
func (r *Registry) Track(conn Conn) func() {
	if r == nil || conn == nil {
		return func() {}
	}
	r.mu.Lock()
	r.conns[conn] = struct{}{}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.conns, conn)
			r.mu.Unlock()
		})
	}
}

// CloseAll closes every tracked connection with the given code and reason and
// returns how many it closed. Connections are snapshotted under the lock and
// closed outside it. Close errors are ignored: the connection is being
// discarded regardless, and the first Close wins, so a later normal-closure
// from the connection's own teardown does not override the code sent here.
func (r *Registry) CloseAll(code websocket.StatusCode, reason string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	conns := make([]Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()

	for _, c := range conns {
		_ = c.Close(code, reason)
	}
	return len(conns)
}
