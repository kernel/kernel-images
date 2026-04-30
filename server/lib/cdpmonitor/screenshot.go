package cdpmonitor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
)

// tryScreenshot fires a screenshot if the 2s rate-limit window has elapsed.
// screenshotInFlight CAS is checked first so that a blocked attempt never
// consumes the rate-limit window without starting a capture. lastScreenshotAt
// is only advanced after the in-flight slot is claimed; if that CAS then loses
// to a concurrent goroutine the slot is released and we return cleanly.
// sourceEvent is the CDP event that triggered the capture; sessionID is used
// to snapshot nav context before the async goroutine fires.
func (m *Monitor) tryScreenshot(ctx context.Context, sourceEvent, sessionID string) {
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
	var navData json.RawMessage
	var navMeta map[string]string
	if cs := m.computedFor(sessionID); cs != nil {
		navData, navMeta = cs.navSnapshot()
	}
	m.asyncWg.Go(func() {
		defer m.screenshotInFlight.Store(false)
		m.captureScreenshot(ctx, sourceEvent, navData, navMeta)
	})
}

const screenshotTimeout = 10 * time.Second

// captureScreenshot takes a screenshot via ffmpeg x11grab (or the screenshotFn
// seam in tests), optionally downscales it, and publishes a screenshot event.
// navData and navMeta are pre-snapped from the owning session's computedState;
// they may be nil if no state machine exists for the session.
func (m *Monitor) captureScreenshot(parentCtx context.Context, sourceEvent string, navData json.RawMessage, navMeta map[string]string) {
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

	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	payload := map[string]any{screenshotDataKey: encoded}
	if navData != nil {
		var nav map[string]any
		if json.Unmarshal(navData, &nav) == nil {
			maps.Copy(payload, nav)
		}
	}
	data, _ := json.Marshal(payload)

	m.publish(events.Event{
		Ts:       time.Now().UnixMicro(),
		Type:     EventScreenshot,
		Category: events.CategorySystem,
		Source:   events.Source{Kind: events.KindLocalProcess, Event: sourceEvent, Metadata: navMeta},
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
