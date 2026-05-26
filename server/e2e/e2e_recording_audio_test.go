package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
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

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	maxDuration := 20
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

	playwrightCode := fmt.Sprintf(`
		await page.goto(%q, { waitUntil: 'load' });
		await page.click('#start');
		await page.waitForFunction(() => window.audioStarted === true);
		await page.waitForTimeout(3000);
		return await page.title();
	`, audioSite.ContainerURL())
	runResp, err := client.ExecutePlaywrightCodeWithResponse(ctx, instanceoapi.ExecutePlaywrightCodeJSONRequestBody{
		Code: playwrightCode,
	})
	require.NoError(t, err, "playwright request failed")
	require.Equal(t, http.StatusOK, runResp.StatusCode(), "unexpected playwright status: %s body=%s", runResp.Status(), string(runResp.Body))
	require.NotNil(t, runResp.JSON200, "expected playwright JSON response")
	require.True(t, runResp.JSON200.Success, "playwright execution failed: %#v", runResp.JSON200)

	stopResp, err := client.StopRecordingWithResponse(ctx, instanceoapi.StopRecordingJSONRequestBody{})
	stopped = true
	require.NoError(t, err, "POST /recording/stop failed")
	require.Equal(t, http.StatusOK, stopResp.StatusCode(), "unexpected stop status: %s body=%s", stopResp.Status(), string(stopResp.Body))

	downloadResp, err := client.DownloadRecordingWithResponse(ctx, nil)
	require.NoError(t, err, "GET /recording/download failed")
	require.Equal(t, http.StatusOK, downloadResp.StatusCode(), "unexpected download status: %s body=%s", downloadResp.Status(), string(downloadResp.Body))
	require.NotEmpty(t, downloadResp.Body, "downloaded recording is empty")
	require.True(t, mp4HasAudioTrack(downloadResp.Body), "downloaded recording does not contain an audio track")
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
  gain.gain.value = 0.2;
  osc.frequency.value = 440;
  osc.connect(gain).connect(ctx.destination);
  osc.start();
  window.audioContext = ctx;
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
