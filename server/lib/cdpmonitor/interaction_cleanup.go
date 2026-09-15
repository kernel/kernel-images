package cdpmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

const cleanupInteractionJS = `window.__kernelEventCleanup && window.__kernelEventCleanup()`

// Registrations belong to a CDP session, but installed listeners belong to live
// documents. Target IDs survive socket replacement; session/context IDs do not.
func (m *Monitor) cleanupAttachedTarget(ctx context.Context, sessionID string, info targetInfo) error {
	m.sessionsMu.RLock()
	_, dirty := m.interactionTargets[info.targetID]
	m.sessionsMu.RUnlock()
	if !dirty {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, telemetryCleanupTimeout)
	defer cancel()
	if err := m.cleanupInteraction(ctx, sessionID); err != nil {
		m.sessionsMu.RLock()
		_, attached := m.sessions[sessionID]
		m.sessionsMu.RUnlock()
		if !attached {
			return nil
		}
		return err
	}
	m.sessionsMu.Lock()
	delete(m.interactionTargets, info.targetID)
	m.sessionsMu.Unlock()
	return nil
}

func (m *Monitor) cleanupInteraction(ctx context.Context, sessionID string) error {
	// runImmediately covers every existing frame's main world, including
	// cross-origin frames in the same renderer, without enabling Runtime/Page.
	raw, err := m.send(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": cleanupInteractionJS, "runImmediately": true,
	}, sessionID)
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
		return errors.New("cleanup script registration returned no identifier")
	}
	_, err = m.send(ctx, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": result.Identifier}, sessionID)
	return err
}

// Before starting discovery on a replacement connection, discard obligations
// only for targets confirmed gone (including targets from an old Chrome process).
func (m *Monitor) pruneInteractionTargets(ctx context.Context, protocol *cdpclient.Client) error {
	m.sessionsMu.RLock()
	dirty := len(m.interactionTargets) != 0
	m.sessionsMu.RUnlock()
	if !dirty {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := protocol.Send(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return err
	}
	var result struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	live := make(map[string]bool, len(result.TargetInfos))
	for _, target := range result.TargetInfos {
		live[target.TargetID] = true
	}
	m.sessionsMu.Lock()
	for target := range m.interactionTargets {
		if !live[target] {
			delete(m.interactionTargets, target)
		}
	}
	m.sessionsMu.Unlock()
	return nil
}
