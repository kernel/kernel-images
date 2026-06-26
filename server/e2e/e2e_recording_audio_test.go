package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestReplayRecordingIncludesAudioTrack(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headfulImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"WIDTH":  "1280",
			"HEIGHT": "720",
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")
	require.NoError(t, c.WaitDevTools(ctx), "devtools not ready")

	// Verify the browser sees a real sound card over pure CDP/websocket. Chromium
	// excludes PulseAudio monitor sources from enumerateDevices(), so the
	// recorder's capture sink alone is invisible as an input. The standalone
	// null-source (KernelInput) is what makes a non-monitor microphone show up,
	// which antibot fingerprinting checks for.
	assertBrowserSeesAudioDevices(t, ctx, c)

	// Serve the tone fixture from inside the container as a file:// page. This
	// keeps the test self-contained instead of relying on host.docker.internal,
	// which is not routable in every sandbox.
	fixtureURL := writeContainerAudioFixture(t, ctx, c)

	playwrightCode := fmt.Sprintf(`
		await page.goto(%q, { waitUntil: 'load' });
		await page.click('#start');
		await page.waitForFunction(() => window.audioStarted === true);
		await page.waitForTimeout(8000);
		return await page.title();
	`, fixtureURL)

	recordReplayAudio(t, ctx, c, playwrightCode, os.Getenv("RECORDING_AUDIO_OUTPUT_PATH"), 0.1)
}

func TestReplayRecordingZombocomArchiveAudio(t *testing.T) {
	outputPath := os.Getenv("RECORDING_ZOMBO_OUTPUT_PATH")
	if outputPath == "" {
		t.Skip("set RECORDING_ZOMBO_OUTPUT_PATH to write a Zombocom archive recording")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headfulImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		Env: map[string]string{
			"WIDTH":  "1280",
			"HEIGHT": "720",
		},
	}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(t, c.WaitReady(ctx), "api not ready")

	playwrightCode := `
		await page.goto('https://archive.org/embed/ZombocomAkaZombo.com', { waitUntil: 'domcontentloaded' });
		await page.waitForSelector('play-av', { timeout: 30000 });

		const playbackState = () => page.evaluate(() => {
			const mediaElements = [];
			const collect = (root) => {
				mediaElements.push(...root.querySelectorAll('audio,video'));
				for (const el of root.querySelectorAll('*')) {
					if (el.shadowRoot) {
						collect(el.shadowRoot);
					}
				}
			};
			collect(document);
			return mediaElements.map((el) => ({
				currentTime: el.currentTime,
				paused: el.paused,
				readyState: el.readyState,
				src: el.currentSrc || el.src,
			}));
		});
		const isPlaying = async () => {
			const playback = await playbackState();
			return playback.some((media) => media.currentTime > 0.2 && !media.paused);
		};

		await page.waitForTimeout(2000);
		await page.waitForFunction(async () => {
			const player = document.querySelector('play-av');
			const video = player?.shadowRoot?.querySelector('video');
			return video && video.readyState >= 2;
		}, null, { timeout: 30000 });
		const playButton = await page.locator('play-av').evaluate((player) => {
			const button = player.shadowRoot?.querySelector('.jw-icon-playback');
			if (!button) {
				throw new Error('archive play button not found');
			}
			const rect = button.getBoundingClientRect();
			return {
				x: rect.left + rect.width / 2,
				y: rect.top + rect.height / 2,
			};
		});
		await page.mouse.click(playButton.x, playButton.y);
		await page.waitForTimeout(2000);
		if (!(await isPlaying())) {
			throw new Error('archive audio did not start after clicking play: ' + JSON.stringify(await playbackState()));
		}

		await page.waitForTimeout(16000);
		const playback = await playbackState();
		if (!playback.some((media) => media.currentTime > 8 && !media.paused)) {
			throw new Error('archive audio did not start: ' + JSON.stringify(playback));
		}
		return playback;
	`

	recordReplayAudio(t, ctx, c, playwrightCode, outputPath, 0.01)
}

func recordReplayAudio(t *testing.T, ctx context.Context, c *TestContainer, playwrightCode string, outputPath string, minPeakLevel float64) {
	t.Helper()

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Safety backstop only: each caller stops the recording explicitly once its
	// Playwright script finishes. Keep this above the longest script (the Zombocom
	// capture can run ~50s) so the cap never truncates the intended recording.
	maxDuration := 120
	maxFileSize := 100
	recordAudio := true
	startResp, err := client.StartRecordingWithResponse(ctx, instanceoapi.StartRecordingJSONRequestBody{
		MaxDurationInSeconds: &maxDuration,
		MaxFileSizeInMB:      &maxFileSize,
		RecordAudio:          &recordAudio,
	})
	require.NoError(t, err, "POST /recording/start failed")
	require.Equal(t, http.StatusCreated, startResp.StatusCode(), "unexpected start status: %s body=%s", startResp.Status(), string(startResp.Body))

	stopped := false
	defer func() {
		if !stopped {
			force := true
			_, _ = client.StopRecordingWithResponse(context.Background(), instanceoapi.StopRecordingJSONRequestBody{ForceStop: &force})
		}
	}()

	runResp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: playwrightCode,
	})
	require.NoError(t, err, "playwright request failed")
	require.Equal(t, http.StatusOK, runResp.StatusCode(), "unexpected playwright status: %s body=%s", runResp.Status(), string(runResp.Body))
	require.NotNil(t, runResp.JSON200, "expected playwright JSON response")
	if !runResp.JSON200.Success {
		t.Fatalf("playwright execution failed: error=%s stderr=%s result=%#v", stringValue(runResp.JSON200.Error), stringValue(runResp.JSON200.Stderr), runResp.JSON200.Result)
	}

	stopResp, err := client.StopRecordingWithResponse(ctx, instanceoapi.StopRecordingJSONRequestBody{})
	stopped = true
	require.NoError(t, err, "POST /recording/stop failed")
	require.Equal(t, http.StatusOK, stopResp.StatusCode(), "unexpected stop status: %s body=%s", stopResp.Status(), string(stopResp.Body))

	downloadResp, err := client.DownloadRecordingWithResponse(ctx, nil)
	require.NoError(t, err, "GET /recording/download failed")
	require.Equal(t, http.StatusOK, downloadResp.StatusCode(), "unexpected download status: %s body=%s", downloadResp.Status(), string(downloadResp.Body))
	require.NotEmpty(t, downloadResp.Body, "downloaded recording is empty")

	if outputPath != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(outputPath), 0o755), "failed to create recording output directory")
		require.NoError(t, os.WriteFile(outputPath, downloadResp.Body, 0o644), "failed to write downloaded recording")
	}

	recordingPath := writeContainerRecording(t, ctx, c, downloadResp.Body)
	require.True(t, mp4HasAudioTrack(downloadResp.Body), "downloaded recording does not contain an audio track")
	require.Greater(t, mp4AudioPeakLevel(t, ctx, c, recordingPath), minPeakLevel, "downloaded recording audio track is silent")
	formatDuration, audioDuration := mp4Durations(t, ctx, c, recordingPath)
	require.GreaterOrEqual(t, audioDuration, formatDuration-2, "downloaded recording audio track ends before the recording does")
}

type mediaDeviceInfo struct {
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	DeviceID string `json:"deviceId"`
}

// assertBrowserSeesAudioDevices connects to the browser over a raw CDP websocket
// and asserts navigator.mediaDevices.enumerateDevices() reports both an audio
// output and a non-monitor audio input. Chromium drops monitor sources from the
// input list, so a passing audioinput assertion confirms the KernelInput
// null-source (not just the KernelOutput sink monitor) is present.
func assertBrowserSeesAudioDevices(t *testing.T, ctx context.Context, c *TestContainer) {
	t.Helper()

	// navigator.mediaDevices is only exposed in a secure context. A file:// page
	// qualifies, so drop a minimal HTML file into the container and load it.
	const securePagePath = "/tmp/enumerate-devices.html"
	exitCode, out, err := c.Exec(ctx, []string{"sh", "-lc", "printf '%s' '<!doctype html><meta charset=utf-8><title>enumerate-devices</title>' > " + securePagePath})
	require.NoError(t, err, "failed to write secure-context page")
	require.Zero(t, exitCode, "failed to write secure-context page: %s", out)

	devices, err := enumerateMediaDevicesViaCDP(ctx, c.CDPURL(), "file://"+securePagePath)
	require.NoError(t, err, "failed to enumerate media devices via CDP")
	t.Logf("enumerateDevices reported %d devices: %+v", len(devices), devices)

	audioInputs := make([]mediaDeviceInfo, 0)
	audioOutputs := make([]mediaDeviceInfo, 0)
	for _, d := range devices {
		switch d.Kind {
		case "audioinput":
			audioInputs = append(audioInputs, d)
		case "audiooutput":
			audioOutputs = append(audioOutputs, d)
		}
	}

	// Chromium drops monitor sources from the input list, so any audioinput entry
	// here is necessarily the standalone KernelInput null-source, not the
	// KernelOutput sink monitor.
	require.NotEmpty(t, audioInputs, "expected at least one non-monitor audioinput device (KernelInput); Chromium filters monitor sources, so a missing entry means the null-source did not load")
	require.NotEmpty(t, audioOutputs, "expected at least one audiooutput device (KernelOutput)")
}

// enumerateMediaDevicesViaCDP opens a CDP target over the websocket proxy,
// navigates to pageURL (which must be a secure-context origin), and evaluates
// navigator.mediaDevices.enumerateDevices() inside the page.
func enumerateMediaDevicesViaCDP(ctx context.Context, wsURL, pageURL string) ([]mediaDeviceInfo, error) {
	client, err := newCDPClient(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	// Grant mic/camera access at the browser level so device labels are exposed.
	if _, err := client.Call(ctx, "Browser.grantPermissions", map[string]any{
		"permissions": []string{"audioCapture", "videoCapture"},
	}, ""); err != nil {
		return nil, fmt.Errorf("Browser.grantPermissions: %w", err)
	}

	targetRaw, err := client.Call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
	if err != nil {
		return nil, fmt.Errorf("Target.createTarget: %w", err)
	}
	targetID, err := decodeJSONStringField(targetRaw, "targetId")
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = client.Call(ctx, "Target.closeTarget", map[string]any{"targetId": targetID}, "")
	}()

	attachRaw, err := client.Call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, "")
	if err != nil {
		return nil, fmt.Errorf("Target.attachToTarget: %w", err)
	}
	sessionID, err := decodeJSONStringField(attachRaw, "sessionId")
	if err != nil {
		return nil, err
	}

	if _, err := client.Call(ctx, "Page.enable", map[string]any{}, sessionID); err != nil {
		return nil, fmt.Errorf("Page.enable: %w", err)
	}
	if _, err := client.Call(ctx, "Runtime.enable", map[string]any{}, sessionID); err != nil {
		return nil, fmt.Errorf("Runtime.enable: %w", err)
	}

	loadCtx, loadCancel := context.WithTimeout(ctx, 20*time.Second)
	defer loadCancel()
	loadDone := make(chan error, 1)
	go func() {
		loadDone <- client.WaitForEvent(loadCtx, "Page.loadEventFired", sessionID)
	}()
	if _, err := client.Call(ctx, "Page.navigate", map[string]any{
		"url": pageURL,
	}, sessionID); err != nil {
		return nil, fmt.Errorf("Page.navigate: %w", err)
	}
	if err := <-loadDone; err != nil {
		return nil, fmt.Errorf("waiting for page load: %w", err)
	}

	const expression = `(async () => {
  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) {
    return JSON.stringify({ error: 'mediaDevices unavailable', secureContext: window.isSecureContext });
  }
  const devices = await navigator.mediaDevices.enumerateDevices();
  return JSON.stringify({ devices: devices.map((d) => ({ kind: d.kind, label: d.label, deviceId: d.deviceId })) });
})()`

	evalRaw, err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, sessionID)
	if err != nil {
		return nil, fmt.Errorf("Runtime.evaluate: %w", err)
	}

	var evalEnvelope struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(evalRaw, &evalEnvelope); err != nil {
		return nil, fmt.Errorf("decode Runtime.evaluate result: %w", err)
	}
	if len(evalEnvelope.ExceptionDetails) > 0 {
		return nil, fmt.Errorf("enumerateDevices raised an exception: %s", string(evalEnvelope.ExceptionDetails))
	}

	var payload struct {
		Error         string            `json:"error"`
		SecureContext bool              `json:"secureContext"`
		Devices       []mediaDeviceInfo `json:"devices"`
	}
	if err := json.Unmarshal([]byte(evalEnvelope.Result.Value), &payload); err != nil {
		return nil, fmt.Errorf("decode enumerateDevices payload %q: %w", evalEnvelope.Result.Value, err)
	}
	if payload.Error != "" {
		return nil, fmt.Errorf("enumerateDevices failed: %s (secureContext=%t)", payload.Error, payload.SecureContext)
	}

	return payload.Devices, nil
}

const audioFixtureHTML = `<!doctype html>
<html>
<head><title>audio replay fixture</title></head>
<body>
<button id="start">start</button>
<script>
document.getElementById('start').addEventListener('click', async () => {
  const ctx = new AudioContext();
  await ctx.resume();
  const osc = ctx.createOscillator();
  const gain = ctx.createGain();
  gain.gain.value = 0.8;
  osc.frequency.value = 440;
  osc.connect(gain).connect(ctx.destination);
  osc.start();
  window.audioGraph = { ctx, osc, gain };
  window.audioStarted = true;
});
</script>
</body>
</html>`

// writeContainerAudioFixture writes the tone-playing HTML fixture into the
// container and returns a file:// URL for it. A file:// page is a secure
// context (AudioContext works) and avoids depending on host networking.
func writeContainerAudioFixture(t *testing.T, ctx context.Context, c *TestContainer) string {
	t.Helper()

	const fixturePath = "/tmp/audio-fixture.html"
	enc := base64.StdEncoding.EncodeToString([]byte(audioFixtureHTML))
	exitCode, out, err := c.Exec(ctx, []string{"sh", "-lc", fmt.Sprintf("echo %s | base64 -d > %s", enc, fixturePath)})
	require.NoError(t, err, "failed to write audio fixture")
	require.Zero(t, exitCode, "failed to write audio fixture: %s", out)
	return "file://" + fixturePath
}

func writeContainerRecording(t *testing.T, ctx context.Context, c *TestContainer, data []byte) string {
	t.Helper()

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	const recordingPath = "/tmp/e2e-recording-audio.mp4"
	params := &instanceoapi.WriteFileParams{Path: recordingPath}
	rsp, err := client.WriteFileWithBodyWithResponse(ctx, params, "video/mp4", bytes.NewReader(data))
	require.NoError(t, err, "write recording for audio analysis")
	require.Equal(t, http.StatusCreated, rsp.StatusCode(), "unexpected write status: %s body=%s", rsp.Status(), string(rsp.Body))
	return recordingPath
}

func mp4HasAudioTrack(data []byte) bool {
	for i := 0; i+16 <= len(data); i++ {
		if !bytes.Equal(data[i:i+4], []byte("hdlr")) {
			continue
		}
		end := i + 32
		if end > len(data) {
			end = len(data)
		}
		if bytes.Contains(data[i:end], []byte("soun")) {
			return true
		}
	}
	return false
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func mp4AudioPeakLevel(t *testing.T, ctx context.Context, c *TestContainer, recordingPath string) float64 {
	t.Helper()

	out, err := execCombinedOutput(
		ctx,
		c,
		"ffmpeg",
		[]string{
			"-hide_banner",
			"-i", recordingPath,
			"-map", "0:a:0",
			"-af", "astats=metadata=1:reset=0",
			"-f", "null",
			"-",
		},
	)
	require.NoError(t, err, "failed to analyze recording audio: %s", string(out))

	matches := regexp.MustCompile(`Max level: ([0-9.]+)`).FindStringSubmatch(string(out))
	require.Len(t, matches, 2, "failed to find audio peak level in ffmpeg output: %s", string(out))

	peak, err := strconv.ParseFloat(matches[1], 64)
	require.NoError(t, err, "failed to parse audio peak level")
	return peak
}

func mp4Durations(t *testing.T, ctx context.Context, c *TestContainer, recordingPath string) (float64, float64) {
	t.Helper()

	out, err := execCombinedOutput(
		ctx,
		c,
		"ffprobe",
		[]string{
			"-v", "error",
			"-show_entries", "format=duration",
			"-show_entries", "stream=codec_type,duration",
			"-of", "json",
			recordingPath,
		},
	)
	require.NoError(t, err, "failed to probe recording durations: %s", string(out))

	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Duration  string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &probe), "failed to parse ffprobe output")

	formatDuration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	require.NoError(t, err, "failed to parse format duration")

	for _, stream := range probe.Streams {
		if stream.CodecType != "audio" {
			continue
		}
		audioDuration, err := strconv.ParseFloat(stream.Duration, 64)
		require.NoError(t, err, "failed to parse audio duration")
		return formatDuration, audioDuration
	}
	t.Fatal("ffprobe did not report an audio stream")
	return 0, 0
}
