// Package sysmon emits VM-internal failure telemetry — OOM kills surfaced
// through /dev/kmsg, and (via the supervisord-shim binary POSTing to the
// telemetry HTTP endpoint) supervised-service crashes.
//
// The package only owns the in-process kmsg reader; service crashes are
// delivered as ordinary caller-published events via POST /telemetry/events
// from the shim. Both paths terminate in the same EventStream.
package sysmon

import (
	"context"
	"log/slog"

	"github.com/kernel/kernel-images/server/lib/events"
)

// DefaultKmsgPath is the standard kernel log device.
const DefaultKmsgPath = "/dev/kmsg"

// Monitor runs the in-process sysmon goroutines and publishes events directly
// to the EventStream. System-category events are always captured regardless
// of any active TelemetrySession config, so we deliberately bypass
// TelemetrySession here.
type Monitor struct {
	es        *events.EventStream
	logger    *slog.Logger
	kmsgPath  string
}

// Option configures a Monitor.
type Option func(*Monitor)

// WithKmsgPath overrides the default /dev/kmsg path. Intended for tests.
func WithKmsgPath(path string) Option {
	return func(m *Monitor) { m.kmsgPath = path }
}

// New constructs a Monitor. The Monitor does nothing until Start is called.
func New(es *events.EventStream, logger *slog.Logger, opts ...Option) *Monitor {
	m := &Monitor{
		es:       es,
		logger:   logger,
		kmsgPath: DefaultKmsgPath,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start launches background goroutines. It returns immediately; goroutines
// shut down when ctx is cancelled.
func (m *Monitor) Start(ctx context.Context) {
	go m.runKmsg(ctx)
}
