package cdpmonitor

import (
	"maps"
	"sync"
)

const terminalHistorySize = 8192

type networkTerminalKind uint8

const (
	networkFinished networkTerminalKind = iota
	networkFailed
)

// NetworkSnapshot contains process-lifetime observed terminal outcomes, not
// socket counts. Up describes capture readiness, not browser responsiveness.
type NetworkSnapshot struct {
	Resets    uint64
	Completed uint64
	Up        bool
	// Failures is a detached snapshot keyed by bounded error code. Index 0 is
	// canceled=false; index 1 is canceled=true (the CDP flag, not inferred).
	Failures map[string][2]uint64
}

type networkCounters struct {
	mu                sync.Mutex
	resets, completed uint64
	failures          map[string][2]uint64
	seen              map[networkRequestKey]struct{}
	history           [terminalHistorySize]networkRequestKey
	next              int
}

func newNetworkCounters() *networkCounters {
	return &networkCounters{
		seen:     make(map[networkRequestKey]struct{}, terminalHistorySize),
		failures: maps.Clone(networkFailureZeros),
	}
}

func (c *networkCounters) terminal(sessionID, requestID string, kind networkTerminalKind, errorText string, canceled bool) {
	// CDP identities are short opaque strings. Bound retained bytes as well as entries.
	if sessionID == "" || requestID == "" || len(sessionID) > 256 || len(requestID) > 256 {
		return
	}
	key := networkRequestKey{sessionID, requestID}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.seen[key]; exists {
		return
	}
	delete(c.seen, c.history[c.next])
	c.history[c.next] = key
	c.next = (c.next + 1) % terminalHistorySize
	c.seen[key] = struct{}{}
	c.completed++
	if kind == networkFailed {
		code := networkErrorCode(errorText)
		counts := c.failures[code]
		index := 0
		if canceled {
			index = 1
		}
		counts[index]++
		c.failures[code] = counts
		if errorText == "net::ERR_CONNECTION_RESET" {
			c.resets++
		}
	}
}

// Connection replacement drains old events before clearing identities; this is
// the generation boundary. Totals intentionally survive it.
func (c *networkCounters) newGeneration() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.seen)
	clear(c.history[:])
	c.next = 0
}

func (c *networkCounters) snapshot() NetworkSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return NetworkSnapshot{Resets: c.resets, Completed: c.completed, Failures: maps.Clone(c.failures)}
}

func (m *Monitor) NetworkSnapshot() NetworkSnapshot {
	s := m.network.snapshot()
	m.lifeMu.Lock()
	conn := m.conn
	m.lifeMu.Unlock()
	if conn == nil || conn.ctx.Err() != nil || conn.protocol.IsClosed() || !conn.ready.Load() {
		return s
	}
	sessions, ready := conn.surface.CaptureSessions()
	if !ready {
		return s
	}
	m.sessionsMu.RLock()
	defer m.sessionsMu.RUnlock()
	for _, id := range sessions {
		if !m.networkReady[id] {
			return s
		}
	}
	s.Up = true
	return s
}
