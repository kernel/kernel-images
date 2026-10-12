package cdpmonitor

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

// Extension adds behavior to the monitor's sessions independently of
// telemetry, on every connection the monitor opens. Methods run on the
// connection's event loop and must not block.
type Extension interface {
	SessionAttached(Session)
	SessionRemoved(sessionID string)
	// ProtocolEvent receives every session-scoped CDP event, including those
	// telemetry ignores while capture is off.
	ProtocolEvent(sessionID, method string, params json.RawMessage)
	// Reset follows the loss of a connection: its sessions are gone.
	Reset()
}

// Session is an attached target on one monitor connection. Send stays bound
// to that connection, so it fails rather than reaching a later one.
type Session struct {
	ID            string
	TargetID      string
	TargetType    string
	ParentFrameID string
	protocol      *cdpclient.Client
}

func (s Session) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.protocol.Send(ctx, method, params, s.ID)
}

// SetExtension registers ext. Call it before Start.
func (m *Monitor) SetExtension(ext Extension) {
	m.extension = ext
}

// RetainPageDomains stops telemetry cleanup from disabling the Page and
// Runtime domains on page and iframe sessions until ReleasePageDomains.
// Disabling them would drop an extension's bindings and pause its
// new-document scripts. Once it returns, no disable is still in flight.
func (m *Monitor) RetainPageDomains() {
	m.pageDomainsMu.Lock()
	m.pageDomainsRetained = true
	m.pageDomainsMu.Unlock()
}

// ReleasePageDomains undoes RetainPageDomains and disables Page and Runtime on
// the page and iframe sessions telemetry is not capturing, leaving them as
// telemetry cleanup would have.
func (m *Monitor) ReleasePageDomains(ctx context.Context) error {
	// Excludes telemetry setup and cleanup, which decide what it owns.
	m.telemetryMu.Lock()
	defer m.telemetryMu.Unlock()
	m.pageDomainsMu.Lock()
	m.pageDomainsRetained = false
	m.pageDomainsMu.Unlock()
	m.sessionsMu.RLock()
	var idle []string
	for id, info := range m.sessions {
		if _, captured := m.optionalSessions[id]; !captured && isPageLikeTarget(info.targetType) {
			idle = append(idle, id)
		}
	}
	m.sessionsMu.RUnlock()
	var err error
	for _, id := range idle {
		for _, method := range []string{"Page.disable", "Runtime.disable"} {
			if _, sendErr := m.send(ctx, method, nil, id); sendErr != nil {
				err = errors.Join(err, sendErr)
			}
		}
	}
	return err
}
