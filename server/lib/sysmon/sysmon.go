// Package sysmon emits VM-internal failure telemetry — OOM kills
// surfaced through /dev/kmsg, and (via the supervisord-shim binary
// POSTing to the telemetry HTTP endpoint) supervised-service crashes.
//
// The package only owns the in-process kmsg reader; service crashes are
// delivered as ordinary caller-published events via POST
// /telemetry/events from the shim.
package sysmon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// kmsgSource abstracts a /dev/kmsg-shaped stream of kernel ring buffer
// messages. The production implementation lives in kmsg_linux.go; tests
// supply a stub via Monitor.kmsgSource.
type kmsgSource interface {
	Messages() <-chan KmsgMessage
	Close() error
}

// PublishFunc receives events emitted by the Monitor. Production callers
// wire this to TelemetrySession.Publish so events are gated by the active
// telemetry config; the Monitor itself ignores the returns.
type PublishFunc func(events.Event) (events.Envelope, bool)

// Monitor runs the in-process sysmon goroutine and hands each event off to
// the configured publish func.
type Monitor struct {
	publish PublishFunc
	logger  *slog.Logger

	// kmsgSource lets tests inject a stub stream of kmsg messages.
	// Production callers leave this nil; Start() then opens /dev/kmsg.
	kmsgSource kmsgSource

	wg sync.WaitGroup
}

type option func(*Monitor)

// withKmsgSource overrides the kmsg source. Test-only.
func withKmsgSource(src kmsgSource) option {
	return func(m *Monitor) { m.kmsgSource = src }
}

// New constructs a Monitor. The Monitor does nothing until Start is
// called.
func New(publish PublishFunc, logger *slog.Logger, opts ...option) *Monitor {
	m := &Monitor{publish: publish, logger: logger}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start opens the kmsg source (validating that it is usable) and
// launches the background OOM reader goroutine. It returns an error if
// the kmsg source cannot be opened; the goroutine then never starts and
// the caller can decide whether the failure is fatal.
//
// Start must be called at most once per Monitor. Calling it twice would
// spawn two readers racing on the same kmsg channel and corrupt the OOM
// state machine. Callers needing a restart should construct a new
// Monitor.
//
// The goroutine shuts down when ctx is cancelled; Wait blocks until it
// returns.
func (m *Monitor) Start(ctx context.Context) error {
	if m.kmsgSource == nil {
		src, err := openKmsgSource(m.logger)
		if err != nil {
			return err
		}
		m.kmsgSource = src
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runOomLoop(ctx)
	}()
	return nil
}

// Wait blocks until all goroutines launched by Start have returned.
func (m *Monitor) Wait() { m.wg.Wait() }

// runOomLoop consumes the kmsg stream, drives the OOM state machine, and
// publishes a system_oom_kill event for each completed instance.
func (m *Monitor) runOomLoop(ctx context.Context) {
	src := m.kmsgSource
	// Closing the source unblocks any read in Messages() so the range
	// terminates cleanly on shutdown. The done channel lets the
	// watcher exit if the source closes on its own (e.g. /dev/kmsg fd
	// dropped) so we don't leak the goroutine past loop exit.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = src.Close()
		case <-done:
		}
	}()

	m.logger.Debug("sysmon: kmsg OOM reader started")

	var s oomScanner
	for msg := range src.Messages() {
		oom := s.feed(msg.Body, msg.Timestamp)
		if oom == nil {
			continue
		}
		m.publishOomKill(*oom)
	}

	m.logger.Debug("sysmon: kmsg OOM reader stopped")
}

func (m *Monitor) publishOomKill(oom OomInstance) {
	data := oapi.BrowserSystemOomKillEventData{
		ProcessName: oom.ProcessName,
		Pid:         oom.Pid,
		RssKb:       oom.RssKb,
	}
	if oom.Constraint != "" {
		c := oapi.BrowserSystemOomKillEventDataConstraint(oom.Constraint)
		// Drop unknown constraint values from the payload rather than
		// emitting a non-enum string that SDKs may reject. The raw
		// kernel label still reaches structured logs below.
		if c.Valid() {
			data.Constraint = &c
		} else {
			m.logger.Warn("sysmon: unknown OOM constraint, omitting from payload", "constraint", oom.Constraint)
		}
	}
	if oom.MemTotalKb > 0 {
		v := oom.MemTotalKb
		data.MemTotalKb = &v
	}
	if oom.MemFreeKb > 0 {
		v := oom.MemFreeKb
		data.MemFreeKb = &v
	}
	if len(oom.TopTasks) > 0 {
		tasks := make([]oapi.BrowserSystemOomKillTask, len(oom.TopTasks))
		for i, t := range oom.TopTasks {
			tasks[i] = oapi.BrowserSystemOomKillTask{
				Pid:   t.Pid,
				Name:  t.Name,
				RssKb: t.RssKb,
			}
		}
		data.TopTasks = &tasks
	}
	if oom.TriggerProcessName != "" {
		v := oom.TriggerProcessName
		data.TriggerProcessName = &v
	}
	if oom.TriggerPid > 0 {
		v := oom.TriggerPid
		data.TriggerPid = &v
	}

	payload, err := json.Marshal(data)
	if err != nil {
		m.logger.Warn("sysmon: marshal oom kill payload", "err", err)
		return
	}
	srcEvent := "linux.oom_kill"
	ev := events.Event{
		Ts:       oom.TimeOfDeath.UnixMicro(),
		Type:     string(oapi.SystemOomKill),
		Category: events.System,
		Source: oapi.BrowserEventSource{
			Kind:  oapi.LocalProcess,
			Event: &srcEvent,
		},
		Data: json.RawMessage(payload),
	}
	m.publish(ev)
	m.logger.Debug("sysmon: emitted system_oom_kill",
		"process", oom.ProcessName,
		"pid", oom.Pid,
	)
}
