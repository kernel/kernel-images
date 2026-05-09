package e2e

import (
	"context"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestScaleToZeroDisableEnable exercises POST /scaletozero/{disable,enable}
// against the real built image. The unikraft control file does not exist
// inside the docker test container, so the underlying scale-to-zero write is
// a no-op — this test validates HTTP wiring, idempotency, and that the
// scale-to-zero middleware coexists with the disable/enable handlers.
func TestScaleToZeroDisableEnable(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Idempotent disable.
	r1, err := client.DisableScaleToZeroWithResponse(ctx)
	require.NoError(t, err, "DisableScaleToZero request failed")
	require.Equal(t, http.StatusNoContent, r1.StatusCode(), "unexpected status: %s body=%s", r1.Status(), string(r1.Body))

	r2, err := client.DisableScaleToZeroWithResponse(ctx)
	require.NoError(t, err, "second DisableScaleToZero request failed")
	require.Equal(t, http.StatusNoContent, r2.StatusCode(), "unexpected status: %s body=%s", r2.Status(), string(r2.Body))

	// Normal request must still flow while disabled (scaletozero middleware
	// runs on every request — the pin must not deadlock or break it).
	readResp, err := client.ReadClipboardWithResponse(ctx)
	require.NoError(t, err, "ReadClipboard request failed while disabled")
	require.Equal(t, http.StatusOK, readResp.StatusCode(), "unexpected read status while disabled: %s body=%s", readResp.Status(), string(readResp.Body))

	// Idempotent enable.
	r3, err := client.EnableScaleToZeroWithResponse(ctx)
	require.NoError(t, err, "EnableScaleToZero request failed")
	require.Equal(t, http.StatusNoContent, r3.StatusCode(), "unexpected status: %s body=%s", r3.Status(), string(r3.Body))

	r4, err := client.EnableScaleToZeroWithResponse(ctx)
	require.NoError(t, err, "second EnableScaleToZero request failed")
	require.Equal(t, http.StatusNoContent, r4.StatusCode(), "unexpected status: %s body=%s", r4.Status(), string(r4.Body))
}
