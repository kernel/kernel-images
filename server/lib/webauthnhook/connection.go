package webauthnhook

import (
	"context"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

const (
	dialTimeout    = 5 * time.Second
	probeInterval  = 5 * time.Second
	probeTimeout   = 5 * time.Second
	reconnectMin   = 250 * time.Millisecond
	reconnectMax   = 5 * time.Second
	installWait    = 15 * time.Second
	installPollGap = 50 * time.Millisecond
)

// connection is one DevTools connection and the page and iframe targets it
// has attached. Its sessions, bindings, and scripts end with it.
type connection struct {
	protocol *cdpclient.Client
	surface  *browsersurface.Tracker
	done     chan struct{} // closed once its events are consumed
}

// run keeps a connection open until ctx ends, replacing it after socket loss,
// a failed probe, or a new upstream URL (Chromium restarted).
func (h *Hook) run(ctx context.Context) {
	updates, unsubscribe := h.upstream.Subscribe()
	defer unsubscribe()
	backoff := reconnectMin
	for {
		if url := h.upstream.Current(); url != "" {
			conn, err := h.connect(ctx, url)
			if err == nil {
				backoff = reconnectMin
				h.serve(ctx, conn, updates)
				h.disconnect(conn)
			} else if ctx.Err() == nil {
				h.log.Warn("webauthn: failed to connect to Chromium", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-updates:
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, reconnectMax)
	}
}

func (h *Hook) connect(ctx context.Context, url string) (*connection, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	protocol, err := cdpclient.DialWithEvents(dialCtx, url)
	if err != nil {
		return nil, err
	}
	conn := &connection{
		protocol: protocol,
		surface:  browsersurface.New(protocol, browsersurface.WithoutLocations()),
		done:     make(chan struct{}),
	}
	events, unsubscribe := conn.surface.Subscribe()
	go func() {
		defer close(conn.done)
		defer unsubscribe()
		for event := range events {
			h.handleEvent(conn, event)
		}
	}()
	startCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	if err := conn.surface.Start(startCtx); err != nil {
		h.disconnect(conn)
		return nil, err
	}
	h.mu.Lock()
	h.conn = conn
	h.mu.Unlock()
	return conn, nil
}

func (h *Hook) serve(ctx context.Context, conn *connection, updates <-chan string) {
	probe := time.NewTicker(probeInterval)
	defer probe.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.protocol.Done():
			h.log.Warn("webauthn: DevTools connection lost; reconnecting")
			return
		case <-updates:
			return
		case <-probe.C:
			// A bounded round trip also catches half-open sockets after suspend.
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			_, err := conn.protocol.GetBrowserVersion(probeCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				h.log.Warn("webauthn: DevTools connection unresponsive; reconnecting", "err", err)
				return
			}
		}
	}
}

// disconnect closes conn and forgets its sessions. Unanswered requests are
// cancelled: their documents resend to the next connection.
func (h *Hook) disconnect(conn *connection) {
	_ = conn.protocol.Close()
	<-conn.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == conn {
		h.conn = nil
	}
	for _, s := range h.sessions {
		for _, c := range s.calls {
			if c.reply == nil {
				c.cancel()
			}
		}
	}
	clear(h.sessions)
}

func (h *Hook) handleEvent(conn *connection, event browsersurface.Event) {
	switch event.Kind {
	case browsersurface.EventSessionAttached:
		if event.Target.Type != "page" && event.Target.Type != "iframe" {
			return
		}
		go h.reconcile(h.track(&session{
			id: event.SessionID, targetID: event.Target.ID, targetType: event.Target.Type,
			parentFrameID: event.Target.ParentFrameID, cdp: cdpSession{protocol: conn.protocol, id: event.SessionID},
		}))
	case browsersurface.EventSessionRemoved:
		h.mu.Lock()
		delete(h.sessions, event.SessionID)
		h.mu.Unlock()
	case browsersurface.EventProtocol:
		if event.Message.SessionID != "" {
			h.protocolEvent(event.Message.SessionID, event.Message.Method, event.Message.Params)
		}
	}
}

// waitInstalled waits until every page and iframe open when the connection
// started has had its first setup attempt, so a caller that turns the hook on
// can rely on documents that are already open. Without a browser there is
// nothing to wait for.
func (h *Hook) waitInstalled(ctx context.Context) {
	if h.upstream.Current() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, installWait)
	defer cancel()
	tick := time.NewTicker(installPollGap)
	defer tick.Stop()
	for !h.installed() {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (h *Hook) installed() bool {
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	if conn == nil {
		return false
	}
	ids, ok := conn.surface.CaptureSessions()
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		if s := h.sessions[id]; s == nil || !s.attempted {
			return false
		}
	}
	return true
}
