package api

import (
	"context"
	"testing"

	"github.com/kernel/kernel-images/server/lib/egresspolicy"
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

// The control plane applies the policy at setup and again whenever a running
// session's allowlist changes, so the endpoint has to carry a session both ways
// and tolerate being told the same thing twice. The session starts filtered
// with its pin in place, as the launcher leaves it, so nothing here needs a
// restart.
func TestPutNetworkEgressPolicy(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	filtered := egresspolicy.Policy{Filtered: true}
	require.NoError(t, svc.egressPolicy.Set(filtered))
	require.NoError(t, svc.egressPin.Sync(filtered, []string{"--proxy-server=http://192.0.2.1:3129"}))

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

// The pin's bypass list is the policy's private hosts, so a pin written for
// other private hosts is rewritten with a restart, and one written for these
// is left alone. The new hosts are recorded even when the restart fails, so the
// next Chromium start pins them.
func TestPutNetworkEgressPolicyRestartsForChangedPrivateHosts(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)
	before := egresspolicy.Policy{Filtered: true, PrivateHosts: &[]string{"10.1.0.0/16"}}
	require.NoError(t, svc.egressPolicy.Set(before))
	require.NoError(t, svc.egressPin.Sync(before, []string{"--proxy-server=http://192.0.2.1:3129"}))
	// Leaves supervisorctl off PATH, so a restart fails.
	t.Setenv("PATH", t.TempDir())

	after := []string{"preview.internal:8443"}
	resp, err := svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{
		Body: &oapi.NetworkEgressPolicy{Filtered: true, PrivateHosts: &after},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.PutNetworkEgressPolicy500JSONResponse{}, resp, "changed private hosts did not restart Chromium")
	require.Equal(t, &after, svc.egressPolicy.Policy().PrivateHosts)

	// Stands in for the launcher, which writes the pin on the next start.
	require.NoError(t, svc.egressPin.Sync(svc.egressPolicy.Policy(), []string{"--proxy-server=http://192.0.2.1:3129"}))
	resp, err = svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{
		Body: &oapi.NetworkEgressPolicy{Filtered: true, PrivateHosts: &after},
	})
	require.NoError(t, err)
	require.Equal(t, oapi.PutNetworkEgressPolicy200JSONResponse{Filtered: true, PrivateHosts: &after}, resp)
}

// Chromium splits its bypass list on ";" and ",", so an entry carrying either
// would add bypass rules of its own.
func TestPutNetworkEgressPolicyRejectsMalformedPrivateHosts(t *testing.T) {
	svc, err := newSvc(t, newMockRecordManager())
	require.NoError(t, err)

	for _, host := range []string{"", "a.internal;*", "a.internal,*", "a.internal *"} {
		hosts := []string{"10.1.0.0/16", host}
		resp, err := svc.PutNetworkEgressPolicy(context.Background(), oapi.PutNetworkEgressPolicyRequestObject{
			Body: &oapi.NetworkEgressPolicy{Filtered: true, PrivateHosts: &hosts},
		})
		require.NoError(t, err)
		require.IsType(t, oapi.PutNetworkEgressPolicy400JSONResponse{}, resp, "accepted private host %q", host)
		require.False(t, svc.egressPolicy.Filtered())
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
