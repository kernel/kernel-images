package e2e

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

type configureExtensionPart struct {
	name string
	zip  []byte
}

type configureE2ERequest struct {
	params        *instanceoapi.ChromiumConfigureParams
	extensions    []configureExtensionPart
	display       string
	chromiumFlags string
	startURL      string
}

func chromiumConfigureE2E(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, request configureE2ERequest) *instanceoapi.ChromiumConfigureResponse {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, extension := range request.extensions {
		part, err := writer.CreateFormFile("extensions.zip_file", extension.name+".zip")
		require.NoError(t, err)
		_, err = part.Write(extension.zip)
		require.NoError(t, err)
		require.NoError(t, writer.WriteField("extensions.name", extension.name))
	}
	if request.display != "" {
		require.NoError(t, writer.WriteField("display", request.display))
	}
	if request.chromiumFlags != "" {
		require.NoError(t, writer.WriteField("chromium_flags", request.chromiumFlags))
	}
	if request.startURL != "" {
		require.NoError(t, writer.WriteField("start_url", request.startURL))
	}
	require.NoError(t, writer.Close())

	response, err := client.ChromiumConfigureWithBodyWithResponse(ctx, request.params, writer.FormDataContentType(), bytes.NewReader(body.Bytes()))
	require.NoError(t, err)
	return response
}

func chromiumConfigureStartCount(t *testing.T, ctx context.Context, c *TestContainer) int {
	t.Helper()
	output, err := execCombinedOutputWithClient(ctx, c, "sh", []string{"-lc", `grep -c 'starting chromium via supervisorctl' /var/log/supervisord/kernel-images-api || true`})
	require.NoError(t, err)
	count, err := strconv.Atoi(strings.TrimSpace(output))
	require.NoError(t, err)
	return count
}

func requireConfiguredExtensionActive(t *testing.T, ctx context.Context, c *TestContainer, name string) {
	t.Helper()
	path := filepath.Join("/home/kernel/extensions", name)
	require.Eventually(t, func() bool {
		client, err := cdpclient.Dial(ctx, c.CDPURL())
		if err != nil {
			return false
		}
		defer client.Close()
		extensions, err := client.GetExtensions(ctx)
		if err != nil {
			return false
		}
		for _, extension := range extensions {
			if extension.Enabled && filepath.Clean(extension.Path) == path {
				return true
			}
		}
		return false
	}, 10*time.Second, 200*time.Millisecond)
}

func TestChromiumConfigureExtensionLoadStrategies(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	for _, test := range []struct {
		name               string
		image              string
		testPartialFailure bool
	}{
		{name: "headless", image: headlessImage, testPartialFailure: true},
		{name: "headful", image: headfulImage},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testChromiumConfigureExtensionLoadStrategies(t, test.image, test.testPartialFailure)
		})
	}
}

func testChromiumConfigureExtensionLoadStrategies(t *testing.T, image string, testPartialFailure bool) {
	ordinaryDir, err := filepath.Abs("test-extension")
	require.NoError(t, err)
	ordinaryZip, err := zipDirToBytes(ordinaryDir)
	require.NoError(t, err)
	enterpriseDir, err := filepath.Abs("test-extension-enterprise")
	require.NoError(t, err)
	enterpriseZip, err := zipDirToBytes(enterpriseDir)
	require.NoError(t, err)
	invalidDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(invalidDir, "manifest.json"), []byte(`{
		"manifest_version": 3,
		"name": "Invalid Configure Extension"
	}`), 0o600))
	invalidZip, err := zipDirToBytes(invalidDir)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := NewTestContainer(t, image)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{"WIDTH": "1024", "HEIGHT": "768"},
	}))
	defer c.Stop(context.WithoutCancel(ctx))
	require.NoError(t, c.WaitReady(ctx))
	require.NoError(t, c.WaitDevTools(ctx))
	client, err := c.APIClient()
	require.NoError(t, err)

	preferCDP := instanceoapi.PreferCdp
	preferCDPParams := &instanceoapi.ChromiumConfigureParams{ExtensionLoadStrategy: &preferCDP}

	t.Run("default restart", func(t *testing.T) {
		before, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		starts := chromiumConfigureStartCount(t, ctx, c)
		response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
			extensions: []configureExtensionPart{{name: "configure-default", zip: ordinaryZip}},
		})
		require.Equal(t, http.StatusOK, response.StatusCode(), "%s", response.Body)
		after, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		require.NotEqual(t, before, after)
		require.Equal(t, starts+1, chromiumConfigureStartCount(t, ctx, c))
		requireConfiguredExtensionActive(t, ctx, c, "configure-default")
	})

	t.Run("prefer CDP", func(t *testing.T) {
		before, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
			params:     preferCDPParams,
			extensions: []configureExtensionPart{{name: "configure-cdp", zip: ordinaryZip}},
		})
		require.Equal(t, http.StatusOK, response.StatusCode(), "%s", response.Body)
		after, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		require.Equal(t, before, after)
		requireConfiguredExtensionActive(t, ctx, c, "configure-cdp")
	})

	t.Run("prefer CDP with display", func(t *testing.T) {
		before, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		starts := chromiumConfigureStartCount(t, ctx, c)
		response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
			params:     preferCDPParams,
			extensions: []configureExtensionPart{{name: "configure-cdp-display", zip: ordinaryZip}},
			display:    `{"width":1280,"height":720,"refresh_rate":60,"restart_chromium":false,"require_idle":true}`,
		})
		require.Equal(t, http.StatusOK, response.StatusCode(), "%s", response.Body)
		after, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		require.Equal(t, before, after)
		require.Equal(t, starts, chromiumConfigureStartCount(t, ctx, c))
		requireConfiguredExtensionActive(t, ctx, c, "configure-cdp-display")
		waitForXRootResolution(t, ctx, c, 1280, 720, 10*time.Second)
	})

	t.Run("startup field restart", func(t *testing.T) {
		before, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		starts := chromiumConfigureStartCount(t, ctx, c)
		response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
			params:        preferCDPParams,
			extensions:    []configureExtensionPart{{name: "configure-startup", zip: ordinaryZip}},
			chromiumFlags: `{"flags":["--disable-background-networking"]}`,
		})
		require.Equal(t, http.StatusOK, response.StatusCode(), "%s", response.Body)
		after, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		require.NotEqual(t, before, after)
		require.Equal(t, starts+1, chromiumConfigureStartCount(t, ctx, c))
		requireConfiguredExtensionActive(t, ctx, c, "configure-startup")
	})

	t.Run("enterprise restart", func(t *testing.T) {
		before, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		starts := chromiumConfigureStartCount(t, ctx, c)
		response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
			params: preferCDPParams,
			extensions: []configureExtensionPart{
				{name: "configure-enterprise", zip: enterpriseZip},
				{name: "configure-enterprise-unpacked", zip: ordinaryZip},
			},
		})
		require.Equal(t, http.StatusOK, response.StatusCode(), "%s", response.Body)
		after, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		require.NotEqual(t, before, after)
		require.Equal(t, starts+1, chromiumConfigureStartCount(t, ctx, c))
		_, err = execCombinedOutputWithClient(ctx, c, "grep", []string{"-q", "configure-enterprise", "/etc/chromium/policies/managed/policy.json"})
		require.NoError(t, err)
		requireConfiguredExtensionActive(t, ctx, c, "configure-enterprise-unpacked")
	})

	t.Run("CDP failure restart", func(t *testing.T) {
		before, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		starts := chromiumConfigureStartCount(t, ctx, c)
		_, err = execCombinedOutputWithClient(ctx, c, "supervisorctl", []string{"-c", "/etc/supervisor/supervisord.conf", "stop", "chromium"})
		require.NoError(t, err)
		response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
			params: preferCDPParams,
			extensions: []configureExtensionPart{
				{name: "configure-fallback-one", zip: ordinaryZip},
				{name: "configure-fallback-two", zip: ordinaryZip},
			},
		})
		require.Equal(t, http.StatusOK, response.StatusCode(), "%s", response.Body)
		after, err := fetchBrowserWebSocketURL(ctx, c)
		require.NoError(t, err)
		require.NotEqual(t, before, after)
		require.Equal(t, starts+1, chromiumConfigureStartCount(t, ctx, c))
		requireConfiguredExtensionActive(t, ctx, c, "configure-fallback-one")
		requireConfiguredExtensionActive(t, ctx, c, "configure-fallback-two")
	})

	if testPartialFailure {
		// The malformed fixture prevents headful Chromium from becoming ready after restart.
		t.Run("partial CDP failure restart", func(t *testing.T) {
			before, err := fetchBrowserWebSocketURL(ctx, c)
			require.NoError(t, err)
			starts := chromiumConfigureStartCount(t, ctx, c)
			response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
				params: preferCDPParams,
				extensions: []configureExtensionPart{
					{name: "configure-partial-one", zip: ordinaryZip},
					{name: "configure-partial-invalid", zip: invalidZip},
				},
			})
			require.Equal(t, http.StatusInternalServerError, response.StatusCode(), "%s", response.Body)
			after, err := fetchBrowserWebSocketURL(ctx, c)
			require.NoError(t, err)
			require.NotEqual(t, before, after)
			require.Equal(t, starts+1, chromiumConfigureStartCount(t, ctx, c))
			requireConfiguredExtensionActive(t, ctx, c, "configure-partial-one")
			_, err = execCombinedOutputWithClient(ctx, c, "grep", []string{"-q", "loaded unpacked extension over CDP.*name=configure-partial-one", "/var/log/supervisord/kernel-images-api"})
			require.NoError(t, err)
		})
	}

	t.Run("invalid strategy", func(t *testing.T) {
		invalid := instanceoapi.ChromiumConfigureParamsExtensionLoadStrategy("invalid")
		response := chromiumConfigureE2E(t, ctx, client, configureE2ERequest{
			params:   &instanceoapi.ChromiumConfigureParams{ExtensionLoadStrategy: &invalid},
			startURL: "data:text/html,<title>invalid-strategy</title>",
		})
		require.Equal(t, http.StatusBadRequest, response.StatusCode(), "%s", response.Body)
	})
}

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
	startURL := `data:text/html,<title>kernel-configure</title>`
	require.NoError(t, w.WriteField("start_url", startURL))
	require.NoError(t, w.Close())

	rsp, err := client.ChromiumConfigureWithBodyWithResponse(ctx, nil, w.FormDataContentType(), io.NopCloser(&buf))
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, rsp.StatusCode(), "unexpected status=%s body=%s", rsp.Status(), string(rsp.Body))
	require.NotNil(t, rsp.JSON200, "want ok json")
	require.True(t, rsp.JSON200.Ok)

	require.Eventually(t, func() bool {
		timeoutSec := 3
		pwResp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightRequest{
			Code:       "return page.url();",
			TimeoutSec: &timeoutSec,
		})
		if err != nil || pwResp.JSON200 == nil || !pwResp.JSON200.Success {
			return false
		}
		got, ok := pwResp.JSON200.Result.(string)
		return ok && got == startURL
	}, 10*time.Second, 250*time.Millisecond)
}
