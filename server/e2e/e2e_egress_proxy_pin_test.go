package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/egresspolicy"
	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// TestEgressProxyPin checks that a filtered session pins Chromium's proxy and
// WebRTC IP handling with managed policy, so that neither an extension loaded
// over CDP nor a runtime flag or chrome policy written through the instance API
// can take control of either, and that the pin comes off again when the session
// is unfiltered.
func TestEgressProxyPin(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	c := NewTestContainer(t, headlessImage)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Proxy v3 browsers are launched with the egress proxy in their base flags.
	// Nothing listens on this one; the test only loads loopback pages, which
	// Chromium never sends through a proxy.
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{"CHROMIUM_FLAGS": "--proxy-server=http://127.0.0.1:9"}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	client, err := c.APIClient()
	require.NoError(t, err)

	putEgressPolicy(t, ctx, client, true)

	// Anyone holding the session's token can write runtime flags and chrome
	// policy. Neither may undo the pin: each write restarts Chromium, and the
	// launcher pins the base flags' proxy and clears per-URL WebRTC rules.
	policyRsp, err := client.PatchChromiumPoliciesWithResponse(ctx, instanceoapi.PatchChromiumPoliciesJSONRequestBody{
		"WebRtcIPHandlingUrl": []map[string]string{{"url": "*", "handling": "default"}},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, policyRsp.StatusCode(), "patch policies: %s", string(policyRsp.Body))
	flagsRsp, err := client.PatchChromiumFlagsWithResponse(ctx, instanceoapi.PatchChromiumFlagsJSONRequestBody{
		Flags: []string{"--proxy-server=http://127.0.0.1:8"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, flagsRsp.StatusCode(), "patch flags: %s", string(flagsRsp.Body))

	out, err := execCombinedOutputWithClient(ctx, c, "cat", []string{egresspolicy.DefaultPinPath})
	require.NoError(t, err, "read pin: %s", out)
	var pin struct {
		ProxySettings       map[string]string `json:"ProxySettings"`
		WebRtcIPHandling    string            `json:"WebRtcIPHandling"`
		WebRtcIPHandlingURL []any             `json:"WebRtcIPHandlingUrl"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &pin), "decode pin: %s", out)
	require.Equal(t, map[string]string{
		"ProxyMode":       "fixed_servers",
		"ProxyServer":     "http://127.0.0.1:9",
		"ProxyBypassList": "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16;100.64.0.0/10;fc00::/7",
	}, pin.ProxySettings)
	require.Equal(t, "disable_non_proxied_udp", pin.WebRtcIPHandling)
	require.NotNil(t, pin.WebRtcIPHandlingURL, "pin does not clear per-URL WebRTC rules: %s", out)
	require.Empty(t, pin.WebRtcIPHandlingURL, "pin does not clear per-URL WebRTC rules: %s", out)

	// Loaded after the restarts above: extensions loaded over CDP do not
	// survive one. The connection stays open for the rest of the test because
	// an attached DevTools session keeps the service worker alive.
	cdp, err := cdpclient.Dial(ctx, c.CDPURL())
	require.NoError(t, err)
	defer cdp.Close()
	worker := attachProxyExtensionWorker(t, ctx, c, cdp)
	level, err := levelOfControl(ctx, cdp, worker, proxySetting)
	require.NoError(t, err)
	require.Equal(t, "not_controllable", level, "an extension can take over the proxy of a filtered session")
	level, err = levelOfControl(ctx, cdp, worker, webRTCSetting)
	require.NoError(t, err)
	require.Equal(t, "not_controllable", level, "an extension can let WebRTC send UDP around the proxy of a filtered session")
	page := attachLoopbackPage(t, ctx, cdp)
	udp, err := udpCandidates(ctx, cdp, page)
	require.NoError(t, err)
	require.Empty(t, udp, "WebRTC gathered UDP candidates on a filtered session")

	waitForPolicyWatch(t, ctx, c)
	putEgressPolicy(t, ctx, client, false)

	_, err = execCombinedOutputWithClient(ctx, c, "test", []string{"!", "-e", egresspolicy.DefaultPinPath})
	require.NoError(t, err, "pin still present after the session was unfiltered")

	// Chromium reloads its policy directory on its own, so the extension
	// regains control without a restart.
	require.Eventually(t, func() bool {
		proxy, err := levelOfControl(ctx, cdp, worker, proxySetting)
		if err != nil {
			return false
		}
		webRTC, err := levelOfControl(ctx, cdp, worker, webRTCSetting)
		return err == nil && proxy == "controllable_by_this_extension" && webRTC == "controllable_by_this_extension"
	}, time.Minute, time.Second, "proxy still pinned after the session was unfiltered")
	// The customer's per-URL rule applies again, which also shows the page can
	// gather UDP candidates when nothing stops it.
	require.Eventually(t, func() bool {
		udp, err := udpCandidates(ctx, cdp, page)
		return err == nil && len(udp) > 0
	}, time.Minute, time.Second, "per-URL WebRTC rule still cleared after the session was unfiltered")
}

func putEgressPolicy(t *testing.T, ctx context.Context, client *instanceoapi.ClientWithResponses, filtered bool) {
	t.Helper()
	rsp, err := client.PutNetworkEgressPolicyWithResponse(ctx, instanceoapi.PutNetworkEgressPolicyJSONRequestBody{Filtered: filtered})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rsp.StatusCode(), "put egress policy filtered=%t: %s", filtered, string(rsp.Body))
}

// attachProxyExtensionWorker loads an extension with the proxy and privacy
// permissions the way a CDP client can, and returns a session attached to its service worker.
func attachProxyExtensionWorker(t *testing.T, ctx context.Context, c *TestContainer, cdp *cdpclient.Client) string {
	t.Helper()
	const dir = "/tmp/egress-pin-ext"
	manifest := `{"manifest_version":3,"name":"proxy","version":"1.0","permissions":["proxy","privacy"],"background":{"service_worker":"bg.js"}}`
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

// waitForPolicyWatch waits until Chromium watches its managed policy
// directory. It installs the watch in a best-effort task some time after it
// starts, and a pin removed before then goes unnoticed until the periodic
// policy reload 15 minutes later: Chromium finds the change when it installs
// the watch, but the periodic reload it schedules next replaces the one it
// scheduled for the change.
func waitForPolicyWatch(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()
	const script = `ino=$(printf '%x' "$(stat -c %i /etc/chromium/policies/managed)")
browser=$(for p in $(pgrep -f -- --remote-debugging-port); do grep -qa -- --type= /proc/$p/cmdline || echo $p; done | sort -n | tail -n 1)
grep -qs "ino:$ino " /proc/$browser/fdinfo/*`
	require.Eventually(t, func() bool {
		_, err := execCombinedOutputWithClient(ctx, c, "sh", []string{"-c", script})
		return err == nil
	}, time.Minute, 500*time.Millisecond, "chromium never started watching its policy directory")
}

// attachLoopbackPage opens a page on the DevTools HTTP endpoint, which needs no
// network and is never proxied, and returns a session attached to it.
func attachLoopbackPage(t *testing.T, ctx context.Context, cdp *cdpclient.Client) string {
	t.Helper()
	raw, err := cdp.Send(ctx, "Target.createTarget", map[string]any{"url": "http://127.0.0.1:9223/json/version"}, "")
	require.NoError(t, err)
	var created struct {
		TargetID string `json:"targetId"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	raw, err = cdp.Send(ctx, "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, "")
	require.NoError(t, err)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(raw, &attached))
	return attached.SessionID
}

// udpCandidates gathers ICE candidates in the page and returns the UDP ones. No
// STUN server is configured: a host candidate is enough to show WebRTC may send
// UDP directly.
func udpCandidates(ctx context.Context, cdp *cdpclient.Client, page string) ([]string, error) {
	raw, err := cdp.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression": `new Promise((resolve) => {
  const pc = new RTCPeerConnection();
  const candidates = [];
  const done = () => { pc.close(); resolve(candidates); };
  pc.onicecandidate = (e) => e.candidate ? candidates.push(e.candidate.candidate) : done();
  pc.createDataChannel('probe');
  pc.createOffer().then((offer) => pc.setLocalDescription(offer));
  setTimeout(done, 5000);
})`,
		"awaitPromise":  true,
		"returnByValue": true,
	}, page)
	if err != nil {
		return nil, err
	}
	var eval struct {
		Result struct {
			Value []string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &eval); err != nil {
		return nil, err
	}
	var udp []string
	for _, candidate := range eval.Result.Value {
		if strings.Contains(strings.ToLower(candidate), " udp ") {
			udp = append(udp, candidate)
		}
	}
	return udp, nil
}

// The extension settings the pin takes away from extensions.
const (
	proxySetting  = "chrome.proxy.settings"
	webRTCSetting = "chrome.privacy.network.webRTCIPHandlingPolicy"
)

// levelOfControl asks the extension's service worker whether it can change a
// Chromium setting.
func levelOfControl(ctx context.Context, cdp *cdpclient.Client, worker, setting string) (string, error) {
	raw, err := cdp.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression":    `new Promise((resolve) => ` + setting + `.get({}, (d) => resolve(d.levelOfControl)))`,
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
