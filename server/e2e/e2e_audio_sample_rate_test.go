package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestBrowserAudioSampleRate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	for _, image := range []struct {
		name string
		tag  string
	}{
		{"headful", headfulImage},
		{"headless", headlessImage},
	} {
		t.Run(image.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			c := NewTestContainer(t, image.tag)
			require.NoError(t, c.Start(ctx, ContainerConfig{}))
			defer c.Stop(ctx)
			require.NoError(t, c.WaitReady(ctx))
			require.NoError(t, c.WaitDevTools(ctx))

			fixtureURL := writeContainerAudioFixture(t, ctx, c)
			client, err := c.APIClient()
			require.NoError(t, err)

			rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
				Code: fmt.Sprintf(`
					await page.goto(%q);
					await context.grantPermissions(['microphone']);
					return await page.evaluate(async () => {
						const defaultContext = new AudioContext();
						const explicit44100 = new AudioContext({ sampleRate: 44100 });
						const explicit48000 = new AudioContext({ sampleRate: 48000 });
						const stream = await navigator.mediaDevices.getUserMedia({
							audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false },
						});
						const rates = {
							defaultRate: defaultContext.sampleRate,
							explicit44100: explicit44100.sampleRate,
							explicit48000: explicit48000.sampleRate,
							microphoneRate: stream.getAudioTracks()[0].getSettings().sampleRate,
						};
						stream.getTracks().forEach(track => track.stop());
						await Promise.all([defaultContext.close(), explicit44100.close(), explicit48000.close()]);
						return rates;
					});
				`, fixtureURL),
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rsp.StatusCode(), "body=%s", rsp.Body)
			require.NotNil(t, rsp.JSON200)
			require.True(t, rsp.JSON200.Success, "body=%s", rsp.Body)

			resultBytes, err := json.Marshal(rsp.JSON200.Result)
			require.NoError(t, err)
			var rates struct {
				DefaultRate    int `json:"defaultRate"`
				Explicit44100  int `json:"explicit44100"`
				Explicit48000  int `json:"explicit48000"`
				MicrophoneRate int `json:"microphoneRate"`
			}
			require.NoError(t, json.Unmarshal(resultBytes, &rates))
			require.Equal(t, 48000, rates.DefaultRate, "default Web Audio sample rate")
			require.Equal(t, 44100, rates.Explicit44100, "explicit 44.1 kHz context")
			require.Equal(t, 48000, rates.Explicit48000, "explicit 48 kHz context")
			require.Equal(t, 48000, rates.MicrophoneRate, "raw microphone sample rate")
		})
	}
}
