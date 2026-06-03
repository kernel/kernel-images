package cdpmonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// tryScreenshot fires a screenshot if the 2s rate-limit window has elapsed.
// screenshotInFlight CAS is checked first so that a blocked attempt never
// consumes the rate-limit window without starting a capture. lastScreenshotAt
// is only advanced after the in-flight slot is claimed; if that CAS then loses
// to a concurrent goroutine the slot is released and we return cleanly.
// sourceEvent is the CDP event that triggered the capture; sessionID is used
// to snapshot nav context before the async goroutine fires.
func (m *Monitor) tryScreenshot(ctx context.Context, sourceEvent, sessionID string) {
	// Skip the ffmpeg capture entirely when the screenshot category is not
	// captured; otherwise the frame would be taken only to be dropped at the
	// telemetry filter.
	if m.screenshotEnabled != nil && !m.screenshotEnabled() {
		return
	}
	now := time.Now().UnixMilli()
	last := m.lastScreenshotAt.Load()
	if now-last < 2000 {
		return
	}
	if !m.screenshotInFlight.CompareAndSwap(false, true) {
		return
	}
	if !m.lastScreenshotAt.CompareAndSwap(last, now) {
		m.screenshotInFlight.Store(false)
		return
	}
	var navMeta map[string]string
	if cs := m.computedFor(sessionID); cs != nil {
		_, navMeta = cs.navSnapshot()
	}
	m.asyncWg.Go(func() {
		defer m.screenshotInFlight.Store(false)
		m.captureScreenshot(ctx, sourceEvent, navMeta)
	})
}

const screenshotTimeout = 10 * time.Second

// captureScreenshot takes a screenshot via ffmpeg x11grab (or the screenshotFn
// seam in tests), optionally downscales it, and publishes a screenshot event.
// navMeta is pre-snapped from the owning session's computedState; it may be nil
// if no state machine exists for the session.
func (m *Monitor) captureScreenshot(parentCtx context.Context, sourceEvent string, navMeta map[string]string) {
	ctx, cancel := context.WithTimeout(parentCtx, screenshotTimeout)
	defer cancel()
	var pngBytes []byte
	var err error

	if m.screenshotFn != nil {
		pngBytes, err = m.screenshotFn(ctx, m.displayNum)
	} else {
		pngBytes, err = captureViaFFmpeg(ctx, m.displayNum, 1)
	}
	if err != nil {
		m.log.Warn("cdpmonitor: screenshot capture failed", "err", err)
		return
	}

	// Downscale if base64 output would exceed ~972KB (~729KB raw × 4/3 base64 inflation).
	const rawThreshold = 729 * 1024
	for scale := 2; len(pngBytes) > rawThreshold && scale <= 16 && m.screenshotFn == nil; scale *= 2 {
		pngBytes, err = captureViaFFmpeg(ctx, m.displayNum, scale)
		if err != nil {
			m.log.Warn("cdpmonitor: screenshot downscale failed", "scale", scale, "err", err)
			return
		}
	}

	data, _ := json.Marshal(oapi.BrowserMonitorScreenshotEventData{
		Png: pngBytes,
	})

	src := oapi.BrowserEventSource{Kind: oapi.LocalProcess, Event: &sourceEvent}
	if navMeta != nil {
		src.Metadata = &navMeta
	}
	m.publish(events.Event{
		Ts:       time.Now().UnixMicro(),
		Type:     EventScreenshot,
		Category: events.Screenshot,
		Source:   src,
		Data:     data,
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

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out.Bytes(), nil
}
