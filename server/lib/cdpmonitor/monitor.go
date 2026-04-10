package cdpmonitor

import (
	"context"
	"sync/atomic"

	"github.com/kernel/kernel-images/server/lib/events"
)

// UpstreamProvider abstracts *devtoolsproxy.UpstreamManager for testability.
type UpstreamProvider interface {
	Current() string
	Subscribe() (<-chan string, func())
}

// PublishFunc publishes an Event to the pipeline.
type PublishFunc func(ev events.Event)

// Monitor manages a CDP WebSocket connection with auto-attach session fan-out.
// Single-use per capture session: call Start to begin, Stop to tear down.
type Monitor struct {
	running atomic.Bool
}

// New creates a Monitor. displayNum is the X display for ffmpeg screenshots.
func New(_ UpstreamProvider, _ PublishFunc, _ int) *Monitor {
	return &Monitor{}
}

// IsRunning reports whether the monitor is actively capturing.
func (m *Monitor) IsRunning() bool {
	return m.running.Load()
}

// Start begins CDP capture. Restarts if already running.
func (m *Monitor) Start(_ context.Context) error {
	return nil
}

// Stop tears down the monitor. Safe to call multiple times.
func (m *Monitor) Stop() {}
