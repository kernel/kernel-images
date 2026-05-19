package e2e

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChromiumConfigureStartURLBare(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"WIDTH":  "1024",
			"HEIGHT": "768",
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx))

	client, err := c.APIClient()
	require.NoError(t, err)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	require.NoError(t, w.WriteField("start_url", `https://example.com/`))
	require.NoError(t, w.Close())

	rsp, err := client.ChromiumConfigureWithBodyWithResponse(ctx, w.FormDataContentType(), io.NopCloser(&buf))
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status=%s body=%s", rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200, "want ok json")
	require.True(t, rsp.JSON200.Ok)
}
