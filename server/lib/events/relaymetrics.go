package events

import (
	"maps"
	"sync"
	"sync/atomic"
)

// Reasons a relay record is dropped.
const (
	relayDropQueueFull        = "queue_full"
	relayDropRejected         = "rejected"
	relayDropRetriesExhausted = "retries_exhausted"
	relayDropShutdown         = "shutdown"
)

// RelayMetrics accumulates the storage relay sink's process-lifetime counters.
// Safe for concurrent use.
type RelayMetrics struct {
	exported   atomic.Uint64
	retries    atomic.Uint64
	queueDepth atomic.Int64

	mu      sync.Mutex
	dropped map[string]uint64
	failed  map[string]uint64
}

// Exported returns records the relay acknowledged.
func (m *RelayMetrics) Exported() uint64 { return m.exported.Load() }

// Retries returns batch retries after a retryable response.
func (m *RelayMetrics) Retries() uint64 { return m.retries.Load() }

// QueueDepth returns records waiting to be sent.
func (m *RelayMetrics) QueueDepth() int64 { return m.queueDepth.Load() }

// Dropped returns dropped records by reason.
func (m *RelayMetrics) Dropped() map[string]uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.dropped)
}

// Failed returns batches given up on, by HTTP status or "error".
func (m *RelayMetrics) Failed() map[string]uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.failed)
}

func (m *RelayMetrics) addDropped(reason string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dropped == nil {
		m.dropped = map[string]uint64{}
	}
	m.dropped[reason] += uint64(n)
}

func (m *RelayMetrics) addFailed(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed == nil {
		m.failed = map[string]uint64{}
	}
	m.failed[status]++
}
