package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	instanceoapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestReplayRecordingIncludesAudioTrack(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	audioSite := newAudioTestSite(t)
	defer audioSite.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c := NewTestContainer(t, headfulImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{
		HostAccess: true,
		Env: map[string]string{
			"WIDTH":        "1280",
			"HEIGHT":       "720",
			"RECORD_AUDIO": "true",
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

	playwrightCode := fmt.Sprintf(`
		await page.goto(%q, { waitUntil: 'load' });
		await page.click('#start');
		await page.waitForFunction(() => window.audioStarted === true);
		await page.waitForTimeout(8000);
		return await page.title();
	`, audioSite.ContainerURL())

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
			"WIDTH":        "1280",
			"HEIGHT":       "720",
			"RECORD_AUDIO": "true",
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

	maxDuration := 35
	maxFileSize := 100
	startResp, err := client.StartRecordingWithResponse(ctx, instanceoapi.StartRecordingJSONRequestBody{
		MaxDurationInSeconds: &maxDuration,
		MaxFileSizeInMB:      &maxFileSize,
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

	require.True(t, mp4HasAudioTrack(downloadResp.Body), "downloaded recording does not contain an audio track")
	require.Greater(t, mp4AudioPeakLevel(t, downloadResp.Body), minPeakLevel, "downloaded recording audio track is silent")
	formatDuration, audioDuration := mp4Durations(t, downloadResp.Body)
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

	devices, err := enumerateMediaDevicesViaCDP(ctx, c.CDPURL())
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

	require.NotEmpty(t, audioInputs, "expected at least one audioinput device; Chromium filters monitor sources, so the KernelInput null-source must exist")
	require.NotEmpty(t, audioOutputs, "expected at least one audiooutput device (KernelOutput)")

	// When permissions reveal labels, confirm the input is our dedicated null
	// source rather than a leaked monitor.
	for _, d := range audioInputs {
		if strings.Contains(d.Label, "KernelInput") {
			return
		}
	}
	for _, d := range audioInputs {
		if d.Label != "" {
			t.Fatalf("expected a KernelInput audioinput device, got labeled inputs: %+v", audioInputs)
		}
	}
}

// enumerateMediaDevicesViaCDP opens a CDP target over the websocket proxy and
// evaluates navigator.mediaDevices.enumerateDevices() inside the page.
func enumerateMediaDevicesViaCDP(ctx context.Context, wsURL string) ([]mediaDeviceInfo, error) {
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

	if _, err := client.Call(ctx, "Runtime.enable", map[string]any{}, sessionID); err != nil {
		return nil, fmt.Errorf("Runtime.enable: %w", err)
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

type audioTestSite struct {
	*httptest.Server
}

func newAudioTestSite(t *testing.T) *audioTestSite {
	t.Helper()

	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err, "failed to listen for audio test site")

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
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
</html>`))
	}))
	srv.Listener = ln
	srv.Start()

	return &audioTestSite{Server: srv}
}

func (s *audioTestSite) ContainerURL() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		panic(err)
	}
	u.Host = net.JoinHostPort("host.docker.internal", u.Port())
	return u.String()
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

func mp4AudioPeakLevel(t *testing.T, data []byte) float64 {
	t.Helper()

	recordingPath := filepath.Join(t.TempDir(), "recording.mp4")
	require.NoError(t, os.WriteFile(recordingPath, data, 0o644), "failed to write recording for audio analysis")

	out, err := exec.Command(
		"docker", "run", "--rm",
		"-v", recordingPath+":/tmp/recording.mp4:ro",
		"--entrypoint", "ffmpeg",
		headfulImage,
		"-hide_banner",
		"-i", "/tmp/recording.mp4",
		"-map", "0:a:0",
		"-af", "astats=metadata=1:reset=0",
		"-f", "null",
		"-",
	).CombinedOutput()
	require.NoError(t, err, "failed to analyze recording audio: %s", string(out))

	matches := regexp.MustCompile(`Max level: ([0-9.]+)`).FindStringSubmatch(string(out))
	require.Len(t, matches, 2, "failed to find audio peak level in ffmpeg output: %s", string(out))

	peak, err := strconv.ParseFloat(matches[1], 64)
	require.NoError(t, err, "failed to parse audio peak level")
	return peak
}

func mp4Durations(t *testing.T, data []byte) (float64, float64) {
	t.Helper()

	recordingPath := filepath.Join(t.TempDir(), "recording.mp4")
	require.NoError(t, os.WriteFile(recordingPath, data, 0o644), "failed to write recording for duration analysis")

	out, err := exec.Command(
		"docker", "run", "--rm",
		"-v", recordingPath+":/tmp/recording.mp4:ro",
		"--entrypoint", "ffprobe",
		headfulImage,
		"-v", "error",
		"-show_entries", "format=duration",
		"-show_entries", "stream=codec_type,duration",
		"-of", "json",
		"/tmp/recording.mp4",
	).CombinedOutput()
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
	require.NoError(t, json.Unmarshal(out, &probe), "failed to parse ffprobe output")

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
