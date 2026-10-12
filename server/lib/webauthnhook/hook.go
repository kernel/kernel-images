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
	"strings"
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
	setupRetryDelay  = time.Second
	deliverTimeout   = 5 * time.Second
	maxCallBytes     = 256 << 10
	maxRequestIDLen  = 64
	maxPendingCalls  = 16  // unanswered calls per session
	maxTrackedCalls  = 256 // answered calls kept per session for resends
	answeredRetained = 30 * time.Second
	maxFrameDepth    = 32
)

// suspendExpression tells a hooked document to leave every request to the
// browser until the hook script runs in it again.
const suspendExpression = "typeof " + replyFunction + ` === "function" && ` + replyFunction + `({"suspend":true})`

//go:embed hook.js
var hookSource string

type Config struct {
	// StatePath keeps the enabled flag across restarts of this process.
	StatePath string
	Signer    Signer
	Logger    *slog.Logger
}

// PageDomains keeps telemetry cleanup from disabling the Page and Runtime
// domains the hook needs while it is on. *cdpmonitor.Monitor implements it.
type PageDomains interface {
	RetainPageDomains()
	ReleasePageDomains(ctx context.Context) error
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
// the page resends until it sees that reply. cancel ends the request when the
// page aborts it.
type call struct {
	reply      json.RawMessage
	answeredAt time.Time
	cancel     context.CancelFunc
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
	domains  bool   // setupMu: the binding may be installed
	scriptID string // setupMu: new-document registration while enabled

	contexts      map[int]executionContext // Hook.mu; main-world contexts only
	frameContexts map[string]int           // Hook.mu; frame ID -> its newest main-world context
	calls         map[callKey]*call        // Hook.mu
}

// Hook implements cdpmonitor.Extension.
type Hook struct {
	cfg     Config
	domains PageDomains
	log     *slog.Logger

	setMu sync.Mutex // serializes Set

	mu       sync.Mutex
	enabled  bool
	sessions map[string]*session
}

var _ cdpmonitor.Extension = (*Hook)(nil)

// New restores the flag a previous run of the process left. While the hook
// is on it retains domains, so telemetry cleanup does not disable them.
func New(cfg Config, domains PageDomains) *Hook {
	h := &Hook{cfg: cfg, domains: domains, log: cfg.Logger, sessions: make(map[string]*session)}
	if loadEnabled(cfg.StatePath, cfg.Logger) {
		h.domains.RetainPageDomains()
		h.enabled = true
	}
	return h
}

// Set turns the hook on or off, persisting the flag first, and waits until the
// current sessions are reconciled. Turning it off tells hooked documents to
// leave requests to the browser, removes the script and binding, and releases
// the Page and Runtime domains to telemetry.
func (h *Hook) Set(ctx context.Context, enabled bool) error {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	if err := saveEnabled(h.cfg.StatePath, enabled); err != nil {
		return err
	}
	h.mu.Lock()
	changed := h.enabled != enabled
	h.mu.Unlock()
	if changed && enabled {
		h.domains.RetainPageDomains()
	}
	h.mu.Lock()
	h.enabled = enabled
	sessions := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(func() { h.reconcile(s) })
	}
	wg.Wait()
	if changed && !enabled {
		if err := h.domains.ReleasePageDomains(ctx); err != nil {
			h.log.Warn("webauthn: failed to release page domains", "err", err)
		}
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
	s.frameContexts = make(map[string]int)
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

// reconcile brings one session to the current flag, retrying once after a
// short delay so a tab is not left unhooked by a transient failure.
func (h *Hook) reconcile(s *session) {
	err := h.reconcileOnce(s)
	if err == nil {
		return
	}
	time.Sleep(setupRetryDelay)
	if err = h.reconcileOnce(s); err != nil && h.live(s) {
		h.log.Error("webauthn: failed to reconcile hook", "session", s.id, "target_type", s.targetType, "err", err)
	}
}

func (h *Hook) live(s *session) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[s.id] == s
}

func (h *Hook) reconcileOnce(s *session) error {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	h.mu.Lock()
	enabled, live := h.enabled, h.sessions[s.id] == s
	contexts := make([]executionContext, 0, len(s.contexts))
	for _, ec := range s.contexts {
		contexts = append(contexts, ec)
	}
	h.mu.Unlock()
	if !live {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if enabled {
		return h.install(ctx, s)
	}
	return h.uninstall(ctx, s, contexts)
}

func (h *Hook) install(ctx context.Context, s *session) error {
	if s.scriptID != "" {
		return nil
	}
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
	return nil
}

// uninstall suspends the hook in the session's documents before removing the
// binding, so they fall back at once rather than after a timeout. The domains
// themselves are released together, once every session is done.
func (h *Hook) uninstall(ctx context.Context, s *session, contexts []executionContext) error {
	if !s.domains {
		return nil
	}
	for _, ec := range contexts {
		_, _ = s.cdp.Send(ctx, "Runtime.evaluate", map[string]any{
			"expression": suspendExpression, "uniqueContextId": ec.uniqueID, "silent": true,
		})
	}
	if s.scriptID != "" {
		if _, err := s.cdp.Send(ctx, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": s.scriptID}); err != nil {
			return err
		}
		s.scriptID = ""
	}
	if _, err := s.cdp.Send(ctx, "Runtime.removeBinding", map[string]any{"name": bindingName}); err != nil {
		return err
	}
	s.domains = false
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
			s.frameContexts[event.Context.AuxData.FrameID] = event.Context.ID
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
				if s.frameContexts[destroyed.frameID] == event.ID {
					delete(s.frameContexts, destroyed.frameID)
				}
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
			clear(s.frameContexts)
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
		(pc.Op != "get" && pc.Op != "create" && pc.Op != "abort") {
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
	existing := s.calls[key]
	if pc.Op == "abort" {
		// A request the page discarded is not sent to metro-api if it has not
		// been already, and stops retrying if it has.
		if existing != nil && existing.reply == nil {
			existing.cancel()
			delete(s.calls, key)
		}
		h.mu.Unlock()
		return
	}
	if existing != nil {
		reply := existing.reply
		h.mu.Unlock()
		if reply == nil {
			reply = ack(pc.ID)
		}
		go h.deliver(s, ec, reply)
		return
	}
	if !h.admitLocked(s) {
		h.mu.Unlock()
		h.log.Warn("webauthn: too many unanswered requests; leaving the request to the browser", "session", sessionID, "limit", maxPendingCalls)
		go h.deliver(s, ec, encodeReply(pc.ID, notHandled))
		return
	}
	callCtx, cancel := context.WithCancel(context.Background())
	c := &call{cancel: cancel}
	s.calls[key] = c
	enabled := h.enabled
	topOrigin, topKnown := h.topOriginLocked(s)
	h.mu.Unlock()

	go func() {
		defer cancel()
		h.deliver(s, ec, ack(pc.ID))
		answer := notHandled
		origin, secure := secureOrigin(ec.origin)
		top, topSecure := secureOrigin(topOrigin)
		if enabled && secure && topKnown && topSecure && h.permitted(s, ec.frameID, pc.Op) && callCtx.Err() == nil {
			answer = h.cfg.Signer.Answer(callCtx, pc.Op, Request{
				Origin: origin, TopOrigin: top, Mediation: pc.Mediation, PublicKey: pc.PublicKey,
			})
		}
		if callCtx.Err() != nil {
			h.log.Debug("webauthn: page aborted request", "op", pc.Op, "origin", ec.origin)
			return
		}
		h.log.Debug("webauthn: answered page request", "op", pc.Op, "origin", ec.origin, "top_origin", topOrigin, "result", answer.Result)
		reply := encodeReply(pc.ID, answer)
		h.mu.Lock()
		c.reply, c.answeredAt = reply, time.Now()
		h.mu.Unlock()
		h.deliver(s, ec, reply)
	}()
}

// admitLocked reports whether the session may start another request. Only
// unanswered requests count; answered ones are kept for resends until they
// expire, or until the oldest is evicted to bound memory.
func (h *Hook) admitLocked(s *session) bool {
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

// topOriginLocked follows out-of-process iframes up to their page. Every
// frame in a session shares the session root's top-level document.
func (h *Hook) topOriginLocked(s *session) (string, bool) {
	for range maxFrameDepth {
		if s.targetType == "page" {
			id, ok := s.frameContexts[s.targetID]
			return s.contexts[id].origin, ok
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
		if _, ok := s.frameContexts[frameID]; ok {
			return s
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
