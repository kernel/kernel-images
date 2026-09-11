package cdpmonitor

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMetricsOnlyDomainsAndTelemetryTransitions(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	var mu sync.Mutex
	var methods []string
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		mu.Lock()
		methods = append(methods, msg.Method)
		mu.Unlock()
		if msg.Method == "Page.addScriptToEvaluateOnNewDocument" {
			return map[string]any{"id": msg.ID, "result": map[string]any{"identifier": "script"}}
		}
		return nil
	})
	ec := newEventCollector()
	m := New(newTestUpstream(srv.wsURL()), ec.publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	<-srv.connCh
	srv.sendToMonitor(t, map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": "s", "targetInfo": map[string]any{"targetId": "t", "type": "page"}}})
	require.Eventually(t, func() bool {
		m.sessionsMu.RLock()
		ready := m.networkReady["s"]
		m.sessionsMu.RUnlock()
		return ready && m.NetworkSnapshot().Up
	}, 3*time.Second, 10*time.Millisecond)
	mu.Lock()
	initialMethods := slices.Clone(methods)
	mu.Unlock()
	require.Contains(t, initialMethods, "Network.enable")
	for _, forbidden := range []string{"Runtime.enable", "Page.enable", "PerformanceTimeline.enable", "Runtime.addBinding", "Page.addScriptToEvaluateOnNewDocument", "Network.getResponseBody"} {
		require.NotContains(t, initialMethods, forbidden)
	}
	m.sessionsMu.RLock()
	computedCount := len(m.computedStates)
	m.sessionsMu.RUnlock()
	require.Zero(t, computedCount)
	require.Zero(t, ec.checkpoint())
	require.NoError(t, m.SetTelemetry(true))
	ec.waitFor(t, EventTabOpened, time.Second)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(methods, "Runtime.evaluate")
	}, time.Second, time.Millisecond)
	require.NoError(t, m.SetTelemetry(false))
	mu.Lock()
	finalMethods := slices.Clone(methods)
	mu.Unlock()
	require.Contains(t, finalMethods, "Page.removeScriptToEvaluateOnNewDocument")
	require.Contains(t, finalMethods, "Runtime.removeBinding")
	require.Contains(t, finalMethods, "Runtime.disable")
	require.NotContains(t, finalMethods, "Network.disable")
	srv.sendToMonitor(t, map[string]any{"method": "Network.loadingFailed", "sessionId": "s", "params": map[string]any{"requestId": "r", "errorText": "net::ERR_CONNECTION_RESET"}})
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Resets == 1 }, time.Second, time.Millisecond)
	require.True(t, m.NetworkSnapshot().Up)
}

func TestNetworkMonitorRetriesStartupAndSameURLDisconnect(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	var discovers atomic.Int32
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, func(msg cdpMessage) any {
		if msg.Method == "Target.setDiscoverTargets" {
			discovers.Add(1)
		}
		return nil
	})
	upstream := newTestUpstream("")
	m := New(upstream, newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	require.NoError(t, m.Start(context.Background()))
	defer m.Stop()
	require.False(t, m.NetworkSnapshot().Up)
	upstream.notifyRestart(srv.wsURL())
	require.Eventually(t, func() bool { return m.NetworkSnapshot().Up }, 3*time.Second, 10*time.Millisecond)
	m.network.terminal("s", "r", "net::ERR_CONNECTION_RESET")
	srv.connMu.Lock()
	conn := srv.conn
	srv.connMu.Unlock()
	require.NoError(t, conn.CloseNow())
	require.Eventually(t, func() bool { return !m.NetworkSnapshot().Up }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return discovers.Load() >= 2 && m.NetworkSnapshot().Up }, 4*time.Second, 10*time.Millisecond)
	require.Equal(t, uint64(1), m.NetworkSnapshot().Resets)
	m.network.terminal("s", "r", "net::ERR_CONNECTION_RESET")
	require.Equal(t, uint64(2), m.NetworkSnapshot().Resets)
	m.Stop()
	require.False(t, m.NetworkSnapshot().Up)
}

func TestNetworkMonitorRetriesInitializationFailures(t *testing.T) {
	for _, method := range []string{"Target.setDiscoverTargets", "Target.getTargets", "Target.attachToTarget", "Target.setAutoAttach", "Network.enable"} {
		t.Run(method, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.close()
			var failed atomic.Bool
			stop := make(chan struct{})
			defer close(stop)
			go listenAndRespond(srv, stop, func(msg cdpMessage) any {
				if msg.Method == method && failed.CompareAndSwap(false, true) {
					return map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "fixture failure"}}
				}
				switch msg.Method {
				case "Target.getTargets":
					return map[string]any{"id": msg.ID, "result": map[string]any{"targetInfos": []any{map[string]any{"targetId": "t", "type": "page"}}}}
				case "Target.attachToTarget":
					return map[string]any{"id": msg.ID, "result": map[string]any{"sessionId": "s"}}
				}
				return nil
			})
			m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
			require.NoError(t, m.SetTelemetry(false))
			require.NoError(t, m.Start(context.Background()))
			defer m.Stop()
			require.Eventually(t, func() bool { return failed.Load() && m.NetworkSnapshot().Up }, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestConcurrentMonitorConfigurationAndShutdown(t *testing.T) {
	srv := newTestServer(t)
	defer srv.close()
	stop := make(chan struct{})
	defer close(stop)
	go listenAndRespond(srv, stop, nil)
	m := New(newTestUpstream(srv.wsURL()), newEventCollector().publishFn(), 0, discardLogger, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, m.Start(ctx))
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for i := range 20 {
				_ = m.SetTelemetry(i%2 == 0)
				_ = m.NetworkSnapshot()
			}
		})
	}
	cancel()
	m.Stop()
	wg.Wait()
	require.False(t, m.IsRunning())
	require.False(t, m.NetworkSnapshot().Up)
}
