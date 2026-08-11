package devtoolsproxy

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// ControlEnabledFunc reports whether control-category telemetry is currently
// captured. The proxy calls it once per forwarded client frame, so it must be
// cheap: telemetry.TelemetrySession.CategoryEnabled is lock-free for this.
type ControlEnabledFunc func() bool

// ExcludedMethodsFunc returns the browser-control methods configured out of the
// cdp_command stream, or nil when none are. Consulted on the worker once the
// method is known, so an exclusion never costs the pump anything.
type ExcludedMethodsFunc func() map[string]struct{}

const (
	// cdpObserverQueueDepth bounds how many forwarded frames may be waiting for
	// classification. Deep enough to absorb a burst of input gestures, shallow
	// enough that a stalled publisher cannot accumulate unbounded garbage.
	cdpObserverQueueDepth = 256
	// cdpObserverMaxFrameBytes caps the frame the worker will parse. Every
	// browser-control command is small — coordinates, enums and short strings —
	// so anything past this is a paste or a data: URL whose event would cost
	// megabytes of parsing to report a length. Those are dropped and counted.
	cdpObserverMaxFrameBytes = 64 * 1024
	// cdpObserverDrainWait bounds how long connection teardown waits for the
	// worker to finish the queue.
	cdpObserverDrainWait = time.Second
)

// cdpObserver turns forwarded client frames into cdp_command events on its own
// goroutine. Observe runs on the pump, so it does only what is needed to decide
// the frame is not worth queuing; classification, sanitation and publication
// all happen on the worker, where they cannot delay CDP or kill the process.
type cdpObserver struct {
	frames          chan observedFrame
	drained         chan struct{}
	publish         EventPublisher
	controlEnabled  ControlEnabledFunc
	excludedMethods ExcludedMethodsFunc
	logger          *slog.Logger

	droppedQueued    atomic.Int64
	droppedOversized atomic.Int64
	droppedPanicked  atomic.Int64
}

// observedFrame is a client frame that reached Chromium, with the time the
// forward completed. The timestamp travels with the frame so queue latency
// does not show up as event time.
type observedFrame struct {
	msg []byte
	ts  int64
}

// newCdpObserver starts the classification worker. It stops when ctx is done.
// A nil publish or controlEnabled disables observation entirely.
func newCdpObserver(ctx context.Context, publish EventPublisher, controlEnabled ControlEnabledFunc, excludedMethods ExcludedMethodsFunc, logger *slog.Logger) *cdpObserver {
	if publish == nil || controlEnabled == nil {
		return nil
	}
	if excludedMethods == nil {
		excludedMethods = func() map[string]struct{} { return nil }
	}
	o := &cdpObserver{
		frames:          make(chan observedFrame, cdpObserverQueueDepth),
		drained:         make(chan struct{}),
		publish:         publish,
		controlEnabled:  controlEnabled,
		excludedMethods: excludedMethods,
		logger:          logger,
	}
	go o.run(ctx)
	return o
}

// Observe queues a forwarded client frame. It never blocks and never parses:
// a frame arrives here only after Chromium has already accepted it, and the
// pump is waiting on the return.
func (o *cdpObserver) Observe(msg []byte, ts int64) {
	if o == nil || !o.controlEnabled() {
		return
	}
	if len(msg) > cdpObserverMaxFrameBytes {
		o.droppedOversized.Add(1)
		return
	}
	select {
	case o.frames <- observedFrame{msg: msg, ts: ts}:
	default:
		o.droppedQueued.Add(1)
	}
}

// Dropped reports how many frames were not turned into events: queue
// saturation, oversized frames and classification panics. Reported on
// cdp_disconnect so a reader sees the loss rather than only the VM's log.
func (o *cdpObserver) Dropped() int64 {
	if o == nil {
		return 0
	}
	return o.droppedQueued.Load() + o.droppedOversized.Load() + o.droppedPanicked.Load()
}

func (o *cdpObserver) run(ctx context.Context) {
	defer close(o.drained)
	for {
		select {
		case <-ctx.Done():
			o.drain()
			o.logDrops()
			return
		case f := <-o.frames:
			o.handle(f)
		}
	}
}

// drain classifies what is already queued once the pump is done, so a client's
// last commands still produce events rather than dying with the connection.
// The queue is bounded, so this is too.
func (o *cdpObserver) drain() {
	for {
		select {
		case f := <-o.frames:
			o.handle(f)
		default:
			return
		}
	}
}

// WaitDrained blocks until the worker has finished the queue. Bounded, so a
// wedged publisher delays connection teardown by at most timeout.
func (o *cdpObserver) WaitDrained(timeout time.Duration) {
	if o == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-o.drained:
	case <-timer.C:
	}
}

// handle classifies one frame. The recover is what keeps a malformed-input bug
// in a sanitizer, or a panicking publisher, from taking the VM down: the pump
// is a bare goroutine and so is this one.
func (o *cdpObserver) handle(f observedFrame) {
	defer func() {
		if r := recover(); r != nil {
			o.droppedPanicked.Add(1)
			o.logger.Error("cdp command telemetry panicked", slog.Any("err", r))
		}
	}()
	ev, ok := cdpCommandEvent(f.msg, f.ts, o.excludedMethods())
	if !ok {
		return
	}
	o.publish(ev)
}

func (o *cdpObserver) logDrops() {
	queued, oversized, panicked := o.droppedQueued.Load(), o.droppedOversized.Load(), o.droppedPanicked.Load()
	if queued+oversized+panicked == 0 {
		return
	}
	o.logger.Warn("cdp command telemetry dropped frames",
		slog.Int64("queue_full", queued),
		slog.Int64("oversized", oversized),
		slog.Int64("panicked", panicked))
}
