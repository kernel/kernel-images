// Package webauthnhook answers pages' navigator.credentials.get/create
// publicKey requests with passkeys held outside the VM.
//
// A script installed in every page and iframe document replaces the WebAuthn
// entry points and sends each request through a CDP binding on the
// cdpmonitor's persistent connection. The hook adds the calling document's
// origin as Chromium reports it, forwards the request to a Signer
// (metro-api), and replies in the same execution context. Requests nothing
// answers fall back to the browser's own implementation.
package webauthnhook

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpmonitor"
)

// bindingName must match BINDING and replyFunction must match REPLY in hook.js.
const (
	bindingName   = "__kernelPasskey"
	replyFunction = "__kernelPasskeyReply"
)

const (
	setupTimeout     = 10 * time.Second
	deliverTimeout   = 5 * time.Second
	maxCallBytes     = 256 << 10
	maxRequestIDLen  = 64
	maxSessionCalls  = 16
	answeredRetained = 30 * time.Second
	maxFrameDepth    = 32
)

//go:embed hook.js
var hookSource string

type Config struct {
	// StatePath keeps the enabled flag across restarts of this process.
	StatePath string
	Signer    Signer
	Logger    *slog.Logger
}

type executionContext struct {
	uniqueID string
	origin   string
	frameID  string
}

type callKey struct {
	contextID string // unique execution context ID
	requestID string
}

// call is one page request. reply is empty until the request is answered;
// the page resends until it sees that reply.
type call struct {
	reply      json.RawMessage
	answeredAt time.Time
}

type sender interface {
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
}

type session struct {
	id            string
	targetID      string
	targetType    string
	parentFrameID string
	cdp           sender

	setupMu  sync.Mutex
	domains  bool   // setupMu: Runtime, Page, and the binding are enabled
	scriptID string // setupMu: new-document registration while enabled

	contexts map[int]executionContext // Hook.mu; main-world contexts only
	calls    map[callKey]*call        // Hook.mu
}

// Hook implements cdpmonitor.Extension.
type Hook struct {
	cfg    Config
	retain func()
	log    *slog.Logger

	setMu sync.Mutex // serializes Set

	mu       sync.Mutex
	enabled  bool
	active   bool // sessions get the binding; stays true once enabled
	sessions map[string]*session
}

var _ cdpmonitor.Extension = (*Hook)(nil)

// New restores the flag a previous run of the process left. retainPageDomains
// is called before the hook first enables domains, so telemetry cleanup stops
// disabling them.
func New(cfg Config, retainPageDomains func()) *Hook {
	h := &Hook{cfg: cfg, retain: retainPageDomains, log: cfg.Logger, sessions: make(map[string]*session)}
	if loadEnabled(cfg.StatePath, cfg.Logger) {
		h.retain()
		h.enabled, h.active = true, true
	}
	return h
}

// Set turns the hook on or off, persisting the flag first, and waits until
// the current sessions are reconciled or ctx ends. Pages hooked while it was
// on keep their hook while it is off; their requests are left to the browser.
func (h *Hook) Set(ctx context.Context, enabled bool) error {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	if err := saveEnabled(h.cfg.StatePath, enabled); err != nil {
		return err
	}
	h.mu.Lock()
	activate := enabled && !h.active
	h.mu.Unlock()
	if activate {
		h.retain()
	}
	h.mu.Lock()
	h.enabled = enabled
	h.active = h.active || enabled
	sessions := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(func() { h.reconcile(s) })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

func (h *Hook) SessionAttached(s cdpmonitor.Session) {
	if s.TargetType != "page" && s.TargetType != "iframe" {
		return
	}
	go h.reconcile(h.track(&session{
		id: s.ID, targetID: s.TargetID, targetType: s.TargetType, parentFrameID: s.ParentFrameID, cdp: s,
	}))
}

func (h *Hook) track(s *session) *session {
	s.contexts = make(map[int]executionContext)
	s.calls = make(map[callKey]*call)
	h.mu.Lock()
	h.sessions[s.id] = s
	h.mu.Unlock()
	return s
}

func (h *Hook) SessionRemoved(sessionID string) {
	h.mu.Lock()
	delete(h.sessions, sessionID)
	h.mu.Unlock()
}

func (h *Hook) Reset() {
	h.mu.Lock()
	clear(h.sessions)
	h.mu.Unlock()
}

// reconcile brings one session to the current flag. Domains and the binding
// stay once added so already-hooked documents keep an answer.
func (h *Hook) reconcile(s *session) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	h.mu.Lock()
	active, enabled, live := h.active, h.enabled, h.sessions[s.id] == s
	h.mu.Unlock()
	if !active || !live {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := h.reconcileSession(ctx, s, enabled); err != nil {
		h.mu.Lock()
		live = h.sessions[s.id] == s
		h.mu.Unlock()
		if live {
			h.log.Warn("webauthn: failed to set up hook", "session", s.id, "target_type", s.targetType, "err", err)
		}
	}
}

func (h *Hook) reconcileSession(ctx context.Context, s *session, enabled bool) error {
	if !s.domains {
		for _, command := range []struct {
			method string
			params any
		}{
			{"Runtime.enable", nil},
			{"Page.enable", nil},
			{"Runtime.addBinding", map[string]any{"name": bindingName}},
		} {
			if _, err := s.cdp.Send(ctx, command.method, command.params); err != nil {
				return err
			}
		}
		s.domains = true
	}
	switch {
	case enabled && s.scriptID == "":
		raw, err := s.cdp.Send(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
			"source": hookSource, "runImmediately": true,
		})
		if err != nil {
			return err
		}
		var result struct {
			Identifier string `json:"identifier"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		s.scriptID = result.Identifier
	case !enabled && s.scriptID != "":
		if _, err := s.cdp.Send(ctx, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": s.scriptID}); err != nil {
			return err
		}
		s.scriptID = ""
	}
	return nil
}

func (h *Hook) ProtocolEvent(sessionID, method string, params json.RawMessage) {
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
		h.mu.Lock()
		if s := h.sessions[sessionID]; s != nil {
			s.contexts[event.Context.ID] = executionContext{
				uniqueID: event.Context.UniqueID, origin: event.Context.Origin, frameID: event.Context.AuxData.FrameID,
			}
		}
		h.mu.Unlock()
	case "Runtime.executionContextDestroyed":
		var event struct {
			ID int `json:"executionContextId"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		h.mu.Lock()
		if s := h.sessions[sessionID]; s != nil {
			if destroyed, ok := s.contexts[event.ID]; ok {
				delete(s.contexts, event.ID)
				for key := range s.calls {
					if key.contextID == destroyed.uniqueID {
						delete(s.calls, key)
					}
				}
			}
		}
		h.mu.Unlock()
	case "Runtime.executionContextsCleared":
		h.mu.Lock()
		if s := h.sessions[sessionID]; s != nil {
			clear(s.contexts)
			clear(s.calls)
		}
		h.mu.Unlock()
	case "Runtime.bindingCalled":
		var event struct {
			Name      string `json:"name"`
			Payload   string `json:"payload"`
			ContextID int    `json:"executionContextId"`
		}
		if json.Unmarshal(params, &event) == nil && event.Name == bindingName && len(event.Payload) <= maxCallBytes {
			h.handleCall(sessionID, event.ContextID, event.Payload)
		}
	}
}

type pageCall struct {
	ID        string          `json:"id"`
	Op        string          `json:"op"`
	Mediation string          `json:"mediation"`
	PublicKey json.RawMessage `json:"publicKey"`
}

type pageReply struct {
	ID string `json:"id"`
	Answer
}

func (h *Hook) handleCall(sessionID string, contextID int, payload string) {
	var pc pageCall
	if json.Unmarshal([]byte(payload), &pc) != nil || pc.ID == "" || len(pc.ID) > maxRequestIDLen ||
		(pc.Op != "get" && pc.Op != "create") {
		return
	}
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return
	}
	ec, ok := s.contexts[contextID]
	if !ok {
		h.mu.Unlock()
		return
	}
	key := callKey{contextID: ec.uniqueID, requestID: pc.ID}
	if existing := s.calls[key]; existing != nil {
		reply := existing.reply
		h.mu.Unlock()
		if reply == nil {
			reply = ack(pc.ID)
		}
		go h.deliver(s, ec, reply)
		return
	}
	for k, c := range s.calls {
		if c.reply != nil && time.Since(c.answeredAt) > answeredRetained {
			delete(s.calls, k)
		}
	}
	if len(s.calls) >= maxSessionCalls {
		h.mu.Unlock()
		go h.deliver(s, ec, encodeReply(pc.ID, notHandled))
		return
	}
	c := &call{}
	s.calls[key] = c
	enabled := h.enabled
	topOrigin, topKnown := h.topOriginLocked(s)
	h.mu.Unlock()

	go func() {
		h.deliver(s, ec, ack(pc.ID))
		answer := notHandled
		origin, secure := secureOrigin(ec.origin)
		top, topSecure := secureOrigin(topOrigin)
		if enabled && secure && topKnown && topSecure && h.permitted(s, ec.frameID, pc.Op) {
			answer = h.cfg.Signer.Answer(context.Background(), pc.Op, Request{
				Origin: origin, TopOrigin: top, Mediation: pc.Mediation, PublicKey: pc.PublicKey,
			})
		}
		h.log.Info("webauthn: answered page request", "op", pc.Op, "origin", ec.origin, "top_origin", topOrigin, "result", answer.Result)
		reply := encodeReply(pc.ID, answer)
		h.mu.Lock()
		c.reply, c.answeredAt = reply, time.Now()
		h.mu.Unlock()
		h.deliver(s, ec, reply)
	}()
}

// topOriginLocked follows out-of-process iframes up to their page. Every
// frame in a session shares the session root's top-level document.
func (h *Hook) topOriginLocked(s *session) (string, bool) {
	for range maxFrameDepth {
		if s.targetType == "page" {
			for _, ec := range s.contexts {
				if ec.frameID == s.targetID {
					return ec.origin, true
				}
			}
			return "", false
		}
		if s = h.frameOwnerLocked(s.parentFrameID); s == nil {
			return "", false
		}
	}
	return "", false
}

func (h *Hook) frameOwnerLocked(frameID string) *session {
	if frameID == "" {
		return nil
	}
	for _, s := range h.sessions {
		for _, ec := range s.contexts {
			if ec.frameID == frameID {
				return s
			}
		}
	}
	return nil
}

// permitted applies the document's permissions policy, which governs the
// WebAuthn API in cross-origin frames. A refused request falls back to the
// browser, which rejects it the way it always would.
func (h *Hook) permitted(s *session, frameID, op string) bool {
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
// localhost. Opaque origins do not parse to a host.
func secureOrigin(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	switch {
	case u.Scheme == "https", u.Scheme == "http" && u.Hostname() == "localhost":
		return origin, true
	}
	return "", false
}

func (h *Hook) deliver(s *session, ec executionContext, reply json.RawMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	if _, err := s.cdp.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression":      replyFunction + "(" + string(reply) + ")",
		"uniqueContextId": ec.uniqueID,
		"silent":          true,
	}); err != nil {
		h.log.Debug("webauthn: failed to deliver reply", "session", s.id, "err", err)
	}
}

func ack(id string) json.RawMessage {
	data, _ := json.Marshal(struct {
		ID  string `json:"id"`
		Ack bool   `json:"ack"`
	}{id, true})
	return data
}

func encodeReply(id string, answer Answer) json.RawMessage {
	data, _ := json.Marshal(pageReply{ID: id, Answer: answer})
	return data
}
