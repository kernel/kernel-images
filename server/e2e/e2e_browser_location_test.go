package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type browserLocationProbe struct {
	Language string `json:"language"`
	Locale   string `json:"locale"`
	TimeZone string `json:"timeZone"`
}

func TestRegionalBrowserLocation(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	for _, test := range []struct {
		name  string
		image string
	}{
		{name: "headful", image: headfulImage},
		{name: "headless", image: headlessImage},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			c := NewTestContainer(t, test.image)
			require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
				"TZ":             "Asia/Singapore",
				"LANG":           "en_SG.UTF-8",
				"LC_ALL":         "en_SG.UTF-8",
				"CHROMIUM_FLAGS": "--lang=en-SG --accept-lang=en-SG,en --remote-allow-origins=*",
			}}), "failed to start container")
			defer c.Stop(ctx)

			require.NoError(t, c.WaitReady(ctx), "api not ready")
			require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

			osLocation, err := execCombinedOutput(ctx, c, "sh", []string{"-c", `printf '%s|%s' "$(locale charmap)" "$(date +%z)"`})
			require.NoError(t, err, "failed to inspect OS locale and timezone")
			require.Equal(t, "UTF-8|+0800", osLocation)

			browserLocation, err := evaluateBrowserLocation(ctx, c.CDPURL())
			require.NoError(t, err)
			require.Equal(t, browserLocationProbe{
				Language: "en-SG",
				Locale:   "en-SG",
				TimeZone: "Asia/Singapore",
			}, browserLocation)
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
