package devtoolsproxy

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
)

const clickFrame = `{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":1,"y":2}}`

// countingPublisher records how many events reached the bus.
type countingPublisher struct {
	n atomic.Int64
}

func (c *countingPublisher) publish(ev events.Event) (events.Envelope, bool) {
	c.n.Add(1)
	return events.Envelope{Event: ev}, true
}

func newTestObserver(t *testing.T, publish EventPublisher, enabled ControlEnabledFunc) *cdpObserver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := newCdpObserver(ctx, testConnID, publish, enabled, nil, silentLogger())
	if o == nil {
		t.Fatal("observer was not created")
	}
	return o
}

// The gate is what makes telemetry free when it is off: a frame observed with
// control disabled must not be retained, parsed or queued.
func TestObserverDoesNoWorkWhenControlIsDisabled(t *testing.T) {
	pub := &countingPublisher{}
	o := newTestObserver(t, pub.publish, func() bool { return false })

	for range 100 {
		o.Observe([]byte(clickFrame), testForwardTs)
	}
	if queued := len(o.frames); queued != 0 {
		t.Fatalf("queued %d frames with control disabled, want 0", queued)
	}
	if got := pub.n.Load(); got != 0 {
		t.Fatalf("published %d events with control disabled, want 0", got)
	}
	if got := o.Dropped(); got != 0 {
		t.Fatalf("counted %d drops with control disabled, want 0: a frame nobody wanted is not a loss", got)
	}
}

func TestObserveAllocatesNothingWhenControlIsDisabled(t *testing.T) {
	o := newTestObserver(t, (&countingPublisher{}).publish, func() bool { return false })
	frame := []byte(clickFrame)
	allocs := testing.AllocsPerRun(1000, func() { o.Observe(frame, testForwardTs) })
	if allocs != 0 {
		t.Fatalf("Observe allocated %v times per call with control disabled, want 0", allocs)
	}
}

// A panicking publisher is the failure the pump must survive: before this the
// panic unwound through the message transform and took the process with it.
func TestPanickingPublisherIsContainedAndCounted(t *testing.T) {
	var published atomic.Int64
	panicking := func(ev events.Event) (events.Envelope, bool) {
		published.Add(1)
		panic("publisher exploded")
	}
	o := newTestObserver(t, panicking, controlOn)

	for range 5 {
		o.Observe([]byte(clickFrame), testForwardTs)
	}
	waitFor(t, func() bool { return o.Dropped() == 5 })
	if got := published.Load(); got != 5 {
		t.Fatalf("publisher called %d times, want 5: the worker must keep going after a panic", got)
	}
}

// Saturation is an acceptable loss, but only a counted one.
func TestQueueSaturationIsCounted(t *testing.T) {
	blocked := make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(blocked) }) })

	blocking := func(ev events.Event) (events.Envelope, bool) {
		<-blocked
		return events.Envelope{Event: ev}, true
	}
	o := newTestObserver(t, blocking, controlOn)

	// One frame occupies the worker, the queue absorbs cdpObserverQueueDepth
	// more, and everything past that is dropped rather than blocking the pump.
	const overshoot = 50
	for range cdpObserverQueueDepth + overshoot + 1 {
		o.Observe([]byte(clickFrame), testForwardTs)
	}
	waitFor(t, func() bool { return o.Dropped() > 0 })
	if got := o.Dropped(); got > overshoot+1 {
		t.Fatalf("dropped %d frames, want at most %d: the queue should absorb the rest", got, overshoot+1)
	}
}

// A big frame is admitted on its merits, not rejected for its size: a large
// paste is a real command, and rejecting it as a lost command is wrong. Only
// the queue's byte budget turns one away.
func TestLargeFramesAreClassifiedRatherThanRejected(t *testing.T) {
	pub := &countingPublisher{}
	o := newTestObserver(t, pub.publish, controlOn)

	big := `{"id":1,"method":"Input.insertText","params":{"text":"` +
		strings.Repeat("x", 256<<10) + `"}}`
	o.Observe([]byte(big), testForwardTs)
	waitFor(t, func() bool { return pub.n.Load() == 1 })

	if got := o.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0: the frame was classified, not lost", got)
	}
}

// Library traffic never reaches the queue, so it neither occupies capacity nor
// counts as a loss, however large it is.
func TestLibraryTrafficIsNeverAdmitted(t *testing.T) {
	pub := &countingPublisher{}
	o := newTestObserver(t, pub.publish, controlOn)

	big := `{"id":1,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"` +
		strings.Repeat("x", 256<<10) + `","objectId":"x"}}`
	o.Observe([]byte(big), testForwardTs)

	if queued := len(o.frames); queued != 0 {
		t.Fatalf("queued %d library frames, want 0", queued)
	}
	if got := o.queuedBytes.Load(); got != 0 {
		t.Fatalf("library traffic held %d queued bytes, want 0", got)
	}
	if got := pub.n.Load(); got != 0 {
		t.Fatalf("published %d events for library traffic, want 0", got)
	}
	if got := o.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0: a frame that would never be an event is not a loss", got)
	}
}

// The failure raf reported: queue capacity exists for control commands, so
// library traffic must not be able to fill it and push a real one out.
func TestLibraryTrafficCannotCrowdOutCommands(t *testing.T) {
	blocked := make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(blocked) }) })

	pub := &countingPublisher{}
	blocking := func(ev events.Event) (events.Envelope, bool) {
		pub.n.Add(1)
		<-blocked
		return events.Envelope{Event: ev}, true
	}
	o := newTestObserver(t, blocking, controlOn)

	// One real command wedges the worker, then far more library frames than the
	// queue could hold arrive.
	o.Observe([]byte(clickFrame), testForwardTs)
	waitFor(t, func() bool { return pub.n.Load() == 1 })
	junk := []byte(`{"id":9,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"` +
		strings.Repeat("x", 1024) + `","objectId":"x"}}`)
	for range cdpObserverQueueDepth * 2 {
		o.Observe(junk, testForwardTs)
	}

	// A real navigation arriving now still gets a slot.
	before := o.Dropped()
	o.Observe([]byte(`{"id":100,"method":"Page.navigate","params":{"url":"https://x.example/"}}`), testForwardTs)
	if o.Dropped() != before {
		t.Fatalf("a real command was dropped after %d library frames", cdpObserverQueueDepth*2)
	}
}

// An excluded method is turned away at admission, so it does not occupy the
// queue either, and is still counted apart from the drops.
func TestExcludedMethodsAreNotAdmitted(t *testing.T) {
	pub := &countingPublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	excluded := func() map[string]struct{} {
		return map[string]struct{}{"Input.dispatchMouseEvent": {}}
	}
	o := newCdpObserver(ctx, testConnID, pub.publish, controlOn, excluded, silentLogger())

	for range 3 {
		o.Observe([]byte(clickFrame), testForwardTs)
	}
	if queued := len(o.frames); queued != 0 {
		t.Fatalf("queued %d excluded frames, want 0", queued)
	}
	if got := o.Excluded(); got != 3 {
		t.Fatalf("excluded = %d, want 3", got)
	}
	if got := o.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0: an exclusion is not a loss", got)
	}
}

// The byte budget is what bounds memory, since there is no per-frame cap.
func TestQueueByteBudgetTurnsAwayWhatItCannotHold(t *testing.T) {
	blocked := make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(blocked) }) })

	blocking := func(ev events.Event) (events.Envelope, bool) {
		<-blocked
		return events.Envelope{Event: ev}, true
	}
	o := newTestObserver(t, blocking, controlOn)

	// Each frame is a sixteenth of the budget, so the budget binds well before
	// the queue depth does.
	frame := []byte(`{"id":1,"method":"Input.insertText","params":{"text":"` +
		strings.Repeat("x", cdpObserverMaxQueuedBytes/16) + `"}}`)
	for range 32 {
		o.Observe(frame, testForwardTs)
	}
	waitFor(t, func() bool { return o.Dropped() > 0 })
	if got := o.queuedBytes.Load(); got > cdpObserverMaxQueuedBytes {
		t.Fatalf("queued %d bytes, over the %d budget", got, cdpObserverMaxQueuedBytes)
	}
}

// Teardown must not lose the commands a client sent last.
func TestObserverDrainsQueuedFramesOnShutdown(t *testing.T) {
	pub := &countingPublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	o := newCdpObserver(ctx, testConnID, pub.publish, controlOn, nil, silentLogger())

	const sent = 20
	for range sent {
		o.Observe([]byte(clickFrame), testForwardTs)
	}
	cancel()
	o.WaitDrained(5 * time.Second)

	if got := pub.n.Load(); got != sent {
		t.Fatalf("published %d events, want %d: queued commands must survive teardown", got, sent)
	}
}

func TestObserverAppliesMethodExclusions(t *testing.T) {
	pub := &countingPublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	excluded := func() map[string]struct{} {
		return map[string]struct{}{"Input.dispatchMouseEvent": {}}
	}
	o := newCdpObserver(ctx, testConnID, pub.publish, controlOn, excluded, silentLogger())

	o.Observe([]byte(clickFrame), testForwardTs)
	o.Observe([]byte(`{"id":2,"method":"Page.reload"}`), testForwardTs)
	waitFor(t, func() bool { return pub.n.Load() == 1 })

	// An excluded method is not a drop: nothing was lost, it was configured out.
	if got := o.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

// Pump reports a disconnect as soon as one direction fails, while the other can
// still forward, so a frame can reach the queue after the worker has drained
// and stopped. Nothing will classify it, so it has to be counted rather than
// quietly left behind: telemetry_dropped is what tells a reader the tail of the
// session is incomplete.
func TestFramesQueuedAfterTeardownAreCountedAsLoss(t *testing.T) {
	pub := &countingPublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	o := newCdpObserver(ctx, testConnID, pub.publish, controlOn, nil, silentLogger())

	o.Observe([]byte(clickFrame), testForwardTs)
	cancel()
	o.WaitDrained(5 * time.Second)
	if got := pub.n.Load(); got != 1 {
		t.Fatalf("published %d events before teardown, want 1", got)
	}
	if got := o.Dropped(); got != 0 {
		t.Fatalf("dropped = %d before the late frame, want 0", got)
	}

	// The straggler the other pump direction forwarded on its way out.
	o.Observe([]byte(clickFrame), testForwardTs)

	if got := pub.n.Load(); got != 1 {
		t.Fatalf("published %d events, want 1: the worker has stopped", got)
	}
	if got := o.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1: a frame nothing will read is a loss", got)
	}
}

// testConnID names the connection in observer tests.
const testConnID = "conn-test"

// An excluded method is configuration, not loss, so it is counted apart from
// the drops. The review asked for it to be counted either way.
func TestExcludedMethodsAreCountedApartFromDrops(t *testing.T) {
	pub := &countingPublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	excluded := func() map[string]struct{} {
		return map[string]struct{}{"Input.dispatchMouseEvent": {}}
	}
	o := newCdpObserver(ctx, testConnID, pub.publish, controlOn, excluded, silentLogger())

	for range 3 {
		o.Observe([]byte(clickFrame), testForwardTs)
	}
	waitFor(t, func() bool { return o.Excluded() == 3 })
	if got := o.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0: an exclusion is not a loss", got)
	}
}

// A supported command whose arguments do not decode reached the browser but
// produced no event, so it is a loss and has to be counted as one.
func TestMalformedParamsAreCountedAsLoss(t *testing.T) {
	pub := &countingPublisher{}
	o := newTestObserver(t, pub.publish, controlOn)

	// x is a string where the protocol defines a number.
	o.Observe([]byte(`{"id":1,"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":"nope"}}`), testForwardTs)
	waitFor(t, func() bool { return o.Dropped() == 1 })
	if got := pub.n.Load(); got != 0 {
		t.Fatalf("published %d events, want 0", got)
	}
}

// Library traffic is still not a loss, so the malformed counter must not catch
// frames that were never browser control to begin with.
func TestUnsupportedMethodsAreNotCountedAsLoss(t *testing.T) {
	pub := &countingPublisher{}
	o := newTestObserver(t, pub.publish, controlOn)

	o.Observe([]byte(`{"id":1,"method":"Runtime.callFunctionOn","params":{"objectId":5}}`), testForwardTs)
	o.Observe([]byte(`not json at all`), testForwardTs)
	waitFor(t, func() bool { return o.queuedBytes.Load() == 0 })
	if got := o.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
}

// Admission runs on the pump goroutine, so its cost is the cost of every
// forwarded client frame. It must not grow with the frame: the method decode
// copies no arguments.
func BenchmarkObserveControlCommand(b *testing.B) {
	o := benchObserver(b)
	frame := []byte(clickFrame)
	b.ReportAllocs()
	for b.Loop() {
		o.Observe(frame, testForwardTs)
	}
}

func BenchmarkObserveLibraryTraffic(b *testing.B) {
	o := benchObserver(b)
	frame := []byte(`{"id":1,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"() => 1","objectId":"x"}}`)
	b.ReportAllocs()
	for b.Loop() {
		o.Observe(frame, testForwardTs)
	}
}

func BenchmarkObserveLargeLibraryTraffic(b *testing.B) {
	o := benchObserver(b)
	frame := []byte(`{"id":1,"method":"Runtime.callFunctionOn","params":{"functionDeclaration":"` +
		strings.Repeat("x", 64<<10) + `","objectId":"x"}}`)
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for b.Loop() {
		o.Observe(frame, testForwardTs)
	}
}

// benchObserver drains continuously so the benchmark measures admission rather
// than a queue filling up.
func benchObserver(b *testing.B) *cdpObserver {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	o := newCdpObserver(ctx, testConnID, func(events.Event) (events.Envelope, bool) {
		return events.Envelope{}, true
	}, controlOn, nil, silentLogger())
	return o
}
