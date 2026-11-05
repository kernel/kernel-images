package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestNetworkMonitorOutlivesCustomerTelemetry(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	require.NoError(t, svc.StartNetworkMonitor())
	defer svc.Shutdown(context.Background())
	require.True(t, svc.cdpMonitor.IsRunning())
	resets, completed, up, failures := svc.NetworkMetrics()
	require.Zero(t, resets)
	require.Zero(t, completed)
	require.False(t, up)
	require.Len(t, failures, 85)
	for _, counts := range failures {
		require.Equal(t, [2]uint64{}, counts)
	}
	_, err = svc.PutTelemetry(context.Background(), oapi.PutTelemetryRequestObject{})
	require.NoError(t, err)
	require.True(t, svc.cdpMonitor.IsRunning())
	_, err = svc.PutTelemetry(context.Background(), oapi.PutTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()}})
	require.NoError(t, err)
	require.False(t, svc.telemetrySession.Active())
	require.True(t, svc.cdpMonitor.IsRunning(), "disabling telemetry must not stop network monitoring")
	require.NoError(t, svc.Shutdown(context.Background()))
	require.False(t, svc.cdpMonitor.IsRunning())
	require.Error(t, svc.StartNetworkMonitor(), "a stopped API lifecycle must not restart capture")
}

func TestShutdownCancelsMonitorBeforeWaitingForRepl(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	require.NoError(t, svc.StartNetworkMonitor())
	defer svc.Shutdown(context.Background())
	require.NoError(t, svc.browserRepl.acquire(context.Background()))
	var once sync.Once
	release := func() { once.Do(svc.browserRepl.release) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- svc.Shutdown(context.Background()) }()
	select {
	case <-svc.lifecycleCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("monitor cancellation waited for REPL shutdown")
	}
	require.Eventually(t, func() bool { return svc.browserRepl.lifecycle.Err() != nil }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return !svc.cdpMonitor.IsRunning() }, time.Second, time.Millisecond)
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after releasing REPL admission")
	}
}

func TestNetworkMonitorTelemetryShutdownRace(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	require.NoError(t, svc.StartNetworkMonitor())
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 20 {
				_, _ = svc.PutTelemetry(context.Background(), oapi.PutTelemetryRequestObject{})
				_, _ = svc.PutTelemetry(context.Background(), oapi.PutTelemetryRequestObject{Body: &oapi.BrowserTelemetryConfig{Browser: allCategoriesDisabled()}})
			}
		})
	}
	require.NoError(t, svc.Shutdown(context.Background()))
	wg.Wait()
	require.False(t, svc.cdpMonitor.IsRunning())
	require.False(t, svc.telemetrySession.Active())
}
