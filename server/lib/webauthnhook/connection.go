package webauthnhook

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

const (
	dialTimeout     = 5 * time.Second
	probeInterval   = 5 * time.Second
	probeTimeout    = 5 * time.Second
	reconnectMin    = 250 * time.Millisecond
	reconnectMax    = 5 * time.Second
	setupTimeout    = 10 * time.Second
	setupRetryDelay = time.Second
	installWait     = 15 * time.Second
	installPollGap  = 50 * time.Millisecond
)

// connection is one DevTools connection and the page and iframe targets it
// has attached. Its sessions, bindings, and scripts end with it.
type connection struct {
	hook     *Hook
	protocol *cdpclient.Client
	surface  *browsersurface.Tracker
	done     chan struct{} // closed once its events are consumed
	// pageSession resolves a session to the page containing it; the tracker's
	// PageSession.
	pageSession func(sessionID string) (pageSessionID, mainFrameID string, ok bool)

	mu       sync.Mutex
	sessions map[string]*session
}

type sender interface {
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// cdpSession sends commands to one target over the hook's connection.
type cdpSession struct {
	protocol *cdpclient.Client
	id       string
}

func (c cdpSession) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.protocol.Send(ctx, method, params, c.id)
}

type executionContext struct {
	uniqueID string
	origin   string
	frameID  string
}

type session struct {
	id         string
	targetType string
	cdp        sender

	attempted     bool                     // connection.mu; first setup attempt has finished
	contexts      map[int]executionContext // connection.mu; main-world contexts only
	frameContexts map[string]int           // connection.mu; frame ID -> its newest main-world context
	calls         map[callKey]*call        // connection.mu
}

// dropCalls forgets the calls match selects, cancelling any still in flight
// so nothing keeps asking metro-api for a document that is gone.
func (s *session) dropCalls(match func(callKey) bool) {
	for key, c := range s.calls {
		if match(key) {
			c.cancel()
			delete(s.calls, key)
		}
	}
}

func allCalls(callKey) bool { return true }

// run keeps a connection open until ctx ends, replacing it after socket loss,
// a failed probe, a failed target attach, or a new upstream URL (Chromium
// restarted).
func (h *Hook) run(ctx context.Context) {
	updates, unsubscribe := h.upstream.Subscribe()
	defer unsubscribe()
	backoff := reconnectMin
	for {
		if url := h.upstream.Current(); url != "" {
			conn, err := h.connect(ctx, url)
			if err == nil {
				connected := time.Now()
				h.serve(ctx, conn, updates)
				h.disconnect(conn)
				// A connection that fails straight away backs off like a failed dial.
				if time.Since(connected) >= probeInterval {
					backoff = reconnectMin
				}
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
	surface := browsersurface.New(protocol)
	conn := h.newConnection(surface.PageSession)
	conn.protocol, conn.surface, conn.done = protocol, surface, make(chan struct{})
	events, unsubscribe := surface.Subscribe()
	go func() {
		defer close(conn.done)
		defer unsubscribe()
		for event := range events {
			conn.handleEvent(event)
		}
	}()
	startCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	if err := surface.Start(startCtx); err != nil {
		h.disconnect(conn)
		return nil, err
	}
	h.mu.Lock()
	h.conn = conn
	h.mu.Unlock()
	return conn, nil
}

func (h *Hook) newConnection(pageSession func(string) (string, string, bool)) *connection {
	return &connection{hook: h, pageSession: pageSession, sessions: make(map[string]*session)}
}

func (h *Hook) serve(ctx context.Context, conn *connection, updates <-chan string) {
	probe := time.NewTicker(probeInterval)
	defer probe.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.protocol.Done():
			h.log.Warn("webauthn: DevTools connection closed; reconnecting")
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

// disconnect closes conn and cancels its unanswered requests; their documents
// resend to the next connection.
func (h *Hook) disconnect(conn *connection) {
	_ = conn.protocol.Close()
	<-conn.done
	h.mu.Lock()
	if h.conn == conn {
		h.conn = nil
	}
	h.mu.Unlock()
	conn.mu.Lock()
	defer conn.mu.Unlock()
	for _, s := range conn.sessions {
		s.dropCalls(allCalls)
	}
}

func (c *connection) handleEvent(event browsersurface.Event) {
	switch event.Kind {
	case browsersurface.EventSessionAttached:
		if event.Target.Type != "page" && event.Target.Type != "iframe" {
			return
		}
		go c.reconcile(c.track(&session{
			id: event.SessionID, targetType: event.Target.Type,
			cdp: cdpSession{protocol: c.protocol, id: event.SessionID},
		}))
	case browsersurface.EventSessionRemoved:
		c.mu.Lock()
		if s := c.sessions[event.SessionID]; s != nil {
			s.dropCalls(allCalls)
			delete(c.sessions, event.SessionID)
		}
		c.mu.Unlock()
	case browsersurface.EventDiscoveryFailed:
		// The target stays unhooked on this connection; the next attaches it.
		c.hook.log.Warn("webauthn: failed to attach a browser target; reconnecting")
		_ = c.protocol.Close()
	case browsersurface.EventProtocol:
		if event.Message.SessionID != "" {
			c.protocolEvent(event.Message.SessionID, event.Message.Method, event.Message.Params)
		}
	}
}

func (c *connection) track(s *session) *session {
	s.contexts = make(map[int]executionContext)
	s.frameContexts = make(map[string]int)
	s.calls = make(map[callKey]*call)
	c.mu.Lock()
	c.sessions[s.id] = s
	c.mu.Unlock()
	return s
}

func (c *connection) live(s *session) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[s.id] == s
}

// reconcile hooks a newly attached session, retrying once after a short
// delay so a tab is not left unhooked by a transient failure.
func (c *connection) reconcile(s *session) {
	err := c.install(s)
	if err != nil {
		time.Sleep(setupRetryDelay)
		if err = c.install(s); err != nil && c.live(s) {
			c.hook.log.Error("webauthn: failed to install hook", "session", s.id, "target_type", s.targetType, "err", err)
		}
	}
	c.mu.Lock()
	s.attempted = true
	c.mu.Unlock()
}

func (c *connection) install(s *session) error {
	if !c.hook.enabled.Load() || !c.live(s) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	for _, command := range []struct {
		method string
		params any
	}{
		{"Runtime.enable", nil},
		{"Page.enable", nil},
		{"Runtime.addBinding", map[string]any{"name": bindingName}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": hookSource, "runImmediately": true}},
	} {
		if _, err := s.cdp.Send(ctx, command.method, command.params); err != nil {
			return err
		}
	}
	return nil
}

func (c *connection) protocolEvent(sessionID, method string, params json.RawMessage) {
	switch method {
	case "Runtime.executionContextCreated":
		var event struct {
			Context struct {
				ID       int    `json:"id"`
				UniqueID string `json:"uniqueId"`
				Origin   string `json:"origin"`
				AuxData  struct {
					FrameID   string `json:"frameId"`
					IsDefault bool   `json:"isDefault"`
				} `json:"auxData"`
			} `json:"context"`
		}
		if json.Unmarshal(params, &event) != nil || !event.Context.AuxData.IsDefault {
			return
		}
		c.mu.Lock()
		if s := c.sessions[sessionID]; s != nil {
			s.contexts[event.Context.ID] = executionContext{
				uniqueID: event.Context.UniqueID, origin: event.Context.Origin, frameID: event.Context.AuxData.FrameID,
			}
			s.frameContexts[event.Context.AuxData.FrameID] = event.Context.ID
		}
		c.mu.Unlock()
	case "Runtime.executionContextDestroyed":
		var event struct {
			ID int `json:"executionContextId"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		c.mu.Lock()
		if s := c.sessions[sessionID]; s != nil {
			if destroyed, ok := s.contexts[event.ID]; ok {
				delete(s.contexts, event.ID)
				if s.frameContexts[destroyed.frameID] == event.ID {
					delete(s.frameContexts, destroyed.frameID)
				}
				s.dropCalls(func(key callKey) bool { return key.contextID == destroyed.uniqueID })
			}
		}
		c.mu.Unlock()
	case "Runtime.executionContextsCleared":
		c.mu.Lock()
		if s := c.sessions[sessionID]; s != nil {
			clear(s.contexts)
			clear(s.frameContexts)
			s.dropCalls(allCalls)
		}
		c.mu.Unlock()
	case "Runtime.bindingCalled":
		var event struct {
			Name      string `json:"name"`
			Payload   string `json:"payload"`
			ContextID int    `json:"executionContextId"`
		}
		if json.Unmarshal(params, &event) == nil && event.Name == bindingName && len(event.Payload) <= maxCallBytes {
			c.handleCall(sessionID, event.ContextID, event.Payload)
		}
	}
}

// suspendAll makes every hooked document fall back at once, rather than after
// the page's timeout once the connection is gone.
func (c *connection) suspendAll() {
	type target struct {
		s  *session
		ec executionContext
	}
	var targets []target
	c.mu.Lock()
	for _, s := range c.sessions {
		for _, ec := range s.contexts {
			targets = append(targets, target{s, ec})
		}
	}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Go(func() { c.deliver(t.s, t.ec, pageMessage{Suspend: true}) })
	}
	wg.Wait()
}

// waitInstalled waits until every page and iframe open when the connection
// started has had its first setup attempt, so a caller that turns the hook on
// can rely on documents that are already open. Without a browser there is
// nothing to wait for.
func (h *Hook) waitInstalled(ctx context.Context) error {
	if h.upstream.Current() == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, installWait)
	defer cancel()
	tick := time.NewTicker(installPollGap)
	defer tick.Stop()
	for !h.installed() {
		select {
		case <-ctx.Done():
			return ErrNotInstalled
		case <-tick.C:
		}
	}
	return nil
}

func (h *Hook) installed() bool {
	conn := h.current()
	if conn == nil {
		return false
	}
	ids, ok := conn.surface.CaptureSessions()
	if !ok {
		return false
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	for _, id := range ids {
		if s := conn.sessions[id]; s == nil || !s.attempted {
			return false
		}
	}
	return true
}
