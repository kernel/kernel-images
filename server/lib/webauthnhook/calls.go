package webauthnhook

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

const (
	deliverTimeout   = 5 * time.Second
	maxCallBytes     = 256 << 10
	maxRequestIDLen  = 64
	maxPendingCalls  = 16  // unanswered calls per session
	maxTrackedCalls  = 256 // answered calls kept per session for resends
	answeredRetained = 30 * time.Second
	// topOriginWait bounds how long a request waits for the tracker to place
	// its frame in a page; an iframe can call at load before that.
	topOriginWait = 2 * time.Second
)

type callKey struct {
	contextID string // unique execution context ID
	requestID string
}

// call is one page request. reply is nil until the request is answered; the
// page resends until it sees that reply. cancel ends the request when the
// page aborts it or its document goes away.
type call struct {
	reply      *pageMessage
	answeredAt time.Time
	cancel     context.CancelFunc
}

// pageCall is what the page sends through BINDING.
type pageCall struct {
	ID        string          `json:"id"`
	Op        string          `json:"op"`
	Mediation string          `json:"mediation"`
	PublicKey json.RawMessage `json:"publicKey"`
}

// pageMessage is what kernel-images sends the page through REPLY: an
// acknowledgement, a final answer, or a suspension of the whole document.
type pageMessage struct {
	ID      string `json:"id,omitempty"`
	Ack     bool   `json:"ack,omitempty"`
	Suspend bool   `json:"suspend,omitempty"`
	*Answer
}

func (c *connection) handleCall(sessionID string, contextID int, payload string) {
	var pc pageCall
	if json.Unmarshal([]byte(payload), &pc) != nil || pc.ID == "" || len(pc.ID) > maxRequestIDLen ||
		(pc.Op != "get" && pc.Op != "create" && pc.Op != "abort") {
		return
	}
	c.mu.Lock()
	s := c.sessions[sessionID]
	if s == nil {
		c.mu.Unlock()
		return
	}
	ec, ok := s.contexts[contextID]
	if !ok {
		c.mu.Unlock()
		return
	}
	key := callKey{contextID: ec.uniqueID, requestID: pc.ID}
	if pc.Op == "abort" {
		// A request the page discarded is not sent to metro-api if it has not
		// been already, and stops retrying if it has.
		s.dropCalls(func(k callKey) bool { return k == key })
		c.mu.Unlock()
		return
	}
	if existing := s.calls[key]; existing != nil {
		reply := pageMessage{ID: pc.ID, Ack: true}
		if existing.reply != nil {
			reply = *existing.reply
		}
		c.mu.Unlock()
		go c.deliver(s, ec, reply)
		return
	}
	if !admitLocked(s) {
		c.mu.Unlock()
		c.hook.log.Warn("webauthn: too many unanswered requests; leaving the request to the browser", "session", sessionID, "limit", maxPendingCalls)
		answer := notHandled
		go c.deliver(s, ec, pageMessage{ID: pc.ID, Answer: &answer})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending := &call{cancel: cancel}
	s.calls[key] = pending
	c.mu.Unlock()
	go c.dispatch(ctx, s, ec, pc, pending)
}

// dispatch acknowledges a new request, asks the signer when the request is
// eligible, and delivers the answer unless the request was dropped meanwhile.
func (c *connection) dispatch(ctx context.Context, s *session, ec executionContext, pc pageCall, pending *call) {
	defer pending.cancel()
	c.deliver(s, ec, pageMessage{ID: pc.ID, Ack: true})
	topOrigin := c.topOrigin(ctx, s.id)
	answer := notHandled
	if req, ok := c.eligible(s, ec, pc, topOrigin); ok && ctx.Err() == nil {
		answer = c.hook.cfg.Signer.Answer(ctx, pc.Op, req)
	}
	if ctx.Err() != nil {
		c.hook.log.Debug("webauthn: page request dropped", "op", pc.Op, "origin", ec.origin)
		return
	}
	c.hook.log.Debug("webauthn: answered page request", "op", pc.Op, "origin", ec.origin, "top_origin", topOrigin, "result", answer.Result)
	reply := pageMessage{ID: pc.ID, Answer: &answer}
	c.mu.Lock()
	pending.reply, pending.answeredAt = &reply, time.Now()
	c.mu.Unlock()
	c.deliver(s, ec, reply)
}

// topOrigin returns the origin of the main document of the page containing
// the session, waiting up to topOriginWait for it to become known. It returns
// "" if it does not, which leaves the request to the browser.
func (c *connection) topOrigin(ctx context.Context, sessionID string) string {
	ctx, cancel := context.WithTimeout(ctx, topOriginWait)
	defer cancel()
	tick := time.NewTicker(installPollGap)
	defer tick.Stop()
	for {
		if origin, ok := c.lookupTopOrigin(sessionID); ok {
			return origin
		}
		select {
		case <-ctx.Done():
			return ""
		case <-tick.C:
		}
	}
}

func (c *connection) lookupTopOrigin(sessionID string) (string, bool) {
	pageSessionID, mainFrameID, ok := c.pageSession(sessionID)
	if !ok {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	page := c.sessions[pageSessionID]
	if page == nil {
		return "", false
	}
	id, ok := page.frameContexts[mainFrameID]
	return page.contexts[id].origin, ok
}

// eligible builds the signer request when the hook is on, both origins are
// ones WebAuthn serves, and the document's permissions policy allows the call.
func (c *connection) eligible(s *session, ec executionContext, pc pageCall, topOrigin string) (Request, bool) {
	origin, secure := secureOrigin(ec.origin)
	top, topSecure := secureOrigin(topOrigin)
	if !c.hook.enabled.Load() || !secure || !topSecure || !c.permitted(s, ec.frameID, pc.Op) {
		return Request{}, false
	}
	return Request{Origin: origin, TopOrigin: top, Mediation: pc.Mediation, PublicKey: pc.PublicKey}, true
}

// admitLocked reports whether the session may start another request. Only
// unanswered requests count; answered ones are kept for resends until they
// expire, or until the oldest is evicted to bound memory.
func admitLocked(s *session) bool {
	pending := 0
	var oldest callKey
	var oldestAt time.Time
	for key, c := range s.calls {
		switch {
		case c.reply == nil:
			pending++
		case time.Since(c.answeredAt) > answeredRetained:
			delete(s.calls, key)
		case oldestAt.IsZero() || c.answeredAt.Before(oldestAt):
			oldest, oldestAt = key, c.answeredAt
		}
	}
	if pending >= maxPendingCalls {
		return false
	}
	if len(s.calls) >= maxTrackedCalls {
		delete(s.calls, oldest)
	}
	return true
}

// permitted applies the document's permissions policy, which governs the
// WebAuthn API in cross-origin frames. A refused request falls back to the
// browser, which rejects it the way it always would.
func (c *connection) permitted(s *session, frameID, op string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	raw, err := s.cdp.Send(ctx, "Page.getPermissionsPolicyState", map[string]any{"frameId": frameID})
	if err != nil {
		return false
	}
	var result struct {
		States []struct {
			Feature string `json:"feature"`
			Allowed bool   `json:"allowed"`
		} `json:"states"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	feature := "publickey-credentials-" + op
	for _, state := range result.States {
		if state.Feature == feature {
			return state.Allowed
		}
	}
	return false
}

// secureOrigin accepts the origins WebAuthn serves: https, and http on
// localhost and its subdomains. Opaque origins do not parse to a host.
func secureOrigin(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	switch {
	case u.Scheme == "https", u.Scheme == "http" && (u.Hostname() == "localhost" || strings.HasSuffix(u.Hostname(), ".localhost")):
		return origin, true
	}
	return "", false
}

// deliver calls REPLY in one execution context. Documents without the hook
// throw, which is harmless.
func (c *connection) deliver(s *session, ec executionContext, msg pageMessage) {
	data, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	if _, err := s.cdp.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression":      replyFunction + "(" + string(data) + ")",
		"uniqueContextId": ec.uniqueID,
		"silent":          true,
	}); err != nil {
		c.hook.log.Debug("webauthn: failed to deliver reply", "session", s.id, "err", err)
	}
}
