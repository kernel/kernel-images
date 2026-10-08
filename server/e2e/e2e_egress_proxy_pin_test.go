package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

const egressPinPath = "/etc/chromium/policies/managed/kernel-egress.json"

// TestEgressProxyPin checks that a filtered session pins Chromium's proxy with
// managed policy, so an extension loaded over CDP cannot take control of it,
// and that the pin comes off again when the session is unfiltered.
func TestEgressProxyPin(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		require.NoError(t, err, "docker not available: %v", err)
	}

	c := NewTestContainer(t, headlessImage)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	require.NoError(t, c.Start(ctx, ContainerConfig{}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	// Proxy v3 browsers are launched with the egress proxy on the command line.
	// Nothing listens on this one; the test never loads a page.
	flagsRsp, err := client.PatchChromiumFlagsWithResponse(ctx, instanceoapi.PatchChromiumFlagsJSONRequestBody{
		Flags: []string{"--proxy-server=http://127.0.0.1:9"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, flagsRsp.StatusCode(), "patch flags: %s", string(flagsRsp.Body))

	putEgressPolicy(t, ctx, client, true)

	out, err := execCombinedOutputWithClient(ctx, c, "cat", []string{egressPinPath})
	require.NoError(t, err, "read pin: %s", out)
	var pin struct {
		ProxySettings map[string]string `json:"ProxySettings"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &pin), "decode pin: %s", out)
	require.Equal(t, map[string]string{
		"ProxyMode":       "fixed_servers",
		"ProxyServer":     "http://127.0.0.1:9",
		"ProxyBypassList": "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7",
	}, pin.ProxySettings)

	// Loaded after the restart that applied the pin: extensions loaded over CDP
	// do not survive one. The connection stays open for the rest of the test
	// because an attached DevTools session keeps the service worker alive.
	cdp, err := cdpclient.Dial(ctx, c.CDPURL())
	require.NoError(t, err)
	defer cdp.Close()
	worker := attachProxyExtensionWorker(t, ctx, c, cdp)
	level, err := proxyLevelOfControl(ctx, cdp, worker)
	require.NoError(t, err)
	require.Equal(t, "not_controllable", level, "an extension can take over the proxy of a filtered session")

	putEgressPolicy(t, ctx, client, false)

	_, err = execCombinedOutputWithClient(ctx, c, "test", []string{"!", "-e", egressPinPath})
	require.NoError(t, err, "pin still present after the session was unfiltered")

	// Chromium reloads its policy directory on its own, so the extension
	// regains control without a restart.
	require.Eventually(t, func() bool {
		level, err := proxyLevelOfControl(ctx, cdp, worker)
		return err == nil && level == "controllable_by_this_extension"
	}, time.Minute, time.Second, "proxy still pinned after the session was unfiltered")
}

func putEgressPolicy(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, filtered bool) {
	t.Helper()
	rsp, err := client.PutNetworkEgressPolicyWithResponse(ctx, instanceoapi.PutNetworkEgressPolicyJSONRequestBody{Filtered: filtered})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "put egress policy filtered=%t: %s", filtered, string(rsp.Body))
}

// attachProxyExtensionWorker loads an extension with the proxy permission the
// way a CDP client can, and returns a session attached to its service worker.
func attachProxyExtensionWorker(t *testing.T, ctx context.Context, c *TestContainer, cdp *cdpclient.Client) string {
	t.Helper()
	const dir = "/tmp/egress-pin-ext"
	manifest := `{"manifest_version":3,"name":"proxy","version":"1.0","permissions":["proxy"],"background":{"service_worker":"bg.js"}}`
	script := fmt.Sprintf("mkdir -p %[1]s && printf '%%s' '%[2]s' > %[1]s/manifest.json && printf '' > %[1]s/bg.js && chmod -R a+rX %[1]s", dir, manifest)
	out, err := execCombinedOutputWithClient(ctx, c, "sh", []string{"-c", script})
	require.NoError(t, err, "write extension: %s", out)

	extensionID, err := cdp.LoadUnpackedExtension(ctx, dir)
	require.NoError(t, err)

	workerURL := "chrome-extension://" + extensionID + "/bg.js"
	var targetID string
	require.Eventually(t, func() bool {
		raw, err := cdp.Send(ctx, "Target.getTargets", nil, "")
		if err != nil {
			return false
		}
		var res struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		}
		if json.Unmarshal(raw, &res) != nil {
			return false
		}
		for _, info := range res.TargetInfos {
			if info.Type == "service_worker" && info.URL == workerURL {
				targetID = info.TargetID
				return true
			}
		}
		return false
	}, 30*time.Second, 250*time.Millisecond, "extension service worker never started")

	raw, err := cdp.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "")
	require.NoError(t, err)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(raw, &attached))
	return attached.SessionID
}

// proxyLevelOfControl asks the extension's service worker whether it can set
// Chromium's proxy.
func proxyLevelOfControl(ctx context.Context, cdp *cdpclient.Client, worker string) (string, error) {
	raw, err := cdp.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression":    `new Promise((resolve) => chrome.proxy.settings.get({}, (d) => resolve(d.levelOfControl)))`,
		"awaitPromise":  true,
		"returnByValue": true,
	}, worker)
	if err != nil {
		return "", err
	}
	var eval struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &eval); err != nil {
		return "", err
	}
	return eval.Result.Value, nil
}
