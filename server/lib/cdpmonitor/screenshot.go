package cdpmonitor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/onkernel/kernel-images/server/lib/events"
)

// maybeScreenshot triggers a screenshot if the rate-limit window has elapsed.
// It uses an atomic CAS on lastScreenshotAt to ensure only one screenshot runs
// at a time.
func (m *Monitor) maybeScreenshot(ctx context.Context) {
	now := time.Now().UnixMilli()
	last := m.lastScreenshotAt.Load()
	if now-last < 2000 {
		return
	}
	if !m.lastScreenshotAt.CompareAndSwap(last, now) {
		return
	}
	go m.captureScreenshot(ctx)
}

// captureScreenshot takes a screenshot via ffmpeg x11grab (or the screenshotFn
// seam in tests), optionally downscales it, and publishes a screenshot event.
func (m *Monitor) captureScreenshot(ctx context.Context) {
	var pngBytes []byte
	var err error

	if m.screenshotFn != nil {
		pngBytes, err = m.screenshotFn(ctx, m.displayNum)
	} else {
		pngBytes, err = captureViaFFmpeg(ctx, m.displayNum, 1)
	}
	if err != nil {
		return
	}

	// Downscale if base64 output would exceed 950KB (~729KB raw).
	const rawThreshold = 729 * 1024
	for scale := 2; len(pngBytes) > rawThreshold && scale <= 16 && m.screenshotFn == nil; scale *= 2 {
		pngBytes, err = captureViaFFmpeg(ctx, m.displayNum, scale)
		if err != nil {
			return
		}
	}

	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	data, _ := json.Marshal(map[string]string{"png": encoded})

	m.publish(events.Event{
		Ts:          time.Now().UnixMilli(),
		Type:        "screenshot",
		Category:    events.CategorySystem,
		Source:      events.Source{Kind: events.KindLocalProcess},
		DetailLevel: events.DetailStandard,
		Data:        data,
	})
}

// captureViaFFmpeg runs ffmpeg x11grab to capture a PNG screenshot.
// If divisor > 1, a scale filter is applied to reduce the output size.
func captureViaFFmpeg(ctx context.Context, displayNum, divisor int) ([]byte, error) {
	args := []string{
		"-f", "x11grab",
		"-i", fmt.Sprintf(":%d", displayNum),
		"-vframes", "1",
	}
	if divisor > 1 {
		args = append(args, "-vf", fmt.Sprintf("scale=iw/%d:ih/%d", divisor, divisor))
	}
	args = append(args, "-f", "image2", "pipe:1")

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
