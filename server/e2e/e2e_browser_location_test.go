package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type browserLocationProbe struct {
	Language string `json:"language"`
	Locale   string `json:"locale"`
	TimeZone string `json:"timeZone"`
}

// Image names are pointers because init sets them after package variables.
var browserVariants = []struct {
	name  string
	image *string
}{
	{name: "headful", image: &headfulImage},
	{name: "headless", image: &headlessImage},
}

// TestRegionalBrowserTimezone starts a browser in Singapore time. The images
// ship tzdata but no generated regional POSIX locales, so guest processes use
// C.UTF-8 for UTF-8 text.
func TestRegionalBrowserTimezone(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	for _, variant := range browserVariants {
		t.Run(variant.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			c := NewTestContainer(t, *variant.image)
			require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
				"TZ":                      "Asia/Singapore",
				"KERNEL_BROWSER_TIMEZONE": "Asia/Singapore",
				"LANG":                    "C.UTF-8",
				"LC_ALL":                  "C.UTF-8",
				"CHROMIUM_FLAGS":          "--remote-allow-origins=*",
			}}), "failed to start container")
			defer c.Stop(ctx)

			require.NoError(t, c.WaitReady(ctx), "api not ready")
			require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

			// Read the system timezone from /etc/localtime, not the TZ variable.
			osLocation, err := execCombinedOutput(ctx, c, "sh", []string{"-c", `printf '%s|%s' "$(locale charmap)" "$(env -u TZ date +%z)"`})
			require.NoError(t, err, "failed to inspect OS locale and timezone")
			require.Equal(t, "UTF-8|+0800", osLocation)

			browserLocation, err := evaluateBrowserLocation(ctx, c.CDPURL())
			require.NoError(t, err)
			require.Equal(t, "Asia/Singapore", browserLocation.TimeZone)
		})
	}
}

// TestRegionalBrowserLocale checks that the browser, not guest POSIX locale
// data or copied resource packs, provides the regional locale: at startup from
// production's --lang flags while the regional LANG/LC_ALL locale is not
// installed, and at runtime from a native location update applied under
// C.UTF-8.
func TestRegionalBrowserLocale(t *testing.T) {
	requireKernelBrowser(t)
	singapore := browserLocationProbe{Language: "en-SG", Locale: "en-SG", TimeZone: "Asia/Singapore"}

	for _, variant := range browserVariants {
		t.Run(variant.name+"/startup", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			c := NewTestContainer(t, *variant.image)
			require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
				"TZ":                      "Asia/Singapore",
				"KERNEL_BROWSER_TIMEZONE": "Asia/Singapore",
				"LANG":                    "en_SG.UTF-8",
				"LC_ALL":                  "en_SG.UTF-8",
				"CHROMIUM_FLAGS":          "--lang=en-SG --accept-lang=en-SG,en --remote-allow-origins=*",
			}}), "failed to start container")
			defer c.Stop(ctx)
			require.NoError(t, c.WaitReady(ctx), "api not ready")
			require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

			installed, err := execCombinedOutput(ctx, c, "sh", []string{"-c", `locale -a | grep -ci '^en_sg' || true`})
			require.NoError(t, err)
			require.Equal(t, "0", strings.TrimSpace(installed), "the regional POSIX locale must be absent for this check to prove the browser does not use it")

			browserLocation, err := evaluateBrowserLocation(ctx, c.CDPURL())
			require.NoError(t, err)
			require.Equal(t, singapore, browserLocation)
		})

		t.Run(variant.name+"/native", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			c := NewTestContainer(t, *variant.image)
			require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
				"TZ":                      "America/Los_Angeles",
				"KERNEL_BROWSER_TIMEZONE": "America/Los_Angeles",
				"LANG":                    "C.UTF-8",
				"LC_ALL":                  "C.UTF-8",
				"CHROMIUM_FLAGS":          "--lang=en-US --accept-lang=en-US,en --remote-allow-origins=*",
				"KERNEL_INSTANCE_JWT":     locationInstanceToken,
			}}), "failed to start container")
			defer c.Stop(ctx)
			require.NoError(t, c.WaitReady(ctx), "api not ready")
			require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

			bundle := imageLocationBundle{Epoch: "lease-a", Generation: 1, TimeZone: "Asia/Singapore", Locale: "en-SG", Languages: []string{"en-SG", "en"}}
			resetLocation(ctx, t, c, "", bundle)
			waitLocationApplied(ctx, t, c, bundle)

			osLocation, err := execCombinedOutput(ctx, c, "sh", []string{"-c", `printf '%s|%s' "$(locale charmap)" "$(env -u TZ date +%z)"`})
			require.NoError(t, err, "failed to inspect OS locale and timezone")
			require.Equal(t, "UTF-8|+0800", osLocation)

			browserLocation, err := evaluateBrowserLocation(ctx, c.CDPURL())
			require.NoError(t, err)
			require.Equal(t, singapore, browserLocation)
		})
	}
}

func evaluateBrowserLocation(ctx context.Context, wsURL string) (browserLocationProbe, error) {
	client, err := newCDPClient(ctx, wsURL)
	if err != nil {
		return browserLocationProbe{}, err
	}
	defer client.Close()

	targetRaw, err := client.Call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
	if err != nil {
		return browserLocationProbe{}, fmt.Errorf("Target.createTarget: %w", err)
	}
	targetID, err := decodeJSONStringField(targetRaw, "targetId")
	if err != nil {
		return browserLocationProbe{}, err
	}
	defer func() {
		_, _ = client.Call(ctx, "Target.closeTarget", map[string]any{"targetId": targetID}, "")
	}()

	attachRaw, err := client.Call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, "")
	if err != nil {
		return browserLocationProbe{}, fmt.Errorf("Target.attachToTarget: %w", err)
	}
	sessionID, err := decodeJSONStringField(attachRaw, "sessionId")
	if err != nil {
		return browserLocationProbe{}, err
	}

	const expression = `JSON.stringify({
  language: navigator.language,
  locale: Intl.DateTimeFormat().resolvedOptions().locale,
  timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone
})`
	evalRaw, err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
	}, sessionID)
	if err != nil {
		return browserLocationProbe{}, fmt.Errorf("Runtime.evaluate: %w", err)
	}

	var evalEnvelope struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(evalRaw, &evalEnvelope); err != nil {
		return browserLocationProbe{}, fmt.Errorf("decode Runtime.evaluate result: %w", err)
	}
	if len(evalEnvelope.ExceptionDetails) > 0 {
		return browserLocationProbe{}, fmt.Errorf("browser location probe raised an exception: %s", evalEnvelope.ExceptionDetails)
	}

	var result browserLocationProbe
	if err := json.Unmarshal([]byte(evalEnvelope.Result.Value), &result); err != nil {
		return browserLocationProbe{}, fmt.Errorf("decode browser location probe %q: %w", evalEnvelope.Result.Value, err)
	}
	return result, nil
}
