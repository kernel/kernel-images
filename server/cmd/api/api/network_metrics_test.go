package api

import (
	"context"
	"sync"
	"testing"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestNetworkMonitorOutlivesCustomerTelemetry(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	require.NoError(t, svc.StartNetworkMonitor())
	defer svc.Shutdown(context.Background())
	require.True(t, svc.cdpMonitor.IsRunning())
	resets, completed, up := svc.NetworkMetrics()
	require.Zero(t, resets)
	require.Zero(t, completed)
	require.False(t, up)
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
