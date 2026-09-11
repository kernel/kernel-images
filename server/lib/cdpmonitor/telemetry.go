package cdpmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

const telemetryCleanupTimeout = 3 * time.Second

// SetTelemetry commits and fences the desired capture revision without waiting
// for CDP. The API lifecycle worker owns draining, cleanup, and re-enabling.
func (m *Monitor) SetTelemetry(enabled bool) error {
	m.desiredMu.Lock()
	state := m.desiredTelemetry.Load()
	changed := (state&1 != 0) != enabled
	if changed {
		state = (state &^ 1) + 2
		if enabled {
			state |= 1
		}
		m.desiredTelemetry.Store(state)
	}
	m.desiredMu.Unlock()
	if changed {
		m.signalTelemetry()
	}
	return nil
}

func (m *Monitor) captureEnabled() bool {
	state := m.desiredTelemetry.Load()
	return state&1 != 0 && state == m.appliedTelemetry.Load()
}

func (m *Monitor) signalTelemetry() {
	select {
	case m.telemetryChanged <- struct{}{}:
	default:
	}
}

func (m *Monitor) reconcileTelemetry(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.telemetryChanged:
			m.restartMu.Lock()
			if ctx.Err() == nil {
				if err := m.applyTelemetry(m.desiredTelemetry.Load()); err != nil {
					m.log.Warn("cdpmonitor: telemetry cleanup failed; reconnecting", "err", err)
				}
			}
			m.restartMu.Unlock()
		}
	}
}

// restartMu excludes connection replacement. A revision change always drains the
// previous capture, including an off/on pair coalesced before this worker runs.
func (m *Monitor) applyTelemetry(state uint64) error {
	m.telemetryChanging.Store(true)
	defer m.telemetryChanging.Store(false)
	m.telemetryMu.Lock()
	defer m.telemetryMu.Unlock()
	if m.appliedTelemetry.Load() == state {
		return nil
	}
	m.telemetryEnabled = false
	// Do not cancel an in-flight WebSocket write or lose a registration's removal ID.
	// This bounded work runs outside the API lock; shutdown cancels its parent.
	m.telemetryWg.Wait()
	if m.telemetryCancel != nil {
		m.telemetryCancel()
	}
	m.sessionsMu.Lock()
	states := m.computedStates
	m.computedStates = make(map[string]*computedState)
	scripts := make(map[string]string, len(m.optionalSessions))
	for id, script := range m.optionalSessions {
		scripts[id] = script
	}
	m.sessionsMu.Unlock()
	for _, cs := range states {
		cs.stop()
	}
	m.computedPublishMu.Lock()
	m.computedPublishMu.Unlock()
	m.pendReqMu.Lock()
	clear(m.pendingRequests)
	m.pendReqMu.Unlock()
	m.mainSessionID.Store(mainSessionUnset)
	m.bindingRateMu.Lock()
	clear(m.bindingLastSeen)
	m.bindingRateMu.Unlock()
	m.proxyRateMu.Lock()
	clear(m.proxyLastEmit)
	m.proxyRateMu.Unlock()
	m.lifeMu.Lock()
	conn := m.conn
	m.lifeMu.Unlock()
	if conn != nil && conn.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(conn.ctx, telemetryCleanupTimeout)
		defer cancel()
		var cleanupErr error
		for sessionID, scriptID := range scripts {
			cleanupErr = errors.Join(cleanupErr, m.disableOptionalDomains(ctx, sessionID, scriptID))
		}
		m.sessionsMu.RLock()
		pending := len(m.interactionTargets) != 0
		m.sessionsMu.RUnlock()
		if cleanupErr == nil && pending {
			cleanupErr = errors.New("interaction cleanup requires target reattachment")
		}
		if cleanupErr != nil {
			// Target-scoped obligations survive this socket; new sessions retry cleanup.
			conn.cancel()
			return cleanupErr
		}
	}
	m.sessionsMu.Lock()
	clear(m.optionalSessions)
	m.sessionsMu.Unlock()
	// Do not resurrect an obsolete enabled revision after slow cleanup.
	if m.desiredTelemetry.Load() != state {
		m.signalTelemetry()
		return nil
	}
	if state&1 == 0 {
		m.appliedTelemetry.Store(state)
		return nil
	}
	if conn == nil || conn.ctx.Err() != nil {
		return nil // The replacement connection applies the latest desired revision.
	}
	m.telemetryCtx, m.telemetryCancel = context.WithCancel(conn.ctx)
	m.telemetryEnabled = true
	m.appliedTelemetry.Store(state)
	m.sessionsMu.RLock()
	sessions := make(map[string]targetInfo, len(m.sessions))
	for id, info := range m.sessions {
		// Pending attachments must finish orphan cleanup before optional setup.
		if m.networkReady[id] {
			sessions[id] = info
		}
	}
	m.sessionsMu.RUnlock()
	for id, info := range sessions {
		m.captureWg.Go(func() {
			m.telemetryMu.RLock()
			defer m.telemetryMu.RUnlock()
			m.enableOptionalCapture(m.telemetryCtx, id, info)
		})
	}
	return nil
}

// Caller holds telemetryMu; sessionsMu protects attachment/removal and the
// per-session initialization marker while optional setup runs asynchronously.
func (m *Monitor) enableOptionalCapture(ctx context.Context, sessionID string, info targetInfo) {
	m.sessionsMu.Lock()
	_, exists := m.sessions[sessionID]
	_, initialized := m.optionalSessions[sessionID]
	if !exists || initialized || !m.telemetryEnabled || !m.captureEnabled() || ctx.Err() != nil {
		m.sessionsMu.Unlock()
		return
	}
	m.optionalSessions[sessionID] = ""
	m.sessionsMu.Unlock()
	m.preparePageCapture(sessionID, info)
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	m.enableDomains(ctx, sessionID, info.targetType)
	if isPageLikeTarget(info.targetType) {
		if err := m.injectScript(ctx, sessionID); errors.Is(err, context.DeadlineExceeded) {
			m.lifeMu.Lock()
			if m.conn != nil {
				m.conn.cancel()
			}
			m.lifeMu.Unlock()
		}
	}
}

func (m *Monitor) preparePageCapture(sessionID string, info targetInfo) {
	if !m.telemetryEnabled || !m.captureEnabled() || info.targetType != targetTypePage {
		return
	}
	m.sessionsMu.Lock()
	if _, exists := m.sessions[sessionID]; !exists || m.computedStates[sessionID] != nil {
		m.sessionsMu.Unlock()
		return
	}
	ctx := m.telemetryCtx
	m.computedStates[sessionID] = newComputedState(func(ev events.Event) (events.Envelope, bool) {
		m.computedPublishMu.Lock()
		defer m.computedPublishMu.Unlock()
		if ctx.Err() != nil {
			return events.Envelope{}, false
		}
		return m.publish(ev)
	})
	m.sessionsMu.Unlock()
	data, _ := json.Marshal(oapi.BrowserPageTabOpenedEventData{
		TargetId: info.targetID, TargetType: oapi.BrowserTargetType(info.targetType),
		Url: info.url, Title: ptrOf(info.title), OpenerId: ptrOf(info.openerID),
	})
	m.publishEvent(EventTabOpened, events.Page, oapi.BrowserEventSource{Kind: oapi.Cdp}, "Target.attachedToTarget", data, sessionID)
}
