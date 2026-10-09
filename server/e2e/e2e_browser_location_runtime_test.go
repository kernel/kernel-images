package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
)

const locationInstanceToken = "e2e-instance-token"

// locationProbeScript starts a dedicated worker on first use and reports the
// page's and the worker's timezone, locale and language. The worker keeps
// running across updates so it is an already-running recipient.
const locationProbeScript = `(async () => {
	const probe = () => ({
		timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
		locale: Intl.DateTimeFormat().resolvedOptions().locale,
		language: navigator.language,
		offset: new Date().getTimezoneOffset(),
	});
	if (!window.__locationWorker) {
		window.__locationWorker = new Worker(URL.createObjectURL(new Blob([
			'onmessage=()=>postMessage({timeZone:Intl.DateTimeFormat().resolvedOptions().timeZone,locale:Intl.DateTimeFormat().resolvedOptions().locale,language:navigator.language,offset:new Date().getTimezoneOffset()})'
		], {type: 'text/javascript'})));
	}
	const worker = await new Promise((resolve, reject) => {
		__locationWorker.onmessage = event => resolve(event.data);
		__locationWorker.onerror = reject;
		__locationWorker.postMessage(null);
	});
	return JSON.stringify({page: probe(), worker});
})()`

type locationObservation struct {
	TimeZone string `json:"timeZone"`
	Locale   string `json:"locale"`
	Language string `json:"language"`
	Offset   int    `json:"offset"`
}

type locationProbeResult struct {
	Page   locationObservation `json:"page"`
	Worker locationObservation `json:"worker"`
}

type imageLocationBundle struct {
	Epoch      string   `json:"epoch"`
	Generation uint64   `json:"generation"`
	TimeZone   string   `json:"timezone"`
	Locale     string   `json:"locale"`
	Languages  []string `json:"languages"`
}

type imageLocationStatus struct {
	ActiveEpoch string               `json:"active_epoch"`
	Accepted    *imageLocationBundle `json:"accepted"`
	Applied     *imageLocationBundle `json:"applied"`
	Components  map[string]bool      `json:"components"`
	Error       string               `json:"error"`
}

// TestBrowserTimezoneFollowsLocaltime starts Chromium with the production
// environment and replaces /etc/localtime at runtime, the way the image applies
// a timezone: Chromium can miss the first replacement after it starts, so the
// image rewrites the same target until renderers observe it. The test does the
// same. It needs no Kernel CDP commands, so it runs on every image.
func TestBrowserTimezoneFollowsLocaltime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		"TZ":                      "America/Los_Angeles",
		"KERNEL_BROWSER_TIMEZONE": "America/Los_Angeles",
		"CHROMIUM_FLAGS":          "--remote-allow-origins=*",
	}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	pid, started := chromiumProcess(ctx, t, c)
	requireChromiumWithoutTZ(ctx, t, c, pid)

	page, err := openLocationPage(ctx, c.CDPURL())
	require.NoError(t, err)
	defer page.Close(ctx)
	waitTimeZone(ctx, t, page, "America/Los_Angeles", nil)

	// Includes a same-value change, which must leave the browser converged.
	for _, timezone := range []string{"Asia/Singapore", "Europe/Berlin", "Europe/Berlin", "America/New_York"} {
		replace := func() {
			_, err := execCombinedOutput(ctx, c, "sh", []string{"-c", fmt.Sprintf(`ln -s /usr/share/zoneinfo/%s /etc/.localtime-e2e && mv -fT /etc/.localtime-e2e /etc/localtime`, timezone)})
			require.NoError(t, err)
		}
		replace()
		waitTimeZone(ctx, t, page, timezone, replace)
	}

	restartedPID, restartedStart := chromiumProcess(ctx, t, c)
	require.Equal(t, pid, restartedPID, "timezone changes must not restart Chromium")
	require.Equal(t, started, restartedStart, "timezone changes must not restart Chromium")
}

func requireChromiumWithoutTZ(ctx context.Context, t *testing.T, c *TestContainer, pid string) {
	t.Helper()
	environment, err := execCombinedOutput(ctx, c, "sh", []string{"-c", fmt.Sprintf(`tr '\0' '\n' < /proc/%s/environ | grep '^TZ=' || true`, pid)})
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(environment), "Chromium must start without TZ so it watches /etc/localtime")
}

// waitTimeZone polls until the page and its already-running worker report
// timezone with the matching UTC offset, calling rewrite every 250ms if set.
func waitTimeZone(ctx context.Context, t *testing.T, page *locationPage, timezone string, rewrite func()) {
	t.Helper()
	zone, err := time.LoadLocation(timezone)
	require.NoError(t, err)
	_, offsetSeconds := time.Now().In(zone).Zone()
	deadline := time.Now().Add(10 * time.Second)
	started := time.Now()
	lastRewrite := started
	rewrites := 0
	var observed locationProbeResult
	for time.Now().Before(deadline) {
		require.NoError(t, page.evalJSON(ctx, locationProbeScript, &observed))
		if observed.Page.TimeZone == timezone && observed.Worker.TimeZone == timezone &&
			observed.Page.Offset == -offsetSeconds/60 && observed.Worker.Offset == -offsetSeconds/60 {
			t.Logf("%s converged in %v after %d rewrites", timezone, time.Since(started).Round(time.Millisecond), rewrites)
			return
		}
		if rewrite != nil && time.Since(lastRewrite) >= 250*time.Millisecond {
			rewrite()
			rewrites++
			lastRewrite = time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("browser did not follow /etc/localtime to %s: %+v", timezone, observed)
}

// TestBrowserLocationRuntimeUpdate starts Chromium with the production
// environment, which sets both TZ and KERNEL_BROWSER_TIMEZONE, and changes its
// timezone and locale at runtime without restarting it.
func TestBrowserLocationRuntimeUpdate(t *testing.T) {
	requireKernelBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		"TZ":                      "America/Los_Angeles",
		"KERNEL_BROWSER_TIMEZONE": "America/Los_Angeles",
		"LANG":                    "en_US.UTF-8",
		"LC_ALL":                  "en_US.UTF-8",
		"CHROMIUM_FLAGS":          "--lang=en-US --accept-lang=en-US,en --remote-allow-origins=*",
		"KERNEL_INSTANCE_JWT":     locationInstanceToken,
	}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	pid, started := chromiumProcess(ctx, t, c)
	requireChromiumWithoutTZ(ctx, t, c, pid)

	page, err := openLocationPage(ctx, c.CDPURL())
	require.NoError(t, err)
	defer page.Close(ctx)
	requireLocation(ctx, t, c, page, "America/Los_Angeles", "en-US")

	// A to B through the lease-reset endpoint, then B to C through configure.
	singapore := imageLocationBundle{Epoch: "lease-a", Generation: 1, TimeZone: "Asia/Singapore", Locale: "en-SG", Languages: []string{"en-SG", "en"}}
	resetLocation(ctx, t, c, "", singapore)
	waitLocationApplied(ctx, t, c, singapore)
	requireLocation(ctx, t, c, page, "Asia/Singapore", "en-SG")

	berlin := imageLocationBundle{Epoch: "lease-a", Generation: 2, TimeZone: "Europe/Berlin", Locale: "de-DE", Languages: []string{"de-DE", "de"}}
	configureLocation(ctx, t, c, berlin, http.StatusOK)
	waitLocationApplied(ctx, t, c, berlin)
	requireLocation(ctx, t, c, page, "Europe/Berlin", "de-DE")

	// Same-value retry and stale generation.
	configureLocation(ctx, t, c, berlin, http.StatusOK)
	waitLocationApplied(ctx, t, c, berlin)
	configureLocation(ctx, t, c, singapore, http.StatusConflict)

	restartedPID, restartedStart := chromiumProcess(ctx, t, c)
	require.Equal(t, pid, restartedPID, "location updates must not restart Chromium")
	require.Equal(t, started, restartedStart, "location updates must not restart Chromium")

	// A restarted Chromium starts from the launcher's startup timezone and must
	// converge back to the accepted bundle.
	_, err = execCombinedOutput(ctx, c, "supervisorctl", []string{"-c", "/etc/supervisor/supervisord.conf", "restart", "chromium"})
	require.NoError(t, err)
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready after restart")
	waitLocationApplied(ctx, t, c, berlin)
	restartedPage, err := openLocationPage(ctx, c.CDPURL())
	require.NoError(t, err)
	defer restartedPage.Close(ctx)
	requireLocation(ctx, t, c, restartedPage, "Europe/Berlin", "de-DE")

	// A new lease epoch restarts bundle generations; Chromium's keep increasing.
	newYork := imageLocationBundle{Epoch: "lease-b", Generation: 1, TimeZone: "America/New_York", Locale: "en-US", Languages: []string{"en-US", "en"}}
	resetLocation(ctx, t, c, "lease-a", newYork)
	waitLocationApplied(ctx, t, c, newYork)
	requireLocation(ctx, t, c, restartedPage, "America/New_York", "en-US")
}

// chromiumProcess returns the browser process ID and its start time in clock
// ticks since boot.
func chromiumProcess(ctx context.Context, t *testing.T, c *TestContainer) (string, string) {
	t.Helper()
	out, err := execCombinedOutput(ctx, c, "sh", []string{"-c", `pid=$(pgrep -o -x chromium || pgrep -o -x chrome) && echo "$pid $(cut -d' ' -f22 /proc/$pid/stat)"`})
	require.NoError(t, err)
	fields := strings.Fields(out)
	require.Len(t, fields, 2, "unexpected process output %q", out)
	return fields[0], fields[1]
}

// requireLocation checks the page, its already-running worker and the OS
// without polling: the image reports a bundle applied only after every
// existing recipient has observed it.
func requireLocation(ctx context.Context, t *testing.T, c *TestContainer, page *locationPage, timezone, locale string) {
	t.Helper()
	zone, err := time.LoadLocation(timezone)
	require.NoError(t, err)
	_, offsetSeconds := time.Now().In(zone).Zone()
	want := locationObservation{TimeZone: timezone, Locale: locale, Language: locale, Offset: -offsetSeconds / 60}

	var observed locationProbeResult
	require.NoError(t, page.evalJSON(ctx, locationProbeScript, &observed))
	require.Equal(t, want, observed.Page, "page")
	require.Equal(t, want, observed.Worker, "existing worker")

	// The container environment still carries the startup TZ, so read the
	// system timezone without it.
	date, err := execCombinedOutput(ctx, c, "env", []string{"-u", "TZ", "date", "+%z"})
	require.NoError(t, err)
	require.Equal(t, time.Now().In(zone).Format("-0700"), strings.TrimSpace(date))
}

func resetLocation(ctx context.Context, t *testing.T, c *TestContainer, previousEpoch string, bundle imageLocationBundle) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"previous_epoch": previousEpoch, "bundle": bundle})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBaseURL()+"/internal/browser-location/reset", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+locationInstanceToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(respBody))
}

func configureLocation(ctx context.Context, t *testing.T, c *TestContainer, bundle imageLocationBundle, wantStatus int) {
	t.Helper()
	payload, err := json.Marshal(bundle)
	require.NoError(t, err)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("browser_location", string(payload)))
	require.NoError(t, writer.Close())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBaseURL()+"/configure", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	require.Equal(t, wantStatus, resp.StatusCode, string(respBody))
}

func waitLocationApplied(ctx context.Context, t *testing.T, c *TestContainer, bundle imageLocationBundle) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var status imageLocationStatus
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBaseURL()+"/browser/location", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		err = json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		require.NoError(t, err)
		if status.Applied != nil && status.Applied.Epoch == bundle.Epoch && status.Applied.Generation == bundle.Generation {
			for _, component := range []string{"timezone", "browser", "renderers", "network_contexts"} {
				require.True(t, status.Components[component], "applied without %s convergence: %+v", component, status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("browser location %s/%d was not applied: %+v", bundle.Epoch, bundle.Generation, status)
}
