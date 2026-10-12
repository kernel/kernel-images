package cdpmonitor

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

// bindingName is the JS function exposed via Runtime.addBinding.
// Page JS calls this to fire Runtime.bindingCalled CDP events.
const bindingName = "__kernelEvent"

// isPageLikeTarget reports whether the target type supports page-level CDP
// domains (Page.*, PerformanceTimeline.*, Page.addScriptToEvaluateOnNewDocument).
// Workers and service workers only support Runtime.* and Network.*.
func isPageLikeTarget(targetType string) bool {
	return targetType == "page" || targetType == "iframe"
}

// enableDomains enables optional telemetry domains, registers the event binding,
// and starts layout-shift observation. Failures are non-fatal.
// Page-level domains (Page.enable, PerformanceTimeline.enable, Runtime.addBinding)
// are skipped for worker and service_worker targets that don't support them.
func (m *Monitor) enableDomains(ctx context.Context, sessionID string, targetType string) {
	if _, err := m.send(ctx, "Runtime.enable", nil, sessionID); err != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to enable CDP domain", "method", "Runtime.enable", "session", sessionID, "err", err)
	}

	if !isPageLikeTarget(targetType) {
		return
	}

	if _, err := m.send(ctx, "Page.enable", nil, sessionID); err != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to enable CDP domain", "method", "Page.enable", "session", sessionID, "err", err)
	}

	// Inspector.targetCrashed reports a renderer crash on this session.
	if _, err := m.send(ctx, "Inspector.enable", nil, sessionID); err != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to enable CDP domain", "method", "Inspector.enable", "session", sessionID, "err", err)
	}

	if _, err := m.send(ctx, "Runtime.addBinding", map[string]any{
		"name": bindingName,
	}, sessionID); err != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to register JS binding", "session", sessionID, "err", err)
	}

	if _, err := m.send(ctx, "PerformanceTimeline.enable", map[string]any{
		"eventTypes": []string{timelineEventLayoutShift, timelineEventLCP},
	}, sessionID); err != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to enable PerformanceTimeline", "session", sessionID, "err", err)
	}
}

// injectedJS tracks clicks, keys, and scrolls via the __kernelEvent binding.
// Layout shifts are handled natively by PerformanceTimeline.enable.
//
//go:embed interaction.js
var injectedJS string

type interactionInjection struct {
	targetID  string
	destroyed bool // sessionsMu
}

// injectScript installs the interaction tracker for the session on both future
// document loads and the currently-loaded document, so a page that was already
// live when the session attached is tracked without waiting for a navigation.
// Idempotent for the current binding; replaces listeners from an old connection.
func (m *Monitor) injectScript(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.sessionsMu.Lock()
	info, exists := m.sessions[sessionID]
	injection := &interactionInjection{targetID: info.targetID}
	if exists {
		m.pendingInjections[injection] = struct{}{}
	}
	m.sessionsMu.Unlock()
	raw, err := m.send(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": injectedJS,
	}, sessionID)
	var rejection *cdpclient.Error
	rejected := errors.As(err, &rejection)
	m.sessionsMu.Lock()
	delete(m.pendingInjections, injection)
	destroyed := injection.destroyed
	// Detach alone preserves success/uncertain obligations; confirmed destruction
	// invalidates even a late result. Rejection never erases an earlier obligation.
	if exists && !destroyed && !rejected {
		m.interactionTargets[info.targetID] = struct{}{}
	}
	m.sessionsMu.Unlock()
	if destroyed {
		return nil
	}
	if err != nil {
		return err
	}
	var result struct {
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.Identifier == "" {
		return errors.New("script registration returned no identifier")
	}
	m.sessionsMu.Lock()
	if _, exists := m.optionalSessions[sessionID]; exists {
		m.optionalSessions[sessionID] = result.Identifier
	}
	m.sessionsMu.Unlock()
	// Some documents have no evaluable main-world context (e.g. chrome:// pages);
	// the registration above still applies to the next load. Surface anything else.
	if _, evalErr := m.send(ctx, "Runtime.evaluate", map[string]any{
		"expression": injectedJS,
	}, sessionID); evalErr != nil && ctx.Err() == nil {
		m.log.Warn("cdpmonitor: failed to inject interaction script into current document", "session", sessionID, "err", evalErr)
	}
	return nil
}

func (m *Monitor) disableOptionalDomains(ctx context.Context, sessionID, scriptID string) error {
	m.sessionsMu.RLock()
	info, exists := m.sessions[sessionID]
	_, dirty := m.interactionTargets[info.targetID]
	m.sessionsMu.RUnlock()
	if !exists {
		return nil
	}
	var cleanupErr error
	if scriptID != "" {
		_, cleanupErr = m.send(ctx, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": scriptID}, sessionID)
	}
	if isPageLikeTarget(info.targetType) {
		if dirty {
			cleanupErr = errors.Join(cleanupErr, m.cleanupInteraction(ctx, sessionID))
		}
		for _, command := range []struct {
			method string
			params any
		}{
			{"Runtime.removeBinding", map[string]any{"name": bindingName}},
			{"PerformanceTimeline.enable", map[string]any{"eventTypes": []string{}}},
			{"Inspector.disable", nil},
			{"Page.disable", nil},
		} {
			if _, err := m.send(ctx, command.method, command.params, sessionID); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
	}
	_, err := m.send(ctx, "Runtime.disable", nil, sessionID)
	cleanupErr = errors.Join(cleanupErr, err)
	if cleanupErr == nil {
		m.sessionsMu.Lock()
		delete(m.interactionTargets, info.targetID)
		delete(m.optionalSessions, sessionID)
		m.sessionsMu.Unlock()
	}
	return cleanupErr
}
