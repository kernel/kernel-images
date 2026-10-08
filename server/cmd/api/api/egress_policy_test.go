package api

import (
	"context"
	"testing"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// A VM nothing has been applied to behaves as it did before the policy
// existed.
func TestGetNetworkEgressPolicyDefaultsToUnfiltered(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)

	resp, err := svc.GetNetworkEgressPolicy(context.Background(), oapi.GetNetworkEgressPolicyRequestObject{})
	require.NoError(t, err)
	require.Equal(t, oapi.GetNetworkEgressPolicy200JSONResponse{Filtered: false}, resp)
}

// The control plane applies the policy at setup and again whenever an
// allowlist is added to or removed from a running session, so the endpoint has
// to carry a session both ways and tolerate being told the same thing twice.
// The pin is already in place, as the launcher leaves it once Chromium has
// started filtered, so nothing here needs a restart.
func TestPutNetworkEgressPolicy(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	require.NoError(t, svc.egressPin.Sync(true, []string{"--proxy-server=http://192.0.2.1:3129"}))

	for _, want := range []bool{true, true, false, false} {
		resp, err := svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{
			Body: &oapi.NetworkEgressPolicy{Filtered: want},
		})
		require.NoError(t, err)
		require.Equal(t, oapi.PutNetworkEgressPolicy200JSONResponse{Filtered: want}, resp)
		require.Equal(t, want, svc.egressPolicy.Filtered())
		pinned, err := svc.egressPin.Present()
		require.NoError(t, err)
		require.Equal(t, want, pinned)
	}
}

// Without the pin, filtering means restarting Chromium so the launcher writes
// it. When that fails the caller is told so, and the session stays filtered:
// the next Chromium start pins the proxy, and the CDP proxy keeps refusing
// contexts with their own proxy in the meantime.
func TestPutNetworkEgressPolicyFailsWhenThePinCannotBeApplied(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	// Leaves supervisorctl off PATH, so the restart fails.
	t.Setenv("PATH", t.TempDir())

	resp, err := svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{
		Body: &oapi.NetworkEgressPolicy{Filtered: true},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.PutNetworkEgressPolicy500JSONResponse{}, resp)
	require.True(t, svc.egressPolicy.Filtered())
	pinned, err := svc.egressPin.Present()
	require.NoError(t, err)
	require.False(t, pinned)
}

func TestPutNetworkEgressPolicyRejectsMissingBody(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)

	resp, err := svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.PutNetworkEgressPolicy400JSONResponse{}, resp)
	require.False(t, svc.egressPolicy.Filtered())
}
