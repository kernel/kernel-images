package cdpmonitor

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// SetTelemetry reconciles optional capture on the existing connection. Network
// observation is never disabled. Callers still publish through TelemetrySession.
func (m *Monitor) SetTelemetry(enabled bool) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.restartMu.Lock()
	defer m.restartMu.Unlock()
	m.telemetryMu.RLock()
	unchanged := m.telemetryEnabled == enabled
	m.telemetryMu.RUnlock()
	if unchanged {
		return nil
	}
	m.telemetryChanging.Store(true)
	defer m.telemetryChanging.Store(false)
	m.telemetryMu.Lock()
	defer m.telemetryMu.Unlock()
	m.telemetryEnabled = enabled
	m.lifeMu.Lock()
	conn := m.conn
	m.lifeMu.Unlock()
	if !enabled {
		// Drain bounded CDP work before cancelling: cancelling a WebSocket write
		// can close the shared connection or lose a script-registration result.
		m.telemetryWg.Wait()
		if m.telemetryCancel != nil {
			m.telemetryCancel()
		}
		m.sessionsMu.Lock()
		states := m.computedStates
		m.computedStates = make(map[string]*computedState)
		scripts := m.optionalSessions
		m.optionalSessions = make(map[string]string)
		m.sessionsMu.Unlock()
		for _, state := range states {
			state.stop()
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
		if conn != nil && conn.ctx.Err() == nil {
			ctx, cancel := context.WithTimeout(conn.ctx, sendTimeout)
			defer cancel()
			var cleanupErr error
			for sessionID, scriptID := range scripts {
				cleanupErr = errors.Join(cleanupErr, m.disableOptionalDomains(ctx, sessionID, scriptID))
			}
			if cleanupErr != nil {
				// Attempt every session's cleanup before closing our connection to
				// remove any remaining subscriptions and retry in metrics-only mode.
				conn.cancel()
				return cleanupErr
			}
		}
		return nil
	}
	if conn == nil || conn.ctx.Err() != nil {
		return nil
	}
	m.telemetryCtx, m.telemetryCancel = context.WithCancel(conn.ctx)
	m.sessionsMu.RLock()
	sessions := make(map[string]targetInfo, len(m.sessions))
	for id, info := range m.sessions {
		sessions[id] = info
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
	if !exists || initialized || !m.telemetryEnabled || ctx.Err() != nil {
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
			// A timed-out registration may have installed a script without giving
			// us its removal ID. Replace this connection rather than retaining it.
			m.lifeMu.Lock()
			if m.conn != nil {
				m.conn.cancel()
			}
			m.lifeMu.Unlock()
		}
	}
}

func (m *Monitor) preparePageCapture(sessionID string, info targetInfo) {
	if !m.telemetryEnabled || info.targetType != targetTypePage {
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

// Runtime context bookkeeping is independent of optional event delivery, which
// can be paused during configuration. It lets cleanup reach same-process frames.
func (m *Monitor) trackExecutionContext(msg cdpMessage) {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()
	switch msg.Method {
	case "Runtime.executionContextCreated":
		var p struct {
			Context struct {
				ID int `json:"id"`
			} `json:"context"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.Context.ID != 0 {
			if m.contexts[msg.SessionID] == nil {
				m.contexts[msg.SessionID] = make(map[int]struct{})
			}
			m.contexts[msg.SessionID][p.Context.ID] = struct{}{}
		}
	case "Runtime.executionContextDestroyed":
		var p struct {
			ID int `json:"executionContextId"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			delete(m.contexts[msg.SessionID], p.ID)
		}
	case "Runtime.executionContextsCleared":
		delete(m.contexts, msg.SessionID)
	}
}
