package cdpmonitor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/require"
)

type changingUpstream struct {
	*testUpstream
	once sync.Once
	next string
}

func (u *changingUpstream) Current() string {
	current := u.testUpstream.Current()
	u.once.Do(func() { u.notifyRestart(u.next) })
	return current
}

func TestNetworkSubscriptionPrecedesInitialDial(t *testing.T) {
	old := newTestServer(t)
	defer old.close()
	fresh := newTestServer(t)
	defer fresh.close()
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(old, stop, nil)
	go listenAndRespond(fresh, stop, nil)
	upstream := &changingUpstream{testUpstream: newTestUpstream(old.wsURL()), next: fresh.wsURL()}
	m := New(upstream, newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	select {
	case <-fresh.connCh:
	case <-time.After(3 * time.Second):
		t.Fatal("lost restart between Current and dial")
	}
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, time.Second, 10*time.Millisecond)
}

func TestTelemetryDisableDrainsComputedPublication(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	m := New(newTestUpstream(srv.wsURL()), func(ev events.Event) (events.Envelope, bool) {
		if ev.Type == EventNetworkIdle {
			close(entered)
			<-release
		}
		return events.Envelope{Event: ev}, true
	}, 0, discardLogger, nil)
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	defer unblock()
	<-srv.connCh
	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": "s", "targetInfo": map[string]any{"targetId": "t", "type": "page"}}})
	require.Eventually(t, func() bool { return m.computedFor("s") != nil }, time.Second, time.Millisecond)
	state := m.computedFor("s")
	go state.publish(events.Event{Type: EventNetworkIdle})
	<-entered
	done := make(chan error, 1)
	go func() { done <- m.SetTelemetry(false) }()
	select {
	case <-done:
		t.Fatal("disable returned before old computed publication drained")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	require.NoError(t, <-done)
	require.NoError(t, m.SetTelemetry(true))
	// Publishing from an old timer after re-enable must not reach the publisher.
	_, published := state.publish(events.Event{Type: EventNetworkIdle})
	require.False(t, published)
}

func TestTelemetryDisableDrainsSetupWhileCountersContinue(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	blocked := make(chan cdpMessage, 1)
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		if msg.Method == "Runtime.enable" {
			blocked <- msg
			return map[string]any{"method": "Test.unanswered"}
		}
		return nil
	})
	m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(ctx))
	defer m.Stop()
	defer cancel()
	<-srv.connCh
	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": "s", "targetInfo": map[string]any{"targetId": "t", "type": "page"}}})
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		defer m.sessionsMu.RUnlock()
		return m.networkReady["s"]
	}, time.Second, time.Millisecond)
	require.NoError(t, m.SetTelemetry(true))
	var command cdpMessage
	select {
	case command = <-blocked:
	case <-time.After(time.Second):
		t.Fatal("optional setup did not start")
	}
	done := make(chan error, 1)
	go func() { done <- m.SetTelemetry(false) }()
	require.Eventually(t, m.telemetryChanging.Load, time.Second, time.Millisecond)
	srv.sendToMonitor(t, map[string]any{"method": "Network.loadingFailed", "sessionId": "s", "params": map[string]any{"requestId": "r", "errorText": "net::ERR_CONNECTION_RESET"}})
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Resets == 1 }, time.Second, time.Millisecond)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("desired state waited for optional setup")
	}
	require.NotEqual(t, m.desiredTelemetry.Load(), m.appliedTelemetry.Load())
	srv.sendToMonitor(t, map[string]any{"id": command.ID, "result": map[string]any{}})
	waitForTelemetryReconcile(t, m, false)
	require.True(t, m.NetworkSnapshot().Up)
}
