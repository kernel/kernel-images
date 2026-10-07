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
func TestPutNetworkEgressPolicy(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)

	for _, want := range []bool{true, true, false} {
		resp, err := svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{
			Body: &oapi.NetworkEgressPolicy{Filtered: want},
		})
		require.NoError(t, err)
		require.Equal(t, oapi.PutNetworkEgressPolicy200JSONResponse{Filtered: want}, resp)
		require.Equal(t, want, svc.egressPolicy.Filtered())
	}
}

func TestPutNetworkEgressPolicyRejectsMissingBody(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)

	resp, err := svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{})
	require.NoError(t, err)
	require.IsType(t, oapi.PutNetworkEgressPolicy400JSONResponse{}, resp)
	require.False(t, svc.egressPolicy.Filtered())
}
