package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/kernel/kernel-images/server/lib/browserlocation"
	"github.com/stretchr/testify/require"
)

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
	Generation uint64   `json:"generation"`
	TimeZone   string   `json:"timezone"`
	Locale     string   `json:"locale"`
	Languages  []string `json:"languages"`
}

type imageLocationStatus struct {
	Accepted   *imageLocationBundle `json:"accepted"`
	Applied    *imageLocationBundle `json:"applied"`
	Components map[string]bool      `json:"components"`
	Error      string               `json:"error"`
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

	singapore := imageLocationBundle{Generation: 1, TimeZone: "Asia/Singapore", Locale: "en-SG", Languages: []string{"en-SG", "en"}}
	setLocation(ctx, t, c, singapore, http.StatusOK)
	waitLocationApplied(ctx, t, c, singapore)
	requireLocation(ctx, t, c, page, "Asia/Singapore", "en-SG")

	berlin := imageLocationBundle{Generation: 2, TimeZone: "Europe/Berlin", Locale: "de-DE", Languages: []string{"de-DE", "de"}}
	setLocation(ctx, t, c, berlin, http.StatusOK)
	waitLocationApplied(ctx, t, c, berlin)
	requireLocation(ctx, t, c, page, "Europe/Berlin", "de-DE")

	// Same-value retry and stale generation.
	setLocation(ctx, t, c, berlin, http.StatusOK)
	waitLocationApplied(ctx, t, c, berlin)
	setLocation(ctx, t, c, singapore, http.StatusConflict)

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

	// The restarted Chromium takes later generations as before.
	newYork := imageLocationBundle{Generation: 3, TimeZone: "America/New_York", Locale: "en-US", Languages: []string{"en-US", "en"}}
	setLocation(ctx, t, c, newYork, http.StatusOK)
	waitLocationApplied(ctx, t, c, newYork)
	requireLocation(ctx, t, c, restartedPage, "America/New_York", "en-US")
}

// timeZoneProbeScript reports the timezone the page and an already-running
// worker use, and the one the page's Intl resolves the requested identifier
// to, which is what a browser whose host timezone is that identifier reports.
const timeZoneProbeScript = `(async () => {
	if (!window.__zoneWorker) {
		window.__zoneWorker = new Worker(URL.createObjectURL(new Blob([
			'onmessage=()=>postMessage(Intl.DateTimeFormat().resolvedOptions().timeZone)'
		], {type: 'text/javascript'})));
	}
	const worker = await new Promise((resolve, reject) => {
		__zoneWorker.onmessage = event => resolve(event.data);
		__zoneWorker.onerror = reject;
		__zoneWorker.postMessage(null);
	});
	return JSON.stringify({
		expected: new Intl.DateTimeFormat('en', {timeZone: %q}).resolvedOptions().timeZone,
		page: Intl.DateTimeFormat().resolvedOptions().timeZone,
		worker,
	});
})()`

// TestBrowserLocationEveryTimeZone applies every timezone the capabilities
// list. tzdata ships many identifiers as links to other zones (UTC to
// Etc/UTC, Europe/Bratislava to Europe/Prague), and ICU names the host
// timezone after the file /etc/localtime resolves to, so each one must still
// be applied, which requires Chromium to report exactly that identifier, and
// the page and an existing worker must resolve it as Intl resolves it.
func TestBrowserLocationEveryTimeZone(t *testing.T) {
	requireKernelBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		"TZ":             "America/Los_Angeles",
		"CHROMIUM_FLAGS": "--remote-allow-origins=*",
	}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")
	page, err := openLocationPage(ctx, c.CDPURL())
	require.NoError(t, err)
	defer page.Close(ctx)

	started := time.Now()
	timezones := browserlocation.Current().TimeZones
	var mismatches []string
	for index, timezone := range timezones {
		bundle := imageLocationBundle{Generation: uint64(index + 1), TimeZone: timezone, Locale: "en-US", Languages: []string{"en-US", "en"}}
		setLocation(ctx, t, c, bundle, http.StatusOK)
		waitLocationApplied(ctx, t, c, bundle)
		var observed struct {
			Expected string `json:"expected"`
			Page     string `json:"page"`
			Worker   string `json:"worker"`
		}
		require.NoError(t, page.evalJSON(ctx, fmt.Sprintf(timeZoneProbeScript, timezone), &observed))
		if observed.Page != observed.Expected || observed.Worker != observed.Expected {
			mismatches = append(mismatches, fmt.Sprintf("%s: Intl %s, page %s, worker %s", timezone, observed.Expected, observed.Page, observed.Worker))
		}
	}
	t.Logf("applied %d timezones in %v", len(timezones), time.Since(started).Round(time.Second))
	require.Empty(t, mismatches, "pages must resolve each timezone as Intl resolves its identifier")
}

// TestBrowserLocationUnsupportedBrowser runs the image with a browser that
// lacks the location commands. The image must report it unsupported and
// refuse a location instead of accepting one it retries forever.
func TestBrowserLocationUnsupportedBrowser(t *testing.T) {
	if os.Getenv("E2E_KERNEL_BROWSER") == "1" {
		t.Skip("the image ships a browser with the location commands")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
		"TZ":             "America/Los_Angeles",
		"CHROMIUM_FLAGS": "--remote-allow-origins=*",
	}}), "failed to start container")
	defer c.Stop(ctx)
	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBaseURL()+"/browser/location", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode, string(body))

	singapore := imageLocationBundle{Generation: 1, TimeZone: "Asia/Singapore", Locale: "en-SG", Languages: []string{"en-SG", "en"}}
	setLocation(ctx, t, c, singapore, http.StatusNotImplemented)
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

func setLocation(ctx context.Context, t *testing.T, c *TestContainer, bundle imageLocationBundle, wantStatus int) {
	t.Helper()
	payload, err := json.Marshal(bundle)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.APIBaseURL()+"/browser/location", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
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
		if status.Applied != nil && status.Applied.Generation == bundle.Generation {
			for _, component := range []string{"timezone", "browser", "renderers", "network_contexts"} {
				require.True(t, status.Components[component], "applied without %s convergence: %+v", component, status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("browser location generation %d was not applied: %+v", bundle.Generation, status)
}
