package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

const (
	matDisplay = 1 << iota
	matPolicy
	matKioskFlags
	matExtension
	matStartURL

	matMaxBitmask = matDisplay | matPolicy | matKioskFlags | matExtension | matStartURL // 31
)

// TestChromiumConfigureMultipartPowerset runs a representative matrix by default.
// Set E2E_CHROMIUM_CONFIGURE_POWERSET=1 to run every non-empty combination.
func TestChromiumConfigureMultipartPowerset(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	extDir, err := filepath.Abs("test-extension")
	require.NoError(t, err)
	extZip, err := zipDirToBytes(extDir)
	require.NoError(t, err)

	matrix := []int{
		matDisplay,
		matPolicy | matKioskFlags,
		matExtension,
		matDisplay | matPolicy | matKioskFlags | matExtension | matStartURL,
	}
	if os.Getenv("E2E_CHROMIUM_CONFIGURE_POWERSET") == "1" {
		matrix = matrix[:0]
		for bits := 1; bits <= matMaxBitmask; bits++ {
			matrix = append(matrix, bits)
		}
	}

	for _, bits := range matrix {
		bits := bits
		t.Run(chromiumConfigurePowersetLabel(bits), func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()

			c := NewTestContainer(t, headlessImage)
			require.NoError(t, c.Start(ctx, ContainerConfig{
				Env: map[string]string{
					"WIDTH":  "1024",
					"HEIGHT": "768",
				},
			}), "failed to start container")
			defer func() { _ = c.Stop(context.WithoutCancel(ctx)) }()

			require.NoError(t, c.WaitReady(ctx))

			var body bytes.Buffer
			w := multipart.NewWriter(&body)
			require.NoError(t, chromiumConfigurePowersetPopulate(t, w, bits, extZip))
			require.NoError(t, w.Close())

			client, err := c.APIClient()
			require.NoError(t, err)

			rsp, err := client.ChromiumConfigureWithBodyWithResponse(ctx, w.FormDataContentType(), io.NopCloser(bytes.NewReader(body.Bytes())))
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, rsp.StatusCode(),
				"bits=%02x unexpected status=%s body=%s", bits, rsp.Status(), string(rsp.Body))
			require.NotNil(t, rsp.JSON200, "want ok JSON")
			require.True(t, rsp.JSON200.Ok)
		})
	}
}

func chromiumConfigurePowersetLabel(bits int) string {
	var p []string
	if bits&matDisplay != 0 {
		p = append(p, "display")
	}
	if bits&matPolicy != 0 {
		p = append(p, "policy")
	}
	if bits&matKioskFlags != 0 {
		p = append(p, "kiosk")
	}
	if bits&matExtension != 0 {
		p = append(p, "ext")
	}
	if bits&matStartURL != 0 {
		p = append(p, "nav")
	}
	return strings.Join(p, "+")
}

func chromiumConfigurePowersetPopulate(t *testing.T, w *multipart.Writer, bits int, extZip []byte) error {
	t.Helper()

	if bits&matDisplay != 0 {
		restart := true
		requireIdle := true
		disp := instanceoapi.PatchDisplayJSONRequestBody{
			Width:           intPtr(1280),
			Height:          intPtr(720),
			RestartChromium: &restart,
			RequireIdle:     &requireIdle,
		}
		blob, err := json.Marshal(disp)
		require.NoError(t, err)
		if err := w.WriteField("display", string(blob)); err != nil {
			return err
		}
	}

	if bits&matPolicy != 0 {
		// QuicAllowed false is benign and allowed by server policy registry / overrides validation.
		pol := map[string]interface{}{"QuicAllowed": false}
		blob, err := json.Marshal(pol)
		require.NoError(t, err)
		if err := w.WriteField("chrome_policies", string(blob)); err != nil {
			return err
		}
	}

	if bits&matKioskFlags != 0 {
		fl := instanceoapi.PatchChromiumFlagsJSONBody{Flags: []string{"--kiosk"}}
		blob, err := json.Marshal(fl)
		require.NoError(t, err)
		if err := w.WriteField("chromium_flags", string(blob)); err != nil {
			return err
		}
	}

	if bits&matExtension != 0 {
		part, err := w.CreateFormFile("extensions.zip_file", "powerset-ext.zip")
		if err != nil {
			return err
		}
		if _, err := io.Copy(part, bytes.NewReader(extZip)); err != nil {
			return err
		}
		if err := w.WriteField("extensions.name", "powerset"); err != nil {
			return err
		}
	}

	if bits&matStartURL != 0 {
		if err := w.WriteField("start_url", `data:text/html,<title>configure</title>`); err != nil {
			return err
		}
	}
	return nil
}

func intPtr(i int) *int { return &i }

// TestChromiumConfigureStartURLBareHost exercises Kernel-compatible bare host normalization.
func TestChromiumConfigureStartURLBareHost(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{"WIDTH": "1024", "HEIGHT": "768"},
	}))
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("start_url", "example.com"))
	require.NoError(t, mw.Close())

	client, err := c.APIClient()
	require.NoError(t, err)

	rsp, err := client.ChromiumConfigureWithBodyWithResponse(ctx, mw.FormDataContentType(), io.NopCloser(bytes.NewReader(buf.Bytes())))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "%s", string(rsp.Body))
	require.True(t, rsp.JSON200.Ok)
}
