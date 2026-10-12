package cdpmonitor

import (
	"context"
	"encoding/json"

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
// Runtime domains on page and iframe sessions, for the rest of the monitor's
// lifetime. Disabling them would drop an extension's bindings and pause its
// new-document scripts. Once it returns, no disable is still in flight.
func (m *Monitor) RetainPageDomains() {
	m.pageDomainsMu.Lock()
	m.pageDomainsRetained = true
	m.pageDomainsMu.Unlock()
}
