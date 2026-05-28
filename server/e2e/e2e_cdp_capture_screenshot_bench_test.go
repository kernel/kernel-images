package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkCDPCaptureScreenshot measures Page.captureScreenshot latency through
// the CDP proxy. Run with:
// go test -run '^$' -bench BenchmarkCDPCaptureScreenshot -benchtime=25x -count=1 -v ./e2e
func BenchmarkCDPCaptureScreenshot(b *testing.B) {
	if _, err := exec.LookPath("docker"); err != nil {
		b.Skipf("docker not available: %v", err)
	}

	runCDPCaptureScreenshotBenchmark(b, headlessImage)
}

func runCDPCaptureScreenshotBenchmark(b *testing.B, image string) {
	b.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	c := NewTestContainer(b, image)
	require.NoError(b, c.Start(ctx, ContainerConfig{Env: env}), "failed to start container")
	defer c.Stop(ctx)

	require.NoError(b, c.WaitReady(ctx), "api not ready")
	require.NoError(b, c.WaitDevTools(ctx), "devtools not ready")

	client, targetID, sessionID, err := setupScreenshotTarget(ctx, c.CDPURL())
	require.NoError(b, err, "failed to set up CDP screenshot target")
	defer client.Close()
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_, _ = client.Call(closeCtx, "Target.closeTarget", map[string]any{"targetId": targetID}, "")
	}()

	warmupBytes, err := captureScreenshotBytes(ctx, client, sessionID)
	require.NoError(b, err, "warmup screenshot failed")
	require.Greater(b, warmupBytes, 0, "warmup screenshot returned no data")

	b.ReportAllocs()
	b.ResetTimer()

	var totalPayloadBytes int64
	for i := 0; i < b.N; i++ {
		iterCtx, iterCancel := context.WithTimeout(ctx, 15*time.Second)
		screenshotBytes, err := captureScreenshotBytes(iterCtx, client, sessionID)
		iterCancel()
		if err != nil {
			b.Fatalf("capture screenshot %d failed: %v", i, err)
		}
		totalPayloadBytes += int64(screenshotBytes)
	}

	b.StopTimer()

	if b.N > 0 {
		avgBytes := float64(totalPayloadBytes) / float64(b.N)
		b.ReportMetric(avgBytes, "screenshot_bytes/op")
		b.ReportMetric(avgBytes/(1024*1024)/b.Elapsed().Seconds()*float64(b.N), "screenshot_MiB/s")
		b.Logf("[summary] image=%s iterations=%d avg_screenshot_bytes=%.0f", image, b.N, avgBytes)
	}
}

func setupScreenshotTarget(ctx context.Context, wsURL string) (*cdpClient, string, string, error) {
	client, err := newCDPClient(ctx, wsURL)
	if err != nil {
		return nil, "", "", err
	}

	targetRaw, err := client.Call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, "")
	if err != nil {
		client.Close()
		return nil, "", "", fmt.Errorf("Target.createTarget: %w", err)
	}
	targetID, err := decodeJSONStringField(targetRaw, "targetId")
	if err != nil {
		client.Close()
		return nil, "", "", err
	}

	attachRaw, err := client.Call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, "")
	if err != nil {
		client.Close()
		return nil, "", "", fmt.Errorf("Target.attachToTarget: %w", err)
	}
	sessionID, err := decodeJSONStringField(attachRaw, "sessionId")
	if err != nil {
		client.Close()
		return nil, "", "", err
	}

	if _, err := client.Call(ctx, "Page.enable", map[string]any{}, sessionID); err != nil {
		client.Close()
		return nil, "", "", fmt.Errorf("Page.enable: %w", err)
	}
	if _, err := client.Call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             1024,
		"height":            768,
		"deviceScaleFactor": 1,
		"mobile":            false,
	}, sessionID); err != nil {
		client.Close()
		return nil, "", "", fmt.Errorf("Emulation.setDeviceMetricsOverride: %w", err)
	}

	loadCtx, loadCancel := context.WithTimeout(ctx, 15*time.Second)
	defer loadCancel()
	loadDone := make(chan error, 1)
	go func() {
		loadDone <- client.WaitForEvent(loadCtx, "Page.loadEventFired", sessionID)
	}()

	if _, err := client.Call(ctx, "Page.navigate", map[string]any{
		"url": "data:text/html," + url.PathEscape(screenshotBenchmarkHTML()),
	}, sessionID); err != nil {
		client.Close()
		return nil, "", "", fmt.Errorf("Page.navigate: %w", err)
	}
	if err := <-loadDone; err != nil {
		client.Close()
		return nil, "", "", fmt.Errorf("Page.loadEventFired: %w", err)
	}

	_, err = client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":   `document.fonts ? document.fonts.ready.then(() => true) : true`,
		"awaitPromise": true,
	}, sessionID)
	if err != nil {
		client.Close()
		return nil, "", "", fmt.Errorf("Runtime.evaluate: %w", err)
	}

	return client, targetID, sessionID, nil
}

func captureScreenshotBytes(ctx context.Context, client *cdpClient, sessionID string) (int, error) {
	screenshotRaw, err := client.Call(ctx, "Page.captureScreenshot", map[string]any{
		"format":                "png",
		"fromSurface":           true,
		"captureBeyondViewport": false,
	}, sessionID)
	if err != nil {
		return 0, err
	}

	var screenshotEnvelope struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(screenshotRaw, &screenshotEnvelope); err != nil {
		return 0, err
	}
	if screenshotEnvelope.Data == "" {
		return 0, fmt.Errorf("empty screenshot data")
	}

	return base64DecodedSize(screenshotEnvelope.Data), nil
}

func base64DecodedSize(s string) int {
	decodedLen := base64.StdEncoding.DecodedLen(len(s))
	switch {
	case strings.HasSuffix(s, "=="):
		return decodedLen - 2
	case strings.HasSuffix(s, "="):
		return decodedLen - 1
	default:
		return decodedLen
	}
}

func screenshotBenchmarkHTML() string {
	return `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<style>
html, body { margin: 0; width: 100%; height: 100%; overflow: hidden; }
canvas { display: block; width: 1024px; height: 768px; }
</style>
</head>
<body>
<canvas id="c" width="1024" height="768"></canvas>
<script>
const canvas = document.getElementById("c");
const ctx = canvas.getContext("2d");
const image = ctx.createImageData(canvas.width, canvas.height);
let state = 0x12345678;
for (let i = 0; i < image.data.length; i += 4) {
  state = (1664525 * state + 1013904223) >>> 0;
  image.data[i] = state & 255;
  image.data[i + 1] = (state >>> 8) & 255;
  image.data[i + 2] = (state >>> 16) & 255;
  image.data[i + 3] = 255;
}
ctx.putImageData(image, 0, 0);
</script>
</body>
</html>`
}
