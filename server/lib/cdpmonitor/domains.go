package cdpmonitor

import (
	"context"
	_ "embed"
)

// bindingName is the JS function exposed via Runtime.addBinding.
// Page JS calls this to fire Runtime.bindingCalled CDP events.
const bindingName = "__kernelEvent"

// enableDomains enables CDP domains, registers the event binding, and starts
// layout-shift observation. Failures are non-fatal.
func (m *Monitor) enableDomains(ctx context.Context, sessionID string) {
	for _, method := range []string{
		"Runtime.enable",
		"Network.enable",
		"Page.enable",
	} {
		if _, err := m.send(ctx, method, nil, sessionID); err != nil && ctx.Err() == nil {
			m.log.Warn("cdpmonitor: failed to enable CDP domain", "method", method, "session", sessionID, "err", err)
		}
	}

	if _, err := m.send(ctx, "Runtime.addBinding", map[string]any{
		"name": bindingName,
	}, sessionID); err != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to register JS binding", "session", sessionID, "err", err)
	}

	if _, err := m.send(ctx, "PerformanceTimeline.enable", map[string]any{
		"eventTypes": []string{timelineEventLayoutShift},
	}, sessionID); err != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to enable PerformanceTimeline", "session", sessionID, "err", err)
	}
}

// injectedJS tracks clicks, keys, and scrolls via the __kernelEvent binding.
// Layout shifts are handled natively by PerformanceTimeline.enable.
//
//go:embed interaction.js
var injectedJS string

// injectScript registers the interaction tracking JS for the given session.
func (m *Monitor) injectScript(ctx context.Context, sessionID string) error {
	_, err := m.send(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": injectedJS,
	}, sessionID)
	return err
}
