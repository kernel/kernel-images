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
	// cdpObserverMaxQueuedBytes bounds the memory frames awaiting classification
	// can hold. A per-frame cap would be the obvious bound, but the pump cannot
	// know a frame's method without parsing it, so a cap there rejects a large
	// Runtime.callFunctionOn — which would never have produced an event — as
	// though a command had been lost. Budgeting the queue as a whole bounds the
	// same memory while leaving that judgement to the worker.
	cdpObserverMaxQueuedBytes = 8 << 20
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

	// connectionID names this proxy connection on every event it produces, so
	// concurrent clients driving one browser can be told apart.
	connectionID string

	// queuedBytes tracks what the queue is holding, so admission can be decided
	// on bytes rather than frame count alone.
	queuedBytes      atomic.Int64
	droppedQueued    atomic.Int64
	droppedPanicked  atomic.Int64
	droppedMalformed atomic.Int64
	excluded         atomic.Int64
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
func newCdpObserver(ctx context.Context, connectionID string, publish EventPublisher, controlEnabled ControlEnabledFunc, excludedMethods ExcludedMethodsFunc, logger *slog.Logger) *cdpObserver {
	if publish == nil || controlEnabled == nil {
		return nil
	}
	if excludedMethods == nil {
		excludedMethods = func() map[string]struct{} { return nil }
	}
	o := &cdpObserver{
		frames:          make(chan observedFrame, cdpObserverQueueDepth),
		drained:         make(chan struct{}),
		connectionID:    connectionID,
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
	size := int64(len(msg))
	if o.queuedBytes.Add(size) > cdpObserverMaxQueuedBytes {
		o.queuedBytes.Add(-size)
		o.droppedQueued.Add(1)
		return
	}
	select {
	case o.frames <- observedFrame{msg: msg, ts: ts}:
	default:
		o.queuedBytes.Add(-size)
		o.droppedQueued.Add(1)
	}
}

// Excluded reports how many supported commands produced no event because
// excluded_methods named their method. Counted, as the review asked, but apart
// from Dropped: a reader who configured an exclusion has not lost anything.
func (o *cdpObserver) Excluded() int64 {
	if o == nil {
		return 0
	}
	return o.excluded.Load()
}

// Dropped reports how many forwarded frames the classifier never saw or could
// not read: queue saturation, classification panics, commands whose arguments
// did not decode, and anything still queued once the worker has stopped.
// Reported on cdp_disconnect so a reader sees the loss rather than only the
// VM's log. A saturated queue rejects whatever arrives next, which may be
// library traffic that would have produced nothing, so this is an upper bound
// on commands lost rather than a count.
func (o *cdpObserver) Dropped() int64 {
	if o == nil {
		return 0
	}
	dropped := o.droppedQueued.Load() + o.droppedPanicked.Load() + o.droppedMalformed.Load()
	// Pump calls onClose as soon as one direction fails, while the other may
	// still be forwarding, so a frame can be queued after the final drain. Once
	// the worker has stopped nothing will read it, which makes it as lost as one
	// the queue turned away — and silently so, unless it is counted here.
	select {
	case <-o.drained:
		dropped += int64(len(o.frames))
	default:
	}
	return dropped
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
	defer o.queuedBytes.Add(-int64(len(f.msg)))
	defer func() {
		if r := recover(); r != nil {
			o.droppedPanicked.Add(1)
			o.logger.Error("cdp command telemetry panicked", slog.Any("err", r))
		}
	}()
	ev, result := cdpCommandEvent(f.msg, f.ts, o.connectionID, o.excludedMethods())
	switch result {
	case classifyEvent:
		o.publish(ev)
	case classifyExcluded:
		o.excluded.Add(1)
	case classifyMalformed:
		// The command reached the browser but nothing readable reached the
		// stream, which is a loss however malformed the arguments were.
		o.droppedMalformed.Add(1)
	}
}

func (o *cdpObserver) logDrops() {
	queued, panicked, malformed := o.droppedQueued.Load(), o.droppedPanicked.Load(), o.droppedMalformed.Load()
	if queued+panicked+malformed == 0 {
		return
	}
	o.logger.Warn("cdp command telemetry dropped frames",
		slog.Int64("queue_full", queued),
		slog.Int64("panicked", panicked),
		slog.Int64("malformed", malformed))
}
